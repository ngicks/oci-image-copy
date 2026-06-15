package ociimagecopy

// commit_order_test.go asserts the commit-last ordering for push and pull:
// tag-file writes (the "commit" step) must happen AFTER all blob transfers.
//
// The tests use a hookDirs wrapper around FsOciDirs that fires a callback on
// PutTagFile, allowing the test to inspect the share directory at that moment
// and confirm all blobs are already present.

import (
	"context"
	"io"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ngicks/go-fsys-helper/fsutil"
	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
	"github.com/opencontainers/go-digest"
)

// ────────────────────────────────────────────────────────────────────────────
// hookDirs — OciDirs that fires a callback on PutTagFile
// ────────────────────────────────────────────────────────────────────────────

// hookDirs wraps a *FsOciDirs and calls hook() on every PutTagFile
// invocation, before the underlying write. All other operations delegate
// to inner without interception.
type hookDirs struct {
	inner *FsOciDirs
	hook  func()
}

var _ OciDirs = (*hookDirs)(nil)

func (h *hookDirs) Blob(
	ctx context.Context, d digest.Digest, offset int64,
) (io.ReadCloser, int64, error) {
	return h.inner.Blob(ctx, d, offset)
}

func (h *hookDirs) Image(ref imageref.ImageRef) ocidir.DirV1 {
	return h.inner.Image(ref)
}

func (h *hookDirs) BlobSource(
	ctx context.Context, dgst digest.Digest, size int64,
) (fsutil.ResumableSource, error) {
	return h.inner.BlobSource(ctx, dgst, size)
}

func (h *hookDirs) BlobSink(
	ctx context.Context, dgst digest.Digest, size int64,
) (fsutil.ResumableSink, error) {
	return h.inner.BlobSink(ctx, dgst, size)
}

func (h *hookDirs) MkdirBlobParent(dgst digest.Digest) error {
	return h.inner.MkdirBlobParent(dgst)
}

func (h *hookDirs) PutTagFile(
	ctx context.Context, ref imageref.ImageRef, name string, data []byte,
) error {
	if h.hook != nil {
		h.hook()
	}
	return h.inner.PutTagFile(ctx, ref, name, data)
}

// ────────────────────────────────────────────────────────────────────────────
// hookFakeRemote — fakeRemote whose Dir() returns a hookDirs
// ────────────────────────────────────────────────────────────────────────────

// hookFakeRemote is a fakeRemote whose Dir() returns a hookDirs.
// All other methods delegate to fakeRemote.
type hookFakeRemote struct {
	*fakeRemote
	hookD *hookDirs
}

var _ Remote = (*hookFakeRemote)(nil)

func (r *hookFakeRemote) Dir() OciDirs { return r.hookD }

