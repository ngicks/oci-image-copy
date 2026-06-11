package imagecopy

import (
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/opencontainers/go-digest"
)

// recordingSkopeo is a fake [SkopeoLike] that records calls and lets
// tests fabricate responses. copyTo / copyFrom hooks are dispatched
// based on which side of [Copy] is OCI.
type recordingSkopeo struct {
	versionRet string
	inspectRaw map[string][]byte
	copyTo     func(ctx context.Context, src, dst skopeo.TransportRef, sharedBlobDir string) error
	copyFrom   func(ctx context.Context, src, dst skopeo.TransportRef, sharedBlobDir string) error

	inspectCount  atomic.Int32
	copyToCount   atomic.Int32
	copyFromCount atomic.Int32
}

func (s *recordingSkopeo) Version(ctx context.Context) (string, error) {
	if s.versionRet == "" {
		return "fake-skopeo", nil
	}
	return s.versionRet, nil
}

func (s *recordingSkopeo) Inspect(
	ctx context.Context,
	src skopeo.TransportRef,
	raw bool,
	sharedBlobDir string,
	extraArgs ...string,
) ([]byte, error) {
	s.inspectCount.Add(1)
	if data, ok := s.inspectRaw[string(src.Transport)+":"+src.Arg1]; ok {
		return data, nil
	}
	return nil, errors.New("no inspect fixture")
}

func (s *recordingSkopeo) Copy(
	ctx context.Context,
	src, dst skopeo.TransportRef,
	sharedBlobDir string,
) error {
	switch {
	case dst.Transport == skopeo.TransportOci:
		s.copyToCount.Add(1)
		if s.copyTo != nil {
			return s.copyTo(ctx, src, dst, sharedBlobDir)
		}
	case src.Transport == skopeo.TransportOci:
		s.copyFromCount.Add(1)
		if s.copyFrom != nil {
			return s.copyFrom(ctx, src, dst, sharedBlobDir)
		}
	}
	return nil
}

// fakeRemote is an in-memory [Remote] used by orchestrator tests.
// It wires its own [vroot.Fs[vroot.File]] into a [FsOciDirs] for the read/write
// surface, and a [SkopeoLike] for [Remote.LoadImage].
type fakeRemote struct {
	baseDir   string
	transport skopeo.Transport
	readOnly  bool
	skopeoCli SkopeoLike
	fs        vroot.Fs[vroot.File]
	dirs      *FsOciDirs
	assumeHas map[string]struct{}
}

func newFakeRemote(
	baseDir string,
	transport skopeo.Transport,
	sk SkopeoLike,
	fsys vroot.Fs[vroot.File],
) *fakeRemote {
	return &fakeRemote{
		baseDir:   baseDir,
		transport: transport,
		skopeoCli: sk,
		fs:        fsys,
		dirs:      NewFsOciDirs(fsys, 1),
	}
}

func (f *fakeRemote) Close() error   { return nil }
func (f *fakeRemote) ReadOnly() bool { return f.readOnly }
func (f *fakeRemote) Dir() OciDirs   { return f.dirs }

func (f *fakeRemote) ListBlobs(ctx context.Context) iter.Seq2[digest.Digest, error] {
	if f.assumeHas != nil {
		assume := f.assumeHas
		return func(yield func(digest.Digest, error) bool) {
			for d := range assume {
				if !yield(digest.Digest(d), nil) {
					return
				}
			}
		}
	}
	return listBlobsFromFs(ctx, f.fs)
}

func (f *fakeRemote) ListImages(ctx context.Context) iter.Seq2[imageref.ImageRef, error] {
	return listImagesFromFs(ctx, f.fs)
}

func (f *fakeRemote) LoadImage(ctx context.Context, ref imageref.ImageRef) error {
	if f.readOnly {
		return ErrReadOnly
	}
	if f.transport == skopeo.TransportOci {
		return nil
	}
	store := NewStore(f.baseDir)
	tagDirAbs, err := store.DumpDir(ref)
	if err != nil {
		return err
	}
	return f.skopeoCli.Copy(ctx,
		skopeo.TransportRef{Transport: skopeo.TransportOci, Arg1: tagDirAbs, Arg2: ref.String()},
		skopeo.TransportRef{Transport: f.transport, Arg1: ref.String()},
		store.ShareDir(),
	)
}

// newSides constructs local + remote FSes (both NewOsFs
// rooted at separate temp dirs) for orchestrator tests.
func newSides(t *testing.T) (localFS, remoteFS vroot.Fs[vroot.File], localBase, remoteBase string) {
	t.Helper()
	localBase = t.TempDir()
	remoteBase = t.TempDir()
	var err error
	localFS, err = NewOsFs(localBase)
	if err != nil {
		t.Fatal(err)
	}
	remoteFS, err = NewOsFs(remoteBase)
	if err != nil {
		t.Fatal(err)
	}
	return
}

