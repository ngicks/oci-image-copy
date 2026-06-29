package ociimagecopy

// commit_order_test.go asserts the commit-last ordering for push and pull:
// tag-file writes (the "commit" step) must happen AFTER all blob transfers.
//
// The tests use a hookDirs wrapper around FsOciDirs that fires a callback on
// PutIndex / PutOciLayout, allowing the test to inspect the share directory at
// that moment and confirm all blobs are already present.

import (
	"context"
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
// hookDirs — StoreV1 that fires a callback on PutIndex / PutOciLayout
// ────────────────────────────────────────────────────────────────────────────

// hookDirs wraps a *FsOciDirs and calls hook() on every PutIndex and
// PutOciLayout invocation, before the underlying write. All other operations
// delegate to inner without interception.
type hookDirs struct {
	inner *FsOciDirs
	hook  func()
}

var _ StoreV1 = (*hookDirs)(nil)

func (h *hookDirs) Stat(
	ctx context.Context, dgst digest.Digest, size int64,
) (BlobInfo, error) {
	return h.inner.Stat(ctx, dgst, size)
}

func (h *hookDirs) PrepDownload(
	ctx context.Context, dgst digest.Digest, size int64,
) (fsutil.ResumableSource, error) {
	return h.inner.PrepDownload(ctx, dgst, size)
}

func (h *hookDirs) PrepUpload(
	ctx context.Context, dgst digest.Digest, size int64,
) (fsutil.ResumableSink, error) {
	return h.inner.PrepUpload(ctx, dgst, size)
}

func (h *hookDirs) GetIndex(ctx context.Context, ref imageref.ImageRef) ([]byte, error) {
	return h.inner.GetIndex(ctx, ref)
}

func (h *hookDirs) GetOciLayout(ctx context.Context, ref imageref.ImageRef) ([]byte, error) {
	return h.inner.GetOciLayout(ctx, ref)
}

func (h *hookDirs) PutIndex(ctx context.Context, ref imageref.ImageRef, raw []byte) error {
	if h.hook != nil {
		h.hook()
	}
	return h.inner.PutIndex(ctx, ref, raw)
}

func (h *hookDirs) PutOciLayout(ctx context.Context, ref imageref.ImageRef, raw []byte) error {
	if h.hook != nil {
		h.hook()
	}
	return h.inner.PutOciLayout(ctx, ref, raw)
}

// ────────────────────────────────────────────────────────────────────────────
// hookFakeRemote — fakeRemote whose Blobs()/Tags() return a hookDirs
// ────────────────────────────────────────────────────────────────────────────

// hookFakeRemote is a fakeRemote whose Blobs()/Tags() return a hookDirs.
// All other methods delegate to fakeRemote.
type hookFakeRemote struct {
	*fakeRemote
	hookD *hookDirs
}

var _ Remote = (*hookFakeRemote)(nil)

func (r *hookFakeRemote) Blobs() BlobStore { return r.hookD }
func (r *hookFakeRemote) Tags() TagStoreV1 { return r.hookD }

// newHookRemote builds a hookFakeRemote: a fakeRemote whose PutIndex /
// PutOciLayout fires hook(). The underlying FS is rooted at baseDir.
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
// hookTagStore — a TagStoreV1 that fires a callback on PutIndex/PutOciLayout
// ────────────────────────────────────────────────────────────────────────────

// hookTagStore wraps a *FsOciDirs and fires hook() on every PutIndex call
// (the final commit-last write). Used by the pull ordering test to intercept
// the local tag-file write without needing to replace the Local's concrete
// dirs field.
type hookTagStore struct {
	inner *FsOciDirs
	hook  func()
}

var _ TagStoreV1 = (*hookTagStore)(nil)

func (h *hookTagStore) GetIndex(ctx context.Context, ref imageref.ImageRef) ([]byte, error) {
	return h.inner.GetIndex(ctx, ref)
}

func (h *hookTagStore) GetOciLayout(ctx context.Context, ref imageref.ImageRef) ([]byte, error) {
	return h.inner.GetOciLayout(ctx, ref)
}

func (h *hookTagStore) PutIndex(ctx context.Context, ref imageref.ImageRef, raw []byte) error {
	if h.hook != nil {
		h.hook()
	}
	return h.inner.PutIndex(ctx, ref, raw)
}

func (h *hookTagStore) PutOciLayout(ctx context.Context, ref imageref.ImageRef, raw []byte) error {
	if h.hook != nil {
		h.hook()
	}
	return h.inner.PutOciLayout(ctx, ref, raw)
}

// ────────────────────────────────────────────────────────────────────────────
// Tests
// ────────────────────────────────────────────────────────────────────────────

// TestPush_TagFilesWrittenAfterBlobs verifies that when PutIndex is called
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

	// Snapshot which blobs are present in share/ whenever PutIndex fires.
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
		t.Error("PutIndex was never called (no hook invocation recorded)")
	}

	// Every blob that the push would have sent must already be present at
	// the time PutIndex fired.
	for _, wantHex := range allRealHexes() {
		if !slices.Contains(blobsAtTagFileWrite, wantHex) {
			t.Errorf(
				"blob %s was NOT present in share/ when PutIndex fired "+
					"(blobs seen: %v)",
				wantHex, blobsAtTagFileWrite,
			)
		}
	}
}

