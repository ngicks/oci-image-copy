package ociimagecopy

// sshRemote argv-level tests for DumpImage and InspectImage.
// These tests exercise the command-line construction without a real SSH
// connection by injecting a fake SkopeoLike and a real in-process FS.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
)

// buildTestSshRemote constructs an sshRemote suitable for unit testing:
// no real SSH connection, no SFTP client, but with a real OS-backed FS
// rooted at baseDir and an injected SkopeoLike for command recording.
func buildTestSshRemote(
	t *testing.T,
	baseDir string,
	transport skopeo.Transport,
	sk SkopeoLike,
) *sshRemote {
	t.Helper()
	fs, err := NewOsFs(baseDir)
	if err != nil {
		t.Fatalf("NewOsFs: %v", err)
	}
	r := &sshRemote{
		baseDir:   baseDir,
		transport: transport,
		skopeoCli: sk,
		fs:        fs,
		dirs:      NewFsOciDirs(fs, 1),
		closedCh:  make(chan struct{}),
	}
	return r
}

// testDumpImageArgv is the shared argv-assertion logic for DumpImage tests.
// It verifies that DumpImage calls skopeoCli.Copy with:
//   - src.Transport == transport
//   - src.Arg1 == ref.String()
//   - dst.Transport == skopeo.TransportOci
//   - dst.Arg1 == the absolute tag dir derived from baseDir + RelDumpDir(ref)
//   - dst.Arg2 == ref.String()
//   - sharedBlobDir == baseDir/share
func testDumpImageArgv(t *testing.T, transport skopeo.Transport) {
	t.Helper()
	baseDir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(baseDir, "share"), 0o755))

	ref, err := imageref.Parse("ghcr.io/org/repo:v1")
	if err != nil {
		t.Fatal(err)
	}

	// Compute the expected values the same way the production code does.
	relDump, err := RelDumpDir(ref)
	if err != nil {
		t.Fatal(err)
	}
	wantTagDirAbs := filepath.ToSlash(filepath.Join(baseDir, filepath.FromSlash(relDump)))
	wantShareAbs := filepath.ToSlash(filepath.Join(baseDir, "share"))

	var (
		capturedSrc           skopeo.TransportRef
		capturedDst           skopeo.TransportRef
		capturedSharedBlobDir string
	)
	sk := &recordingSkopeo{
		copyTo: func(
			_ context.Context,
			src, dst skopeo.TransportRef,
			sharedBlobDir string,
		) error {
			capturedSrc = src
			capturedDst = dst
			capturedSharedBlobDir = sharedBlobDir
			return nil
		},
	}
	r := buildTestSshRemote(t, baseDir, transport, sk)

	if err := r.DumpImage(context.Background(), ref); err != nil {
		t.Fatalf("DumpImage: %v", err)
	}

	// DumpImage must call skopeoCli.Copy(src=transport, dst=oci) exactly once.
	if got := sk.copyToCount.Load(); got != 1 {
		t.Errorf("Copy(to-oci) called %d times, want 1", got)
	}
	if got := sk.copyFromCount.Load(); got != 0 {
		t.Errorf("Copy(from-oci) called %d times, want 0", got)
	}

	// Assert exact src fields.
	if capturedSrc.Transport != transport {
		t.Errorf("src.Transport = %q, want %q", capturedSrc.Transport, transport)
	}
	if capturedSrc.Arg1 != ref.String() {
		t.Errorf("src.Arg1 = %q, want %q", capturedSrc.Arg1, ref.String())
	}

	// Assert exact dst fields.
	if capturedDst.Transport != skopeo.TransportOci {
		t.Errorf("dst.Transport = %q, want %q", capturedDst.Transport, skopeo.TransportOci)
	}
	if capturedDst.Arg1 != wantTagDirAbs {
		t.Errorf("dst.Arg1 (tagDirAbs) = %q, want %q", capturedDst.Arg1, wantTagDirAbs)
	}
	if capturedDst.Arg2 != ref.String() {
		t.Errorf("dst.Arg2 = %q, want %q", capturedDst.Arg2, ref.String())
	}

	// Assert sharedBlobDir.
	if capturedSharedBlobDir != wantShareAbs {
		t.Errorf("sharedBlobDir = %q, want %q", capturedSharedBlobDir, wantShareAbs)
	}
}

// TestSshRemote_DumpImage_ContainersStorage_Argv verifies that DumpImage
// builds the correct skopeo copy argv for the containers-storage transport.
func TestSshRemote_DumpImage_ContainersStorage_Argv(t *testing.T) {
	t.Parallel()
	testDumpImageArgv(t, skopeo.TransportContainersStorage)
}

// TestSshRemote_DumpImage_DockerDaemon_Argv verifies the same for
// docker-daemon transport.
func TestSshRemote_DumpImage_DockerDaemon_Argv(t *testing.T) {
	t.Parallel()
	testDumpImageArgv(t, skopeo.TransportDockerDaemon)
}

// TestSshRemote_DumpImage_Oci_IsNoOp verifies that DumpImage is a no-op
// for the oci transport (no skopeo call, nil error).
func TestSshRemote_DumpImage_Oci_IsNoOp(t *testing.T) {
	t.Parallel()
	baseDir := t.TempDir()

	sk := &recordingSkopeo{}
	r := buildTestSshRemote(t, baseDir, skopeo.TransportOci, sk)

	ref, err := imageref.Parse("ghcr.io/org/repo:v1")
	if err != nil {
		t.Fatal(err)
	}

	if err := r.DumpImage(context.Background(), ref); err != nil {
		t.Fatalf("DumpImage (oci): expected nil, got: %v", err)
	}

	// No skopeo calls for oci transport.
	if got := sk.copyToCount.Load() + sk.copyFromCount.Load(); got != 0 {
		t.Errorf("oci DumpImage made %d skopeo calls, want 0", got)
	}
}

