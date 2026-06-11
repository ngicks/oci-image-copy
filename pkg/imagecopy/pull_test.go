package imagecopy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
)

func TestPull_HappyPath(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(remoteBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(remoteBase, "share")
	seedDump(t, tagDir, shareDir)

	localSk := &recordingSkopeo{}
	peerSk := &recordingSkopeo{
		copyTo: func(ctx context.Context, src, dst skopeo.TransportRef, sharedBlobDir string) error {
			return nil
		},
	}

	local := newLocal(localFS, localBase, localSk)
	remote := newFakeRemote(remoteBase, skopeo.TransportContainersStorage, peerSk, remoteFS)

	res, err := local.Pull(context.Background(), PullArgs{
		Images:            []string{"ghcr.io/a/b:v1"},
		AssumeLocalHasSet: map[string]struct{}{},
	}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("FailedCount=%d, reports=%+v", res.FailedCount, res.Reports)
	}
	// Pull triggers DumpImage once, which calls skopeoCli.Copy(transport→oci).
	if peerSk.copyToCount.Load() != 1 {
		t.Errorf(
			"peer skopeo CopyToOCI (DumpImage) called %d, want 1",
			peerSk.copyToCount.Load(),
		)
	}
	if localSk.copyFromCount.Load() != 1 {
		t.Errorf(
			"local skopeo CopyFromOCI (LoadImage) called %d, want 1",
			localSk.copyFromCount.Load(),
		)
	}
	for _, hex := range allRealHexes() {
		if _, err := os.Stat(filepath.Join(localBase, "share", "sha256", hex)); err != nil {
			t.Errorf("expected local blob %s present: %v", hex, err)
		}
	}
}

func TestPull_DryRun_NoMutationsAnywhere(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	rawManifest := []byte(realManifestContent)
	peerSk := &recordingSkopeo{
		inspectRaw: map[string][]byte{
			"containers-storage:ghcr.io/a/b:v1": rawManifest,
		},
	}
	localSk := &recordingSkopeo{}

	tagDir := filepath.Join(remoteBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(remoteBase, "share")
	seedDump(t, tagDir, shareDir)

	beforeLocal := snapshotDir(t, localBase)
	beforeRemote := snapshotDir(t, remoteBase)

	local := newLocal(localFS, localBase, localSk)
	remote := newFakeRemote(remoteBase, skopeo.TransportContainersStorage, peerSk, remoteFS)

	res, err := local.Pull(context.Background(), PullArgs{
		Images:            []string{"ghcr.io/a/b:v1"},
		DryRun:            true,
		AssumeLocalHasSet: map[string]struct{}{},
	}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("dry-run had failures: %+v", res.Reports)
	}
	if peerSk.copyToCount.Load() != 0 {
		t.Errorf("dry-run called peer CopyToOCI %d, want 0", peerSk.copyToCount.Load())
	}
	if localSk.copyFromCount.Load() != 0 {
		t.Errorf("dry-run called local CopyFromOCI %d, want 0", localSk.copyFromCount.Load())
	}
	if afterLocal := snapshotDir(t, localBase); afterLocal != beforeLocal {
		t.Errorf("local mutated: before=%v after=%v", beforeLocal, afterLocal)
	}
	if afterRemote := snapshotDir(t, remoteBase); afterRemote != beforeRemote {
		t.Errorf("remote mutated: before=%v after=%v", beforeRemote, afterRemote)
	}
	if !res.Reports[0].DryRun {
		t.Error("DryRun report flag not set")
	}
	if !strings.HasPrefix(res.Reports[0].SummaryLine(), "DRY-RUN would:") {
		t.Errorf("summary missing DRY-RUN prefix: %q", res.Reports[0].SummaryLine())
	}
}

func TestPull_AssumeLocalHas_SkipsEnumeration(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(remoteBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(remoteBase, "share")
	seedDump(t, tagDir, shareDir)

	localSk := &recordingSkopeo{}
	local := newLocal(localFS, localBase, localSk)
	remote := newFakeRemote(
		remoteBase,
		skopeo.TransportContainersStorage,
		&recordingSkopeo{
			copyTo: func(_ context.Context, _, _ skopeo.TransportRef, _ string) error { return nil },
		},
		remoteFS,
	)

	_, err := local.Pull(context.Background(), PullArgs{
		Images:         []string{"ghcr.io/a/b:v1"},
		AssumeLocalHas: []string{"sha256:" + strings.Repeat("9", 64)},
	}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if localSk.inspectCount.Load() != 0 {
		t.Errorf("local skopeo Inspect called %d, want 0", localSk.inspectCount.Load())
	}
}

func TestPull_ResumeFromInterruptedPart(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)
	tagDir := filepath.Join(remoteBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(remoteBase, "share")
	seedDump(t, tagDir, shareDir)

	// Pre-seed a .part file containing 1 byte of layer1 content.
	// The full content is "L1" (2 bytes). The resumable pull should
	// append the remaining byte and then rename the part to final.
	localSha := filepath.Join(localBase, "share", "sha256")
	must(t, os.MkdirAll(localSha, 0o755))
	partPath := filepath.Join(localSha, realLayer1Hex+".part")
	// Write 1 byte of the 2-byte "L1" content.
	must(t, os.WriteFile(partPath, []byte("L"), 0o644))
	// Write the ETag sidecar so the resume path trusts this partial.
	must(t, os.WriteFile(partPath+".etag", []byte("sha256:"+realLayer1Hex), 0o644))

	local := newLocal(localFS, localBase, &recordingSkopeo{})
	remote := newFakeRemote(
		remoteBase,
		skopeo.TransportContainersStorage,
		&recordingSkopeo{
			copyTo: func(ctx context.Context, _, _ skopeo.TransportRef, _ string) error { return nil },
		},
		remoteFS,
	)

	res, err := local.Pull(context.Background(), PullArgs{
		Images:            []string{"ghcr.io/a/b:v1"},
		AssumeLocalHasSet: map[string]struct{}{},
	}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("FailedCount=%d, reports=%+v", res.FailedCount, res.Reports)
	}
	got, err := os.ReadFile(filepath.Join(localSha, realLayer1Hex))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != realLayer1Content {
		t.Errorf("after resume got %q, want %q", got, realLayer1Content)
	}
	if _, err := os.Stat(partPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".part should be gone: stat err=%v", err)
	}
}

// TestPull_DumpImage_CalledOncePerRef verifies that pullOne calls
// DumpImage exactly once per ref, before the manifest read.
func TestPull_DumpImage_CalledOncePerRef(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(remoteBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(remoteBase, "share")
	seedDump(t, tagDir, shareDir)

	local := newLocal(localFS, localBase, &recordingSkopeo{})
	remote := newFakeRemote(
		remoteBase,
		skopeo.TransportContainersStorage,
		&recordingSkopeo{
			copyTo: func(_ context.Context, _, _ skopeo.TransportRef, _ string) error { return nil },
		},
		remoteFS,
	)

	_, err := local.Pull(context.Background(), PullArgs{
		Images:            []string{"ghcr.io/a/b:v1"},
		AssumeLocalHasSet: map[string]struct{}{},
	}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if got := remote.dumpCount.Load(); got != 1 {
		t.Errorf("DumpImage called %d times, want 1", got)
	}
}

// TestPull_DumpImage_Error_KeepGoing verifies that a DumpImage error
// is treated as a per-image failure and that KeepGoing is honored.
func TestPull_DumpImage_Error_KeepGoing(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	// Seed one valid image.
	tagDir := filepath.Join(remoteBase, "ghcr.io", "a", "ok", "_tags", "v1")
	shareDir := filepath.Join(remoteBase, "share")
	seedDump(t, tagDir, shareDir)

	dumpErr := errors.New("simulated dump failure")
	local := newLocal(localFS, localBase, &recordingSkopeo{})
	remote := newFakeRemote(
		remoteBase,
		skopeo.TransportContainersStorage,
		&recordingSkopeo{
			copyTo: func(_ context.Context, src, _ skopeo.TransportRef, _ string) error {
				return nil
			},
		},
		remoteFS,
	)
	// Override DumpImage: fail for "fail", succeed for "ok".
	remote.dumpImageFn = func(_ context.Context, ref imageref.ImageRef) error {
		if strings.Contains(ref.Path, "fail") {
			return dumpErr
		}
		return nil
	}

	res, err := local.Pull(context.Background(), PullArgs{
		Images:    []string{"ghcr.io/a/ok:v1", "ghcr.io/a/fail:v1"},
		KeepGoing: true,
		// Use an empty set to skip local enumeration overhead.
		AssumeLocalHasSet: map[string]struct{}{},
	}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 1 {
		t.Errorf("FailedCount = %d, want 1; reports=%+v", res.FailedCount, res.Reports)
	}
	if len(res.Reports) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(res.Reports))
	}
	if res.Reports[0].Err != nil {
		t.Errorf("report[0] (ok) should succeed, got: %v", res.Reports[0].Err)
	}
	if res.Reports[1].Err == nil {
		t.Error("report[1] (fail) should have error")
	}
	if !strings.Contains(res.Reports[1].Err.Error(), "peer dump") {
		t.Errorf("error should mention 'peer dump': %v", res.Reports[1].Err)
	}
}

// TestPull_DumpImage_ErrReadOnly_ContentPresent verifies that when the
// peer is read-only, Pull logs a warning but succeeds if the mirror
// already has the content.
func TestPull_DumpImage_ErrReadOnly_ContentPresent(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(remoteBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(remoteBase, "share")
	seedDump(t, tagDir, shareDir)

	local := newLocal(localFS, localBase, &recordingSkopeo{})
	remote := newFakeRemote(
		remoteBase, skopeo.TransportContainersStorage, &recordingSkopeo{}, remoteFS,
	)
	remote.readOnly = true // DumpImage will return ErrReadOnly

	res, err := local.Pull(context.Background(), PullArgs{
		Images:            []string{"ghcr.io/a/b:v1"},
		AssumeLocalHasSet: map[string]struct{}{},
	}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("expected success (mirror has content); FailedCount=%d reports=%+v",
			res.FailedCount, res.Reports)
	}
}

// TestPull_DumpImage_ErrReadOnly_ContentAbsent verifies that when the
// peer is read-only and the mirror is empty, Pull returns an error
// from the manifest read.
func TestPull_DumpImage_ErrReadOnly_ContentAbsent(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)
	// Do NOT seed the remote mirror.

	local := newLocal(localFS, localBase, &recordingSkopeo{})
	remote := newFakeRemote(
		remoteBase, skopeo.TransportContainersStorage, &recordingSkopeo{}, remoteFS,
	)
	remote.readOnly = true

	res, err := local.Pull(context.Background(), PullArgs{
		Images:            []string{"ghcr.io/a/b:v1"},
		AssumeLocalHasSet: map[string]struct{}{},
		KeepGoing:         true,
	}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount == 0 {
		t.Error("expected failure when mirror is empty and peer is read-only")
	}
	if len(res.Reports) > 0 && res.Reports[0].Err != nil {
		errMsg := res.Reports[0].Err.Error()
		if !strings.Contains(errMsg, "read-only") {
			t.Errorf(
				"error should mention 'read-only' to hint that dump was skipped: %v",
				res.Reports[0].Err,
			)
		}
	}
}

// TestPull_DryRun_ReadOnlyPeer verifies that a dry-run pull against a
// read-only peer succeeds: InspectImage is called exactly once to
// obtain the manifest, DumpImage is never called, and neither the
// local nor the remote store is mutated.
func TestPull_DryRun_ReadOnlyPeer(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	rawManifest := []byte(realManifestContent)
	peerSk := &recordingSkopeo{
		inspectRaw: map[string][]byte{
			"containers-storage:ghcr.io/a/b:v1": rawManifest,
		},
	}

	local := newLocal(localFS, localBase, &recordingSkopeo{})
	remote := newFakeRemote(remoteBase, skopeo.TransportContainersStorage, peerSk, remoteFS)
	remote.readOnly = true // DumpImage would return ErrReadOnly; must not be called

	beforeRemote := snapshotDir(t, remoteBase)
	beforeLocal := snapshotDir(t, localBase)

	res, err := local.Pull(context.Background(), PullArgs{
		Images:            []string{"ghcr.io/a/b:v1"},
		DryRun:            true,
		AssumeLocalHasSet: map[string]struct{}{},
	}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("dry-run with read-only peer had failures: %+v", res.Reports)
	}

	// InspectImage must be called exactly once (via peerSk.Inspect).
	if got := peerSk.inspectCount.Load(); got != 1 {
		t.Errorf("dry-run: InspectImage called %d times, want 1", got)
	}

	// DumpImage must never be called on dry-run.
	if got := remote.dumpCount.Load(); got != 0 {
		t.Errorf("dry-run: DumpImage called %d times, want 0", got)
	}

	// Neither local nor remote must be mutated.
	if afterLocal := snapshotDir(t, localBase); afterLocal != beforeLocal {
		t.Errorf("dry-run mutated local:\nbefore=%v\nafter=%v", beforeLocal, afterLocal)
	}
	if afterRemote := snapshotDir(t, remoteBase); afterRemote != beforeRemote {
		t.Errorf("dry-run mutated remote:\nbefore=%v\nafter=%v", beforeRemote, afterRemote)
	}
}

// TestPull_DryRun_InspectImage_Used verifies that dry-run calls
// InspectImage (not DumpImage) and leaves everything unmutated.
func TestPull_DryRun_InspectImage_Used(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	rawManifest := []byte(realManifestContent)
	peerSk := &recordingSkopeo{
		inspectRaw: map[string][]byte{
			"containers-storage:ghcr.io/a/b:v1": rawManifest,
		},
	}

	local := newLocal(localFS, localBase, &recordingSkopeo{})
	remote := newFakeRemote(remoteBase, skopeo.TransportContainersStorage, peerSk, remoteFS)

	beforeRemote := snapshotDir(t, remoteBase)

	res, err := local.Pull(context.Background(), PullArgs{
		Images:            []string{"ghcr.io/a/b:v1"},
		DryRun:            true,
		AssumeLocalHasSet: map[string]struct{}{},
	}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("dry-run had failures: %+v", res.Reports)
	}

	// DumpImage must NOT be called on dry-run.
	if got := remote.dumpCount.Load(); got != 0 {
		t.Errorf("dry-run: DumpImage called %d times, want 0", got)
	}
	// InspectImage (via peerSk.Inspect) must have been called once.
	if got := peerSk.inspectCount.Load(); got != 1 {
		t.Errorf("dry-run: InspectImage called %d times, want 1", got)
	}
	// Remote must not be mutated.
	if afterRemote := snapshotDir(t, remoteBase); afterRemote != beforeRemote {
		t.Errorf("dry-run mutated remote:\nbefore=%v\nafter=%v", beforeRemote, afterRemote)
	}
}