// TestPull_TagFilesWrittenAfterBlobs verifies the pull-direction commit-last
// ordering: local PutIndex is called only after all blob data has been
// written into the local share/sha256/ directory.
//
// Because Local.dirs is a concrete *FsOciDirs field (not an interface),
// we cannot swap in a hook by replacing the field. Instead we drive
// pullBlobs + mirrorTagFilesFromPeer directly with a hookTagStore as the
// local destination, preserving the test intent.
func TestPull_TagFilesWrittenAfterBlobs(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(remoteBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(remoteBase, "share")
	seedDump(t, tagDir, shareDir)

	remoteDirs := NewFsOciDirs(remoteFS, 1)
	localDirs := NewFsOciDirs(localFS, 1)
	ctx := context.Background()
	ref, err := imageref.Parse("ghcr.io/a/b:v1")
	if err != nil {
		t.Fatal(err)
	}

	// Read the image closure from the remote.
	view := NewImageView(ctx, remoteDirs, remoteDirs, ref)
	mDesc, man, err := ocidir.ReadManifest(ctx, view)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	descs := ocidir.AllDescriptors(mDesc, man)
	sizes := descriptorSizes(descs)
	digestsSorted := sortedDigests(descriptorDigestSet(descs))
	blobs := toBlobTransfers(digestsSorted, sizes)

	// Build a hookTagStore for the LOCAL side that fires on PutIndex.
	var blobsAtTagFileWrite []string
	localHookTags := &hookTagStore{
		inner: localDirs,
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

	// 1. Pull all blobs from remote into local.
	_, err = pullBlobs(ctx, blobs, remoteDirs, localFS, localDirs, DefaultLocalParallelism, false)
	if err != nil {
		t.Fatalf("pullBlobs: %v", err)
	}

	// 2. Mirror tag files (the "commit" step) using the hook destination.
	if err := mirrorTagFilesFromPeer(ctx, remoteDirs, ref, localHookTags); err != nil {
		t.Fatalf("mirrorTagFilesFromPeer: %v", err)
	}

	if len(blobsAtTagFileWrite) == 0 {
		t.Error("local PutIndex was never called (no hook invocation recorded)")
	}
	// Every blob must have already been written locally before PutIndex fired.
	for _, wantHex := range allRealHexes() {
		if !slices.Contains(blobsAtTagFileWrite, wantHex) {
			t.Errorf(
				"blob %s NOT present locally when PutIndex fired (blobs seen: %v)",
				wantHex, blobsAtTagFileWrite,
			)
		}
	}
}