// newHookRemote builds a hookFakeRemote: a fakeRemote whose PutTagFile
// fires hook(). The underlying FS is rooted at baseDir.
func newHookRemote(
	baseDir string,
	sk SkopeoLike,
	fsys vroot.Fs[vroot.File],
	hook func(),
) *hookFakeRemote {
	inner := NewFsOciDirs(fsys, 1)
	fr := &fakeRemote{
		baseDir:   baseDir,
		transport: skopeo.TransportOci,
		skopeoCli: sk,
		fs:        fsys,
		dirs:      inner,
		assumeHas: map[string]struct{}{},
	}
	return &hookFakeRemote{
		fakeRemote: fr,
		hookD:      &hookDirs{inner: inner, hook: hook},
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Tests
// ────────────────────────────────────────────────────────────────────────────

// TestPush_TagFilesWrittenAfterBlobs verifies that when PutTagFile is called
// (the commit step) on the remote, all expected blobs are already present in
// the remote share/sha256/ directory. This asserts the commit-last ordering.
func TestPush_TagFilesWrittenAfterBlobs(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(localBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(localBase, "share")
	seedDump(t, tagDir, shareDir)

	localSk := &recordingSkopeo{
		copyTo: func(
			ctx context.Context, src, dst skopeo.TransportRef, sharedBlobDir string,
		) error {
			return nil
		},
	}

	// Snapshot which blobs are present in share/ whenever PutTagFile fires.
	var blobsAtTagFileWrite []string
	hook := func() {
		shaDir := filepath.Join(remoteBase, "share", "sha256")
		entries, err := os.ReadDir(shaDir)
		if err == nil {
			for _, e := range entries {
				blobsAtTagFileWrite = append(blobsAtTagFileWrite, e.Name())
			}
		}
	}

	local := newLocal(localFS, localBase, localSk)
	remote := newHookRemote(remoteBase, &recordingSkopeo{}, remoteFS, hook)

	_, err := local.Push(context.Background(), PushArgs{
		Images: []string{"ghcr.io/a/b:v1"},
	}, remote)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	if len(blobsAtTagFileWrite) == 0 {
		t.Error("PutTagFile was never called (no hook invocation recorded)")
	}

	// Every blob that the push would have sent must already be present at
	// the time PutTagFile fired.
	for _, wantHex := range allRealHexes() {
		if !slices.Contains(blobsAtTagFileWrite, wantHex) {
			t.Errorf(
				"blob %s was NOT present in share/ when PutTagFile fired "+
					"(blobs seen: %v)",
				wantHex, blobsAtTagFileWrite,
			)
		}
	}
}

// TestPull_TagFilesWrittenAfterBlobs verifies the pull-direction commit-last
// ordering: local PutTagFile is called only after all blob data has been
// written into the local share/sha256/ directory.
func TestPull_TagFilesWrittenAfterBlobs(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(remoteBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(remoteBase, "share")
	seedDump(t, tagDir, shareDir)

	// Build a hookDirs for the LOCAL side that fires on PutTagFile.
	localInner := NewFsOciDirs(localFS, 1)
	var blobsAtTagFileWrite []string
	localHookD := &hookDirs{
		inner: localInner,
		hook: func() {
			shaDir := filepath.Join(localBase, "share", "sha256")
			entries, err := os.ReadDir(shaDir)
			if err == nil {
				for _, e := range entries {
					blobsAtTagFileWrite = append(blobsAtTagFileWrite, e.Name())
				}
			}
		},
	}

	// Build a Local and replace its dirs field with the hookDirs.
	// This works because Local.dirs is now typed as OciDirs (interface).
	local := &Local{
		baseDir:   localBase,
		transport: skopeo.TransportContainersStorage,
		skopeoCli: &recordingSkopeo{},
		fs:        localFS,
		dirs:      localHookD, // hookDirs intercepts PutTagFile
	}

	remote := newFakeRemote(
		remoteBase,
		skopeo.TransportContainersStorage,
		&recordingSkopeo{
			copyTo: func(_ context.Context, _, _ skopeo.TransportRef, _ string) error {
				return nil
			},
		},
		remoteFS,
	)

	res, err := local.Pull(context.Background(), PullArgs{
		Images:            []string{"ghcr.io/a/b:v1"},
		AssumeLocalHasSet: map[string]struct{}{},
	}, remote)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("Pull failed: %+v", res.Reports)
	}

	if len(blobsAtTagFileWrite) == 0 {
		t.Error("local PutTagFile was never called (no hook invocation recorded)")
	}
	// Every blob must have already been written locally before PutTagFile fired.
	for _, wantHex := range allRealHexes() {
		if !slices.Contains(blobsAtTagFileWrite, wantHex) {
			t.Errorf(
				"blob %s NOT present locally when PutTagFile fired (blobs seen: %v)",
				wantHex, blobsAtTagFileWrite,
			)
		}
	}
}

// Ensure unused imports don't cause compile errors: iter is used via fakeRemote
// (which is defined in push_test.go and uses iter.Seq2). We import it here to
// satisfy the compiler in isolation; the actual use is in push_test.go.
var _ iter.Seq[int] = nil