// seedDump materializes a real OCI dump under tagDir + the blobs in
// shareDir/sha256. The blobs are written with content whose SHA-256
// digests and sizes match what the manifest declares, so that the fsutil
// resumable transfer machinery (which checks sizes and the sha256 pre-
// commit hook verifies digests) can transfer them correctly.
//
// Returns the manifest digest string ("sha256:<hex>").
func seedDump(t *testing.T, tagDir, shareDir string) (manifestDigest string) {
	t.Helper()
	must(t, os.MkdirAll(tagDir, 0o755))
	must(t, os.MkdirAll(filepath.Join(shareDir, "sha256"), 0o755))
	must(
		t,
		os.WriteFile(
			filepath.Join(tagDir, "oci-layout"),
			[]byte(`{"imageLayoutVersion":"1.0.0"}`),
			0o644,
		),
	)
	must(t, os.WriteFile(filepath.Join(tagDir, "index.json"), []byte(realIndexJSON), 0o644))

	manifestDigest = "sha256:" + realManifestHex
	must(
		t,
		os.WriteFile(
			filepath.Join(shareDir, "sha256", realManifestHex),
			[]byte(realManifestContent),
			0o644,
		),
	)
	must(
		t,
		os.WriteFile(
			filepath.Join(shareDir, "sha256", realConfigHex),
			[]byte(realConfigContent),
			0o644,
		),
	)
	must(
		t,
		os.WriteFile(
			filepath.Join(shareDir, "sha256", realLayer1Hex),
			[]byte(realLayer1Content),
			0o644,
		),
	)
	must(
		t,
		os.WriteFile(
			filepath.Join(shareDir, "sha256", realLayer2Hex),
			[]byte(realLayer2Content),
			0o644,
		),
	)
	return manifestDigest
}

// allRealHexes returns all blob hex values used in the real dump fixture.
func allRealHexes() []string {
	return []string{realManifestHex, realConfigHex, realLayer1Hex, realLayer2Hex}
}

func newLocal(localFS vroot.Fs[vroot.File], base string, sk SkopeoLike) *Local {
	return &Local{
		baseDir:   base,
		transport: skopeo.TransportContainersStorage,
		skopeoCli: sk,
		fs:        localFS,
		dirs:      NewFsOciDirs(localFS, 1),
	}
}

