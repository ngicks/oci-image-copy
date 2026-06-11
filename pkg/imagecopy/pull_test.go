package imagecopy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
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
	// Peer is now a passive mirror — Pull no longer triggers a peer-side
	// dump, so peer skopeo should not have been called.
	if peerSk.copyToCount.Load() != 0 {
		t.Errorf(
			"peer skopeo CopyToOCI called %d, want 0 (peer is passive mirror)",
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