// testInspectImageArgv is the shared argv-assertion logic for InspectImage
// tests. It verifies that InspectImage calls skopeoCli.Inspect with:
//   - src.Transport == transport
//   - src.Arg1 == ref.String()
//   - raw == true (so the caller gets raw manifest bytes, not re-marshalled JSON)
func testInspectImageArgv(t *testing.T, transport skopeo.Transport) {
	t.Helper()
	baseDir := t.TempDir()
	rawManifest := []byte(realManifestContent)

	ref, err := imageref.Parse("ghcr.io/org/repo:v1")
	if err != nil {
		t.Fatal(err)
	}

	sk := &recordingSkopeo{
		inspectRaw: map[string][]byte{
			string(transport) + ":" + ref.String(): rawManifest,
		},
	}
	r := buildTestSshRemote(t, baseDir, transport, sk)

	got, err := r.InspectImage(context.Background(), ref)
	if err != nil {
		t.Fatalf("InspectImage: %v", err)
	}
	if string(got) != string(rawManifest) {
		t.Errorf("InspectImage returned wrong bytes:\ngot:  %q\nwant: %q", got, rawManifest)
	}
	if n := sk.inspectCount.Load(); n != 1 {
		t.Errorf("Inspect called %d times, want 1", n)
	}

	// Assert the TransportRef and raw flag passed to Inspect.
	call := sk.lastInspect
	if call.Src.Transport != transport {
		t.Errorf("Inspect src.Transport = %q, want %q", call.Src.Transport, transport)
	}
	if call.Src.Arg1 != ref.String() {
		t.Errorf("Inspect src.Arg1 = %q, want %q", call.Src.Arg1, ref.String())
	}
	if !call.Raw {
		t.Error("Inspect raw = false, want true (must request raw manifest bytes)")
	}
}

// TestSshRemote_InspectImage_ContainersStorage_Argv verifies that
// InspectImage calls skopeo inspect --raw for the containers-storage transport.
func TestSshRemote_InspectImage_ContainersStorage_Argv(t *testing.T) {
	t.Parallel()
	testInspectImageArgv(t, skopeo.TransportContainersStorage)
}

// TestSshRemote_InspectImage_DockerDaemon_Argv verifies the same for
// docker-daemon transport.
func TestSshRemote_InspectImage_DockerDaemon_Argv(t *testing.T) {
	t.Parallel()
	testInspectImageArgv(t, skopeo.TransportDockerDaemon)
}

// TestSshRemote_InspectImage_Oci_ReadsFromMirror verifies that for the oci
// transport, InspectImage reads the manifest bytes from the mirror
// (no skopeo call) and returns the exact raw bytes (so sha256 is preserved).
func TestSshRemote_InspectImage_Oci_ReadsFromMirror(t *testing.T) {
	t.Parallel()
	baseDir := t.TempDir()
	tagDir := filepath.Join(baseDir, "ghcr.io", "org", "repo", "_tags", "v1")
	shareDir := filepath.Join(baseDir, "share")
	seedDump(t, tagDir, shareDir)

	sk := &recordingSkopeo{}
	r := buildTestSshRemote(t, baseDir, skopeo.TransportOci, sk)

	ref, err := imageref.Parse("ghcr.io/org/repo:v1")
	if err != nil {
		t.Fatal(err)
	}

	got, err := r.InspectImage(context.Background(), ref)
	if err != nil {
		t.Fatalf("InspectImage (oci): %v", err)
	}

	// The returned bytes must be exactly the raw manifest content so that
	// sha256(got) == realManifestHex.
	if string(got) != realManifestContent {
		t.Errorf("InspectImage (oci) returned wrong bytes:\ngot: %q\nwant: %q",
			got, realManifestContent)
	}
	// No skopeo calls for oci transport.
	if n := sk.inspectCount.Load(); n != 0 {
		t.Errorf("oci InspectImage made %d skopeo inspect calls, want 0", n)
	}
}

// TestParseDumpDirRel_Valid checks that well-formed dump-dir paths reconstruct
// the expected ImageRef.
func TestParseDumpDirRel_Valid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		rel     string
		wantStr string
	}{
		{"tagged", "ghcr.io/a/b/_tags/v1", "ghcr.io/a/b:v1"},
		{
			"digested",
			"ghcr.io/a/b/_digests/" + repeat("a", 64),
			"ghcr.io/a/b@sha256:" + repeat("a", 64),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := parseDumpDirRel(tc.rel)
			if err != nil {
				t.Fatalf("parseDumpDirRel(%q): %v", tc.rel, err)
			}
			if ref.String() != tc.wantStr {
				t.Errorf("got %q, want %q", ref.String(), tc.wantStr)
			}
		})
	}
}

// TestParseDumpDirRel_RejectsMalicious checks the read-side validation guards
// against maliciously-named dump dirs on a peer (traversal host/segment,
// slash/overlong tag, bad digest hex).
func TestParseDumpDirRel_RejectsMalicious(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, rel string }{
		{"traversal segment", "ghcr.io/a/../b/_tags/v1"},
		{"overlong tag", "ghcr.io/a/b/_tags/" + repeat("x", 129)},
		{"bad tag leading char", "ghcr.io/a/b/_tags/.bad"},
		{"reserved segment", "ghcr.io/a/_tags/b/_tags/v1"},
		{"short digest hex", "ghcr.io/a/b/_digests/abc"},
		{"non-hex digest", "ghcr.io/a/b/_digests/" + repeat("z", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseDumpDirRel(tc.rel); err == nil {
				t.Fatalf("parseDumpDirRel(%q) = nil error, want rejection", tc.rel)
			}
		})
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}