func TestPush_HappyPath_Sends_Then_Loads(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(localBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(localBase, "share")
	seedDump(t, tagDir, shareDir)

	localSk := &recordingSkopeo{
		copyTo: func(ctx context.Context, src, dst skopeo.TransportRef, sharedBlobDir string) error {
			return nil
		},
	}
	remoteSk := &recordingSkopeo{}

	local := newLocal(localFS, localBase, localSk)
	remote := newFakeRemote(remoteBase, skopeo.TransportContainersStorage, remoteSk, remoteFS)
	remote.assumeHas = map[string]struct{}{}

	res, err := local.Push(context.Background(), PushArgs{
		Images: []string{"ghcr.io/a/b:v1"},
	}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("FailedCount=%d, reports=%+v", res.FailedCount, res.Reports)
	}
	if got := localSk.copyToCount.Load(); got != 1 {
		t.Errorf("local skopeo CopyToOCI called %d, want 1", got)
	}
	if got := remoteSk.copyFromCount.Load(); got != 1 {
		t.Errorf("remote skopeo CopyFromOCI (LoadImage) called %d, want 1", got)
	}

	remoteShare := filepath.Join(remoteBase, "share", "sha256")
	for _, hex := range allRealHexes() {
		if _, err := os.Stat(filepath.Join(remoteShare, hex)); err != nil {
			t.Errorf("expected remote blob %s present: %v", hex, err)
		}
	}
	for _, n := range []string{"index.json", "oci-layout"} {
		if _, err := os.Stat(
			filepath.Join(remoteBase, "ghcr.io", "a", "b", "_tags", "v1", n),
		); err != nil {
			t.Errorf("expected remote tag-dir file %s: %v", n, err)
		}
	}
}

func TestPush_ReusesRemoteHas(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(localBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(localBase, "share")
	seedDump(t, tagDir, shareDir)

	remoteHas := map[string]struct{}{
		"sha256:" + realLayer1Hex: {},
		"sha256:" + realLayer2Hex: {},
	}

	local := newLocal(localFS, localBase, &recordingSkopeo{})
	remote := newFakeRemote(
		remoteBase,
		skopeo.TransportContainersStorage,
		&recordingSkopeo{},
		remoteFS,
	)
	remote.assumeHas = remoteHas

	res, err := local.Push(context.Background(), PushArgs{
		Images:             []string{"ghcr.io/a/b:v1"},
		AssumeRemoteHasSet: remoteHas,
	}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Reports[0].Sent; got != 2 {
		t.Errorf("Sent = %d, want 2 (manifest + config; layers reused)", got)
	}
	if got := res.Reports[0].Reused; got != 2 {
		t.Errorf("Reused = %d, want 2 (the two layers)", got)
	}
}

func TestPush_DryRun_NoMutationsAnywhere(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	rawManifest := []byte(realManifestContent)
	localSk := &recordingSkopeo{
		inspectRaw: map[string][]byte{
			"containers-storage:ghcr.io/a/b:v1": rawManifest,
		},
	}
	remoteSk := &recordingSkopeo{}

	beforeLocal := snapshotDir(t, localBase)
	beforeRemote := snapshotDir(t, remoteBase)

	local := newLocal(localFS, localBase, localSk)
	remote := newFakeRemote(remoteBase, skopeo.TransportContainersStorage, remoteSk, remoteFS)
	remote.assumeHas = map[string]struct{}{}

	res, err := local.Push(context.Background(), PushArgs{
		Images: []string{"ghcr.io/a/b:v1"},
		DryRun: true,
	}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("dry-run had failures: %+v", res.Reports)
	}
	if localSk.copyToCount.Load() != 0 {
		t.Errorf("dry-run called CopyToOCI %d times, want 0", localSk.copyToCount.Load())
	}
	if remoteSk.copyFromCount.Load() != 0 {
		t.Errorf("dry-run called CopyFromOCI %d times, want 0", remoteSk.copyFromCount.Load())
	}
	if got := localSk.inspectCount.Load(); got != 1 {
		t.Errorf("dry-run Inspect count %d, want 1", got)
	}
	afterLocal := snapshotDir(t, localBase)
	afterRemote := snapshotDir(t, remoteBase)
	if beforeLocal != afterLocal {
		t.Errorf("local mutated: before=%v after=%v", beforeLocal, afterLocal)
	}
	if beforeRemote != afterRemote {
		t.Errorf("remote mutated: before=%v after=%v", beforeRemote, afterRemote)
	}
	if !res.Reports[0].DryRun {
		t.Error("DryRun report flag not set")
	}
	if !strings.HasPrefix(res.Reports[0].SummaryLine(), "DRY-RUN would:") {
		t.Errorf("summary missing DRY-RUN prefix: %q", res.Reports[0].SummaryLine())
	}
}

func TestPush_KeepGoing_AccumulatesErrors(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(localBase, "ghcr.io", "a", "ok", "_tags", "v1")
	shareDir := filepath.Join(localBase, "share")
	seedDump(t, tagDir, shareDir)

	localSk := &recordingSkopeo{
		copyTo: func(ctx context.Context, src, dst skopeo.TransportRef, sharedBlobDir string) error {
			if strings.Contains(src.Arg1, "fail") {
				return errors.New("simulated failure")
			}
			return nil
		},
	}

	local := newLocal(localFS, localBase, localSk)
	remote := newFakeRemote(
		remoteBase,
		skopeo.TransportContainersStorage,
		&recordingSkopeo{},
		remoteFS,
	)
	remote.assumeHas = map[string]struct{}{}

	res, err := local.Push(context.Background(), PushArgs{
		Images:    []string{"ghcr.io/a/ok:v1", "ghcr.io/a/fail:v1"},
		KeepGoing: true,
	}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 1 {
		t.Errorf("FailedCount = %d, want 1; reports=%+v", res.FailedCount, res.Reports)
	}
	if len(res.Reports) != 2 {
		t.Errorf("got %d reports, want 2", len(res.Reports))
	}
}

func TestPush_AssumeRemoteHasFromRawStrings_NoEnumeration(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(localBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(localBase, "share")
	seedDump(t, tagDir, shareDir)

	peerSk := &recordingSkopeo{} // Inspect fails by default

	local := newLocal(localFS, localBase, &recordingSkopeo{})
	remote := newFakeRemote(remoteBase, skopeo.TransportContainersStorage, peerSk, remoteFS)

	_, err := local.Push(context.Background(), PushArgs{
		Images:          []string{"ghcr.io/a/b:v1"},
		AssumeRemoteHas: []string{"sha256:" + strings.Repeat("9", 64)},
	}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if peerSk.inspectCount.Load() != 0 {
		t.Errorf(
			"peer skopeo Inspect called %d times, want 0 (--assume-remote-has should skip enumeration)",
			peerSk.inspectCount.Load(),
		)
	}
}

func TestPush_RejectsReadOnlyRemote(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(localBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(localBase, "share")
	seedDump(t, tagDir, shareDir)

	local := newLocal(localFS, localBase, &recordingSkopeo{})
	remote := newFakeRemote(
		remoteBase,
		skopeo.TransportContainersStorage,
		&recordingSkopeo{},
		remoteFS,
	)
	remote.readOnly = true
	remote.assumeHas = map[string]struct{}{}

	_, err := local.Push(context.Background(), PushArgs{
		Images: []string{"ghcr.io/a/b:v1"},
	}, remote)
	if err == nil {
		t.Fatal("expected error for read-only peer, got nil")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("error %v does not mention read-only", err)
	}
}

// snapshotDir returns a stable string of all paths under root +
// their sizes, for "did anything change" assertions.
func snapshotDir(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b.WriteString(rel)
		b.WriteByte('\t')
		if !fi.IsDir() {
			b.WriteString(int64ToStr(fi.Size()))
		} else {
			b.WriteString("dir")
		}
		b.WriteByte('\n')
		return nil
	})
	return b.String()
}

func int64ToStr(n int64) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}
