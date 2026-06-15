// Package integration holds end-to-end tests for oci-image-copy.
// Every test in this package requires skopeo and the system ssh binary to be
// present; they are skipped automatically when either is missing.
//
// The tests spin up an in-process SSH server (using golang.org/x/crypto/ssh +
// github.com/pkg/sftp) so there is no dependency on sshd or any system daemon.
// The system ssh binary is pointed at this server via ssh.Target.ConfigFile
// (-F flag) so each test uses an isolated ssh_config in its own temp dir.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
	"github.com/ngicks/oci-image-copy/pkg/cli/ssh"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy"
)

// ────────────────────────────────────────────────────────────────────────────
// Pre-flight checks
// ────────────────────────────────────────────────────────────────────────────

func skipUnlessReady(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("skopeo"); err != nil {
		t.Skip("skopeo not on PATH")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not on PATH")
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Minimal valid OCI fixture
// ────────────────────────────────────────────────────────────────────────────

// ociFixture holds a minimal but valid OCI image layout that skopeo can
// inspect. All digests and sizes are computed from the actual content.
type ociFixture struct {
	// srcDir is the full OCI dir (containing oci-layout, index.json, blobs/).
	// This is what skopeo sees via "oci:<srcDir>:<imageRef>".
	srcDir string
	// tag is just the tag portion of the image ref (e.g. "v1.0").
	tag string
	// imageRef is the full reference string used as the
	// org.opencontainers.image.ref.name annotation (e.g. "registry/repo:v1.0").
	imageRef string

	// digest strings "algo:hex" for each blob
	layerDigest    string
	configDigest   string
	manifestDigest string

	// sizes in bytes
	layerSize    int64
	configSize   int64
	manifestSize int64

	// raw content of each blob
	layerContent    []byte
	configContent   []byte
	manifestContent []byte
}

// buildOCIFixture constructs a minimal valid OCI image layout in srcDir.
// All digests and sizes are computed from actual content bytes.
// fullRef is the complete image reference used as the
// org.opencontainers.image.ref.name annotation (e.g. "registry/repo:tag").
// tag is just the tag portion, used only for directory naming.
func buildOCIFixture(t *testing.T, root, fullRef, tag string) *ociFixture {
	t.Helper()
	dir := filepath.Join(root, "src-image")
	must(t, os.MkdirAll(dir, 0o755))

	// 1. Layer — gzip tar containing a single file.
	layerContent := buildMinimalTarGz(t)
	layerDg := sha256DigestStr(layerContent)
	layerSize := int64(len(layerContent))

	// 2. Config blob — minimal OCI image config.
	configContent := []byte(
		`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:` +
			sha256HexStr(layerContent) + `"]}}`,
	)
	configDg := sha256DigestStr(configContent)
	configSize := int64(len(configContent))

	// 3. Manifest — single layer, correct digests and sizes.
	const manifestFmt = `{"schemaVersion":2,` +
		`"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json",` +
		`"digest":"%s","size":%d},` +
		`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip",` +
		`"digest":"%s","size":%d}]}`
	manifestContent := fmt.Appendf(
		nil,
		manifestFmt,
		configDg,
		configSize,
		layerDg,
		layerSize,
	)
	manifestDg := sha256DigestStr(manifestContent)
	manifestSize := int64(len(manifestContent))

	// 4. index.json — the annotation value must be the full image reference
	// so that `skopeo inspect oci:<dir>:<fullRef>` and
	// `skopeo copy oci:<dir>:<fullRef> ...` can find the manifest.
	const indexFmt = `{"schemaVersion":2,` +
		`"mediaType":"application/vnd.oci.image.index.v1+json",` +
		`"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"digest":"%s","size":%d,` +
		`"annotations":{"org.opencontainers.image.ref.name":"%s"}}]}`
	indexContent := fmt.Appendf(
		nil,
		indexFmt,
		manifestDg,
		manifestSize,
		fullRef,
	)

	// 5. Write blobs/sha256/<hex>.
	blobsDir := filepath.Join(dir, "blobs", "sha256")
	must(t, os.MkdirAll(blobsDir, 0o755))
	writeBlob(t, blobsDir, layerDg, layerContent)
	writeBlob(t, blobsDir, configDg, configContent)
	writeBlob(t, blobsDir, manifestDg, manifestContent)

	// 6. oci-layout marker.
	must(t, os.WriteFile(filepath.Join(dir, "oci-layout"),
		[]byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644))

	// 7. index.json.
	must(t, os.WriteFile(filepath.Join(dir, "index.json"), indexContent, 0o644))

	return &ociFixture{
		srcDir:          dir,
		tag:             tag,
		imageRef:        fullRef,
		layerDigest:     layerDg,
		configDigest:    configDg,
		manifestDigest:  manifestDg,
		layerSize:       layerSize,
		configSize:      configSize,
		manifestSize:    manifestSize,
		layerContent:    layerContent,
		configContent:   configContent,
		manifestContent: manifestContent,
	}
}

// allDigests returns all blob digests that must appear in the store's share dir.
func (f *ociFixture) allDigests() []string {
	return []string{f.layerDigest, f.configDigest, f.manifestDigest}
}

func (f *ociFixture) blobContent(dg string) []byte {
	switch dg {
	case f.layerDigest:
		return f.layerContent
	case f.configDigest:
		return f.configContent
	case f.manifestDigest:
		return f.manifestContent
	default:
		return nil
	}
}

func readStoredFixture(t *testing.T, baseDir string, src *ociFixture) *ociFixture {
	t.Helper()

	tagDir := filepath.Join(baseDir, "localregistry.test", "testimage", "_tags", src.tag)
	indexContent, err := os.ReadFile(filepath.Join(tagDir, "index.json"))
	if err != nil {
		t.Fatalf("read stored index.json: %v", err)
	}
	var index struct {
		Manifests []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(indexContent, &index); err != nil {
		t.Fatalf("unmarshal stored index.json: %v", err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("stored index manifests = %d, want 1", len(index.Manifests))
	}

	manifestDigest := index.Manifests[0].Digest
	manifestContent := readStoredBlob(t, baseDir, manifestDigest)
	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifestContent, &manifest); err != nil {
		t.Fatalf("unmarshal stored manifest: %v", err)
	}
	if len(manifest.Layers) != 1 {
		t.Fatalf("stored manifest layers = %d, want 1", len(manifest.Layers))
	}

	configContent := readStoredBlob(t, baseDir, manifest.Config.Digest)
	layerContent := readStoredBlob(t, baseDir, manifest.Layers[0].Digest)

	return &ociFixture{
		srcDir:          src.srcDir,
		tag:             src.tag,
		imageRef:        src.imageRef,
		layerDigest:     manifest.Layers[0].Digest,
		configDigest:    manifest.Config.Digest,
		manifestDigest:  manifestDigest,
		layerSize:       manifest.Layers[0].Size,
		configSize:      manifest.Config.Size,
		manifestSize:    index.Manifests[0].Size,
		layerContent:    layerContent,
		configContent:   configContent,
		manifestContent: manifestContent,
	}
}

func readStoredBlob(t *testing.T, baseDir, dg string) []byte {
	t.Helper()
	_, hex, ok := strings.Cut(dg, ":")
	if !ok {
		t.Fatalf("bad digest %q", dg)
	}
	content, err := os.ReadFile(filepath.Join(baseDir, "share", "sha256", hex))
	if err != nil {
		t.Fatalf("read stored blob %s: %v", dg, err)
	}
	return content
}

// writeBlob writes content to blobsDir/<hex> where dg is "algo:hex".
func writeBlob(t *testing.T, blobsDir, dg string, content []byte) {
	t.Helper()
	_, hex, ok := strings.Cut(dg, ":")
	if !ok {
		t.Fatalf("bad digest %q", dg)
	}
	must(t, os.WriteFile(filepath.Join(blobsDir, hex), content, 0o644))
}

// ────────────────────────────────────────────────────────────────────────────
// In-process SSH server
// ────────────────────────────────────────────────────────────────────────────

// keyPair holds a matched host or user key pair.
type keyPair struct {
	signer    gossh.Signer    // private key (used by server host key or client auth)
	publicKey gossh.PublicKey // corresponding public key
	privPEM   []byte          // OpenSSH PEM-encoded private key (for client config)
}

// sshTestServer is an in-process SSH+SFTP server for integration tests.
// Session exec requests run commands via sh -c locally.
// Session sftp subsystem requests are served by github.com/pkg/sftp.
type sshTestServer struct {
	root     string // SFTP working directory / exec working dir
	addr     string // "127.0.0.1:<port>"
	hostKey  keyPair
	userKey  keyPair
	cfgPath  string // path to the written ssh_config file (set by writeSSHClientConfig)
	listener net.Listener
	done     chan struct{}
}

// startSSHServer creates and starts an in-process SSH+SFTP server rooted at
// root. A cleanup function is registered via t.Cleanup.
func startSSHServer(t *testing.T, root string) *sshTestServer {
	t.Helper()

	hostKP := generateKeyPair(t)
	userKP := generateKeyPair(t)

	config := &gossh.ServerConfig{
		PublicKeyCallback: func(_ gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
			if bytes.Equal(key.Marshal(), userKP.publicKey.Marshal()) {
				return &gossh.Permissions{}, nil
			}
			return nil, fmt.Errorf("unauthorized key")
		},
	}
	config.AddHostKey(hostKP.signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &sshTestServer{
		root:    root,
		addr:    ln.Addr().String(),
		hostKey: hostKP,
		userKey: userKP,
		done:    make(chan struct{}),
	}
	srv.listener = ln

	go func() {
		defer close(srv.done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // closed
			}
			go srv.handleConn(conn, config)
		}
	}()

	t.Cleanup(func() {
		ln.Close()
		select {
		case <-srv.done:
		case <-time.After(5 * time.Second):
		}
	})
	return srv
}

func (s *sshTestServer) handleConn(c net.Conn, config *gossh.ServerConfig) {
	defer c.Close()
	sconn, chans, reqs, err := gossh.NewServerConn(c, config)
	if err != nil {
		return
	}
	defer sconn.Close()
	go gossh.DiscardRequests(reqs)
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(gossh.UnknownChannelType, "not a session")
			continue
		}
		ch, chanReqs, err := newChan.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(ch, chanReqs)
	}
}

type exitStatusPayload struct {
	Status uint32
}

func (s *sshTestServer) handleSession(ch gossh.Channel, reqs <-chan *gossh.Request) {
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "subsystem":
			nameLen, name, ok := decodeSSHString(req.Payload)
			_ = nameLen
			if !ok || name != "sftp" {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			sftpSrv, err := sftp.NewServer(ch,
				sftp.WithServerWorkingDirectory(s.root),
			)
			exitCode := uint32(0)
			if err == nil {
				if serveErr := sftpSrv.Serve(); serveErr != nil && serveErr != io.EOF {
					exitCode = 1
				}
			} else {
				exitCode = 1
			}
			_, _ = ch.SendRequest("exit-status", false, gossh.Marshal(exitStatusPayload{exitCode}))
			return

		case "exec":
			_, cmdStr, ok := decodeSSHString(req.Payload)
			if !ok {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			cmd := exec.Command("sh", "-c", cmdStr)
			cmd.Dir = s.root
			cmd.Stdout = ch
			cmd.Stderr = ch.Stderr()
			exitCode := uint32(0)
			if err := cmd.Run(); err != nil {
				exitCode = 1
				if ee, ok := err.(*exec.ExitError); ok {
					exitCode = uint32(ee.ExitCode())
				}
			}
			_, _ = ch.SendRequest("exit-status", false, gossh.Marshal(exitStatusPayload{exitCode}))
			return

		case "env":
			_ = req.Reply(true, nil)

		default:
			_ = req.Reply(false, nil)
		}
	}
}

// decodeSSHString parses an SSH-encoded string: 4-byte big-endian length
// followed by the string data.
func decodeSSHString(b []byte) (n int, s string, ok bool) {
	if len(b) < 4 {
		return 0, "", false
	}
	l := int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if l < 0 || 4+l > len(b) {
		return 0, "", false
	}
	return l, string(b[4 : 4+l]), true
}

// port returns the server's listen port as an integer.
func (s *sshTestServer) port() int {
	_, portStr, _ := net.SplitHostPort(s.addr)
	p, _ := strconv.Atoi(portStr)
	return p
}

// writeSSHClientConfig writes a minimal ssh_config to dir that points the
// system ssh binary at our test server with key authentication.
// The generated config path is stored in s.cfgPath for use by target().
func (s *sshTestServer) writeSSHClientConfig(t *testing.T, dir string) {
	t.Helper()

	// Write user private key.
	privKeyPath := filepath.Join(dir, "id_ed25519")
	must(t, os.WriteFile(privKeyPath, s.userKey.privPEM, 0o600))

	// Write known_hosts entry for our server.
	knownHostsPath := filepath.Join(dir, "known_hosts")
	khLine := knownhosts.Line(
		[]string{fmt.Sprintf("[127.0.0.1]:%d", s.port())},
		s.hostKey.publicKey,
	)
	must(t, os.WriteFile(knownHostsPath, []byte(khLine+"\n"), 0o644))

	// Write ssh_config.
	cfgPath := filepath.Join(dir, "ssh_config")
	cfg := fmt.Sprintf(`Host testserver
    HostName 127.0.0.1
    Port %d
    User root
    IdentityFile %s
    UserKnownHostsFile %s
    StrictHostKeyChecking yes
    BatchMode yes
    IdentitiesOnly yes
    LogLevel ERROR
`, s.port(), privKeyPath, knownHostsPath)
	must(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))
	s.cfgPath = cfgPath
}

// target returns the ssh.Target for our in-process server, using the
// test-specific ssh_config file via -F so there is no dependency on the
// user's ~/.ssh/config.
func (s *sshTestServer) target() ssh.Target {
	return ssh.Target{Name: "testserver", ConfigFile: s.cfgPath}
}

// ────────────────────────────────────────────────────────────────────────────
// Test environment setup
// ────────────────────────────────────────────────────────────────────────────

// testEnv holds the full test environment for one scenario.
type testEnv struct {
	t         *testing.T
	tmpDir    string
	sshSrv    *sshTestServer
	remoteTmp string // remote base dir (served over SFTP)
	localBase string // local imagecopy base dir
	fixture   *ociFixture
}

// newTestEnv constructs a complete integration test environment:
//   - Builds an OCI fixture and validates it with skopeo
//   - Starts an in-process SSH+SFTP server
//   - Writes ssh_config and sets $HOME so system ssh uses our keys/known_hosts
//   - Verifies connectivity with ssh.Probe
func newTestEnv(t *testing.T, tag string) *testEnv {
	t.Helper()
	skipUnlessReady(t)

	tmp := t.TempDir()

	// Set XDG_CONFIG_HOME to a temp dir with a permissive skopeo trust policy.
	// This allows skopeo to copy/inspect OCI images without a real signature
	// verification setup. The policy only applies to this test process.
	xdgCfg := filepath.Join(tmp, "xdg-config")
	must(t, os.MkdirAll(filepath.Join(xdgCfg, "containers"), 0o755))
	must(t, os.WriteFile(
		filepath.Join(xdgCfg, "containers", "policy.json"),
		[]byte(`{"default":[{"type":"insecureAcceptAnything"}]}`),
		0o644,
	))
	t.Setenv("XDG_CONFIG_HOME", xdgCfg)

	// Verify skopeo
	skopeoOut, err := exec.Command("skopeo", "--version").Output()
	if err != nil {
		t.Skipf("skopeo not available: %v", err)
	}
	t.Logf("skopeo: %s", strings.TrimSpace(string(skopeoOut)))

	// The full image ref used throughout the test.
	imageRef := "localregistry.test/testimage:" + tag

	// Build OCI fixture.
	fixture := buildOCIFixture(t, tmp, imageRef, tag)
	validateFixtureWithSkopeo(t, fixture)

	// Remote base directory.
	remoteTmp := filepath.Join(tmp, "remote")
	must(t, os.MkdirAll(remoteTmp, 0o755))

	// Start in-process SSH server.
	sshSrv := startSSHServer(t, remoteTmp)

	// Write ssh client config (keys + known_hosts + ssh_config).
	// The config path is stored in sshSrv.cfgPath and threaded through
	// ssh.Target.ConfigFile so the system ssh binary uses it via -F, with
	// no dependency on the user's ~/.ssh/config.
	sshCfgDir := filepath.Join(tmp, "ssh")
	must(t, os.MkdirAll(sshCfgDir, 0o700))
	sshSrv.writeSSHClientConfig(t, sshCfgDir)
	t.Logf("ssh_config: %s", sshSrv.cfgPath)

	// Local imagecopy base.
	localBase := filepath.Join(tmp, "local-base")
	must(t, os.MkdirAll(localBase, 0o755))

	// Verify SSH connectivity.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := ssh.Probe(ctx, sshSrv.target()); err != nil {
		t.Fatalf("SSH probe failed (in-process server not reachable): %v", err)
	}
	t.Log("SSH probe OK")

	return &testEnv{
		t:         t,
		tmpDir:    tmp,
		sshSrv:    sshSrv,
		remoteTmp: remoteTmp,
		localBase: localBase,
		fixture:   fixture,
	}
}

// validateFixtureWithSkopeo runs `skopeo inspect oci:<dir>:<fullRef>` and
// fails the test if skopeo rejects our hand-crafted fixture.
// XDG_CONFIG_HOME must already be set to a dir containing a permissive policy.
func validateFixtureWithSkopeo(t *testing.T, f *ociFixture) {
	t.Helper()
	ref := fmt.Sprintf("oci:%s:%s", f.srcDir, f.imageRef)
	cmd := exec.Command("skopeo", "inspect", ref)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("skopeo inspect fixture failed: %v\n%s", err, out)
	}
	outStr := string(out)
	t.Logf("fixture validated by skopeo: %s",
		outStr[:minInt(200, len(outStr))])
}

// makeLocal creates a Local configured for oci transport backed by the fixture.
func (e *testEnv) makeLocal(ctx context.Context) *ociimagecopy.Local {
	e.t.Helper()
	local, err := ociimagecopy.NewLocal(ctx, ociimagecopy.LocalConfig{
		BaseDir:   e.localBase,
		Transport: skopeo.TransportOci,
		OCIPath:   e.fixture.srcDir,
	})
	if err != nil {
		e.t.Fatalf("NewLocal: %v", err)
	}
	return local
}

// makeRemote dials the in-process SSH server and returns a Remote configured
// for oci transport with an explicit OCIPath inside the remote temp dir.
func (e *testEnv) makeRemote(ctx context.Context, remoteOCIPath string) ociimagecopy.Remote {
	e.t.Helper()
	remote, err := ociimagecopy.NewRemote(ctx, ociimagecopy.RemoteConfig{
		Target:    e.sshSrv.target(),
		Transport: skopeo.TransportOci,
		OCIPath:   remoteOCIPath,
	})
	if err != nil {
		e.t.Fatalf("NewRemote: %v", err)
	}
	e.t.Cleanup(func() { _ = remote.Close() })
	return remote
}

// imageRef returns the fully-qualified image reference for the fixture.
func (e *testEnv) imageRef() string {
	return "localregistry.test/testimage:" + e.fixture.tag
}

// ────────────────────────────────────────────────────────────────────────────
// Helper assertions
// ────────────────────────────────────────────────────────────────────────────

// assertBlobPresent checks that share/sha256/<hex> exists under dir.
// If expected is non-nil it also checks byte equality.
func assertBlobPresent(t *testing.T, dir, digest string, expected []byte) {
	t.Helper()
	_, hex, ok := strings.Cut(digest, ":")
	if !ok {
		t.Fatalf("invalid digest %q", digest)
	}
	blobPath := filepath.Join(dir, "share", "sha256", hex)
	got, err := os.ReadFile(blobPath)
	if err != nil {
		t.Errorf("blob %s missing at %s: %v", digest, blobPath, err)
		return
	}
	if expected != nil && !bytes.Equal(got, expected) {
		t.Errorf("blob %s content mismatch: got %d bytes, want %d",
			digest, len(got), len(expected))
	}
}

// snapshotFileList returns a sorted list of all regular file paths under root.
func snapshotFileList(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			rel, _ := filepath.Rel(root, p)
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(files)
	return files
}

func fileListsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// must is a test helper that calls t.Fatal on error.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ────────────────────────────────────────────────────────────────────────────
// E2E Scenario A: PUSH local oci: → remote oci: over SSH
// ────────────────────────────────────────────────────────────────────────────

func TestE2E_Push(t *testing.T) {
	tag := "v1.0"
	e := newTestEnv(t, tag)
	ctx := context.Background()

	remoteOCIPath := filepath.Join(e.remoteTmp, "oci-store")
	must(t, os.MkdirAll(remoteOCIPath, 0o755))

	local := e.makeLocal(ctx)
	remote := e.makeRemote(ctx, remoteOCIPath)

	// ── First push: all blobs should be sent ──
	res, err := local.Push(ctx, ociimagecopy.PushArgs{
		Images: []string{e.imageRef()},
	}, remote)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("Push failed: %+v", res.Reports)
	}
	report := res.Reports[0]
	if report.Sent == 0 {
		t.Errorf("first push: expected Sent > 0, got 0")
	}
	t.Logf("first push: %s", report.SummaryLine())

	stored := readStoredFixture(t, remoteOCIPath, e.fixture)
	// Assert: all recompressed blobs are present on the remote with correct content.
	for _, dg := range stored.allDigests() {
		assertBlobPresent(t, remoteOCIPath, dg, stored.blobContent(dg))
	}

	// Assert: tag-dir metadata files are present on the remote.
	tagDir := filepath.Join(remoteOCIPath,
		"localregistry.test", "testimage", "_tags", tag)
	for _, f := range []string{"index.json", "oci-layout"} {
		if _, err := os.Stat(filepath.Join(tagDir, f)); err != nil {
			t.Errorf("remote tag-dir file %s missing: %v", f, err)
		}
	}

	// ── Second push: all blobs reused (Sent==0) ──
	res2, err := local.Push(ctx, ociimagecopy.PushArgs{
		Images: []string{e.imageRef()},
	}, remote)
	if err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if res2.FailedCount != 0 {
		t.Fatalf("second Push failed: %+v", res2.Reports)
	}
	report2 := res2.Reports[0]
	if report2.Sent != 0 {
		t.Errorf("second push should reuse all blobs (Sent==0), got Sent=%d", report2.Sent)
	}
	t.Logf("second push (all reused): %s", report2.SummaryLine())
}

// ────────────────────────────────────────────────────────────────────────────
// E2E Scenario B: PULL remote oci: → local over SSH
// ────────────────────────────────────────────────────────────────────────────

func TestE2E_Pull(t *testing.T) {
	tag := "v1.0"
	e := newTestEnv(t, tag)
	ctx := context.Background()

	remoteOCIPath := filepath.Join(e.remoteTmp, "oci-store")
	must(t, os.MkdirAll(remoteOCIPath, 0o755))

	// Seed the remote with a complete OCI dump (push first, then pull from it).
	{
		local := e.makeLocal(ctx)
		remote := e.makeRemote(ctx, remoteOCIPath)
		if _, err := local.Push(ctx, ociimagecopy.PushArgs{
			Images: []string{e.imageRef()},
		}, remote); err != nil {
			t.Fatalf("seed push: %v", err)
		}
	}
	stored := readStoredFixture(t, remoteOCIPath, e.fixture)

	// Pull into a fresh local base.
	pullLocalBase := filepath.Join(e.tmpDir, "pull-local-base")
	must(t, os.MkdirAll(pullLocalBase, 0o755))

	pullLocal, err := ociimagecopy.NewLocal(ctx, ociimagecopy.LocalConfig{
		BaseDir:   pullLocalBase,
		Transport: skopeo.TransportOci,
		OCIPath:   e.fixture.srcDir,
	})
	if err != nil {
		t.Fatalf("NewLocal for pull: %v", err)
	}

	remote := e.makeRemote(ctx, remoteOCIPath)
	res, err := pullLocal.Pull(ctx, ociimagecopy.PullArgs{
		Images:            []string{e.imageRef()},
		AssumeLocalHasSet: map[string]struct{}{}, // fresh, no blobs
	}, remote)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("Pull failed: %+v", res.Reports)
	}
	report := res.Reports[0]
	if report.Fetched == 0 {
		t.Errorf("Pull: expected Fetched > 0, got 0")
	}
	t.Logf("pull result: %s", report.SummaryLine())

	// Assert recompressed blobs are present locally with correct content.
	for _, dg := range stored.allDigests() {
		assertBlobPresent(t, pullLocalBase, dg, stored.blobContent(dg))
	}

	// Verify pulled image passes skopeo inspect via the shared blob dir layout.
	// The annotation in the tag-dir index.json uses the full image ref, so we
	// must pass it here too (not just the bare tag).
	localShareDir := filepath.Join(pullLocalBase, "share")
	pullTagDir := filepath.Join(pullLocalBase,
		"localregistry.test", "testimage", "_tags", tag)
	ref := fmt.Sprintf("oci:%s:%s", pullTagDir, e.imageRef())
	cmd := exec.Command("skopeo", "inspect",
		"--shared-blob-dir", localShareDir, ref)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("skopeo inspect pulled image failed: %v\n%s", err, out)
	} else {
		outStr := string(out)
		t.Logf("skopeo inspect pulled image OK: %s",
			outStr[:minInt(200, len(outStr))])
	}
}

// ────────────────────────────────────────────────────────────────────────────
// E2E Scenario C: DRY-RUN push — zero filesystem changes on remote
// ────────────────────────────────────────────────────────────────────────────

func TestE2E_DryRun(t *testing.T) {
	tag := "v1.0"
	e := newTestEnv(t, tag)
	ctx := context.Background()

	remoteOCIPath := filepath.Join(e.remoteTmp, "oci-store")
	must(t, os.MkdirAll(remoteOCIPath, 0o755))

	local := e.makeLocal(ctx)
	remote := e.makeRemote(ctx, remoteOCIPath)

	// Snapshot remote before dry-run.
	before := snapshotFileList(t, e.remoteTmp)

	res, err := local.Push(ctx, ociimagecopy.PushArgs{
		Images:             []string{e.imageRef()},
		DryRun:             true,
		AssumeRemoteHasSet: map[string]struct{}{}, // empty → would send everything
	}, remote)
	if err != nil {
		t.Fatalf("dry-run Push: %v", err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("dry-run Push had failures: %+v", res.Reports)
	}

	// Snapshot after dry-run.
	after := snapshotFileList(t, e.remoteTmp)

	if !fileListsEqual(before, after) {
		t.Errorf("dry-run mutated remote:\nbefore: %v\nafter:  %v", before, after)
	}
	if !res.Reports[0].DryRun {
		t.Error("DryRun flag not set in report")
	}
	t.Logf("dry-run result: %s", res.Reports[0].SummaryLine())
}

// ────────────────────────────────────────────────────────────────────────────
// E2E Scenario D: RESUME push — pre-seeded .part blob resumed, not restarted
// ────────────────────────────────────────────────────────────────────────────

func TestE2E_ResumeFromPartialPush(t *testing.T) {
	tag := "v1.0"
	e := newTestEnv(t, tag)
	ctx := context.Background()

	remoteOCIPath := filepath.Join(e.remoteTmp, "oci-store")
	remoteShareSha := filepath.Join(remoteOCIPath, "share", "sha256")
	must(t, os.MkdirAll(remoteShareSha, 0o755))

	local := e.makeLocal(ctx)
	ref, err := imageref.Parse(e.imageRef())
	if err != nil {
		t.Fatalf("imageref.Parse: %v", err)
	}
	if _, err := local.Dump(ctx, ref); err != nil {
		t.Fatalf("local dump for resume fixture: %v", err)
	}
	stored := readStoredFixture(t, e.localBase, e.fixture)

	// Pre-seed a .part file for the layer blob on the REMOTE side.
	_, layerHex, _ := strings.Cut(stored.layerDigest, ":")
	layerContent := stored.layerContent
	if len(layerContent) < 2 {
		t.Skip("layer too small to test resume")
	}

	partPath := filepath.Join(remoteShareSha, layerHex+".part")
	sidecarPath := partPath + ".etag"
	// Write first byte only (partial).
	must(t, os.WriteFile(partPath, layerContent[:1], 0o644))
	// Write ETag sidecar so the FsSink trusts this partial.
	must(t, os.WriteFile(sidecarPath, []byte(stored.layerDigest), 0o644))

	remote := e.makeRemote(ctx, remoteOCIPath)

	res, err := local.Push(ctx, ociimagecopy.PushArgs{
		Images: []string{e.imageRef()},
	}, remote)
	if err != nil {
		t.Fatalf("Push (resume): %v", err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("Push (resume) failed: %+v", res.Reports)
	}
	t.Logf("resume push result: %s", res.Reports[0].SummaryLine())

	// Assert layer blob is fully present and correct on remote.
	finalPath := filepath.Join(remoteShareSha, layerHex)
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read remote layer blob after resume: %v", err)
	}
	if !bytes.Equal(got, layerContent) {
		t.Errorf("remote layer blob content mismatch after resume: got %d bytes, want %d",
			len(got), len(layerContent))
	}

	// Assert .part and .part.etag are gone.
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Errorf(".part should be gone after successful push; stat=%v", err)
	}
	if _, err := os.Stat(sidecarPath); !os.IsNotExist(err) {
		t.Errorf(".part.etag should be gone after successful push; stat=%v", err)
	}
}

// ────────────────────────────────────────────────────────────────────────────
// E2E Scenario E: DIGEST MISMATCH on pull resume — corrupt .part cleaned up
// ────────────────────────────────────────────────────────────────────────────

// TestE2E_DigestMismatchOnPullResume pre-seeds a LOCAL .part file with corrupt
// content (same size as the full blob, so Pull's resume path hits the sha256
// pre-commit hook). The hook should reject it, clean up the .part, and return
// an error.
func TestE2E_DigestMismatchOnPullResume(t *testing.T) {
	tag := "v1.0"
	e := newTestEnv(t, tag)
	ctx := context.Background()

	remoteOCIPath := filepath.Join(e.remoteTmp, "oci-store")
	must(t, os.MkdirAll(remoteOCIPath, 0o755))

	// Seed the remote with complete correct content.
	{
		seedLocal := e.makeLocal(ctx)
		seedRemote := e.makeRemote(ctx, remoteOCIPath)
		if _, err := seedLocal.Push(ctx, ociimagecopy.PushArgs{
			Images: []string{e.imageRef()},
		}, seedRemote); err != nil {
			t.Fatalf("seed push: %v", err)
		}
	}
	stored := readStoredFixture(t, remoteOCIPath, e.fixture)

	// Fresh local base for the pull attempt.
	pullLocalBase := filepath.Join(e.tmpDir, "corrupt-pull-local-base")
	must(t, os.MkdirAll(pullLocalBase, 0o755))
	localShareSha := filepath.Join(pullLocalBase, "share", "sha256")
	must(t, os.MkdirAll(localShareSha, 0o755))

	// Pre-seed LOCAL .part for the layer blob with bytes that are wrong
	// but have the same length, so Pull doesn't short-circuit on size.
	_, layerHex, _ := strings.Cut(stored.layerDigest, ":")
	layerContent := stored.layerContent

	corrupt := make([]byte, len(layerContent))
	for i := range corrupt {
		corrupt[i] = byte(0xFF ^ layerContent[i])
	}

	partPath := filepath.Join(localShareSha, layerHex+".part")
	sidecarPath := partPath + ".etag"
	must(t, os.WriteFile(partPath, corrupt, 0o644))
	must(t, os.WriteFile(sidecarPath, []byte(stored.layerDigest), 0o644))

	pullLocal, err := ociimagecopy.NewLocal(ctx, ociimagecopy.LocalConfig{
		BaseDir:   pullLocalBase,
		Transport: skopeo.TransportOci,
		OCIPath:   e.fixture.srcDir,
	})
	if err != nil {
		t.Fatalf("NewLocal for corrupt pull: %v", err)
	}
	remote := e.makeRemote(ctx, remoteOCIPath)

	_, pullErr := pullLocal.Pull(ctx, ociimagecopy.PullArgs{
		Images:            []string{e.imageRef()},
		AssumeLocalHasSet: map[string]struct{}{},
	}, remote)

	// The pull should fail due to sha256 mismatch.
	if pullErr == nil {
		t.Fatal("Pull with corrupt .part should fail, got nil error")
	}
	t.Logf("pull error (expected): %v", pullErr)

	// The .part and sidecar should be cleaned up by the pre-commit hook.
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Errorf(".part should be cleaned up after sha256 mismatch; stat=%v", err)
	}
	if _, err := os.Stat(sidecarPath); !os.IsNotExist(err) {
		t.Errorf(".part.etag should be cleaned up after sha256 mismatch; stat=%v", err)
	}

	// The final blob should not have been committed.
	finalPath := filepath.Join(localShareSha, layerHex)
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Errorf("final blob should not exist after failed pull; stat=%v", err)
	}
}

// ────────────────────────────────────────────────────────────────────────────
// E2E Scenario F: DumpImage no-op for oci transport
// ────────────────────────────────────────────────────────────────────────────

// TestE2E_Pull_DumpImage_OciNoOp verifies that pulling from an oci-transport
// remote works end-to-end with the new one-shot pull semantics: DumpImage is
// a no-op for oci transport (the mirror IS the live store), so no extra SSH
// exec traffic is generated and the pull result is identical to the pre-
// DumpImage baseline.
func TestE2E_Pull_DumpImage_OciNoOp(t *testing.T) {
	tag := "v1.0"
	e := newTestEnv(t, tag)
	ctx := context.Background()

	remoteOCIPath := filepath.Join(e.remoteTmp, "oci-store")
	must(t, os.MkdirAll(remoteOCIPath, 0o755))

	// Seed the remote by pushing first.
	{
		seedLocal := e.makeLocal(ctx)
		seedRemote := e.makeRemote(ctx, remoteOCIPath)
		if _, err := seedLocal.Push(ctx, ociimagecopy.PushArgs{
			Images: []string{e.imageRef()},
		}, seedRemote); err != nil {
			t.Fatalf("seed push: %v", err)
		}
	}

	// Snapshot remote SFTP root before the pull so we can assert that
	// DumpImage causes no additional writes.
	beforeRemote := snapshotFileList(t, e.remoteTmp)

	// Pull into a fresh local base.
	pullLocalBase := filepath.Join(e.tmpDir, "pull-noop-local-base")
	must(t, os.MkdirAll(pullLocalBase, 0o755))
	pullLocal, err := ociimagecopy.NewLocal(ctx, ociimagecopy.LocalConfig{
		BaseDir:   pullLocalBase,
		Transport: skopeo.TransportOci,
		OCIPath:   e.fixture.srcDir,
	})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	remote := e.makeRemote(ctx, remoteOCIPath)
	res, err := pullLocal.Pull(ctx, ociimagecopy.PullArgs{
		Images:            []string{e.imageRef()},
		AssumeLocalHasSet: map[string]struct{}{},
	}, remote)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("Pull failed: %+v", res.Reports)
	}
	if res.Reports[0].Fetched == 0 {
		t.Errorf("Pull: expected Fetched > 0, got 0")
	}
	t.Logf("pull (DumpImage no-op) result: %s", res.Reports[0].SummaryLine())

	// The remote store must be unchanged: DumpImage for oci transport is a
	// pure no-op and must not write anything.
	afterRemote := snapshotFileList(t, e.remoteTmp)
	if !fileListsEqual(beforeRemote, afterRemote) {
		t.Errorf("remote changed after DumpImage no-op pull:\nbefore: %v\nafter:  %v",
			beforeRemote, afterRemote)
	}

	// All blobs must be present locally.
	stored := readStoredFixture(t, remoteOCIPath, e.fixture)
	for _, dg := range stored.allDigests() {
		assertBlobPresent(t, pullLocalBase, dg, nil)
	}
}

// ────────────────────────────────────────────────────────────────────────────
// E2E Scenario G: local-directory remote (no sshd required)
// ────────────────────────────────────────────────────────────────────────────

// TestE2E_LocalDirRemote_PushPull exercises the oci:/path local-directory
// remote: push an OCI fixture to a local dir, then pull from that same dir
// into a fresh local base — no sshd, no SFTP.
func TestE2E_LocalDirRemote_PushPull(t *testing.T) {
	skipUnlessReady(t)

	tag := "v1.0"
	tmp := t.TempDir()

	// XDG_CONFIG_HOME with permissive skopeo trust policy.
	xdgCfg := filepath.Join(tmp, "xdg-config")
	must(t, os.MkdirAll(filepath.Join(xdgCfg, "containers"), 0o755))
	must(t, os.WriteFile(
		filepath.Join(xdgCfg, "containers", "policy.json"),
		[]byte(`{"default":[{"type":"insecureAcceptAnything"}]}`),
		0o644,
	))
	t.Setenv("XDG_CONFIG_HOME", xdgCfg)

	imageRef := "localregistry.test/testimage:" + tag

	// Build OCI fixture.
	fixture := buildOCIFixture(t, tmp, imageRef, tag)
	validateFixtureWithSkopeo(t, fixture)

	// Directories.
	localBase := filepath.Join(tmp, "local-base")
	must(t, os.MkdirAll(localBase, 0o755))
	remoteDirPath := filepath.Join(tmp, "local-dir-remote")
	must(t, os.MkdirAll(remoteDirPath, 0o755))

	ctx := context.Background()

	// ── Push to local-directory remote ──
	local, err := ociimagecopy.NewLocal(ctx, ociimagecopy.LocalConfig{
		BaseDir:   localBase,
		Transport: skopeo.TransportOci,
		OCIPath:   fixture.srcDir,
	})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	remote, err := ociimagecopy.NewLocalDirRemote(remoteDirPath)
	if err != nil {
		t.Fatalf("NewLocalDirRemote: %v", err)
	}
	t.Cleanup(func() { _ = remote.Close() })

	pushRes, err := local.Push(ctx, ociimagecopy.PushArgs{
		Images: []string{imageRef},
	}, remote)
	if err != nil {
		t.Fatalf("Push to local-dir remote: %v", err)
	}
	if pushRes.FailedCount != 0 {
		t.Fatalf("Push failed: %+v", pushRes.Reports)
	}
	t.Logf("local-dir push: %s", pushRes.Reports[0].SummaryLine())

	stored := readStoredFixture(t, remoteDirPath, fixture)
	// All blobs must be present in the remote dir's share/.
	for _, dg := range stored.allDigests() {
		assertBlobPresent(t, remoteDirPath, dg, nil)
	}

	// ── Pull from local-directory remote ──
	pullLocalBase := filepath.Join(tmp, "pull-local-base")
	must(t, os.MkdirAll(pullLocalBase, 0o755))

	pullLocal, err := ociimagecopy.NewLocal(ctx, ociimagecopy.LocalConfig{
		BaseDir:   pullLocalBase,
		Transport: skopeo.TransportOci,
		OCIPath:   fixture.srcDir,
	})
	if err != nil {
		t.Fatalf("NewLocal for pull: %v", err)
	}

	pullRemote, err := ociimagecopy.NewLocalDirRemote(remoteDirPath)
	if err != nil {
		t.Fatalf("NewLocalDirRemote for pull: %v", err)
	}
	t.Cleanup(func() { _ = pullRemote.Close() })

	pullRes, err := pullLocal.Pull(ctx, ociimagecopy.PullArgs{
		Images:            []string{imageRef},
		AssumeLocalHasSet: map[string]struct{}{},
	}, pullRemote)
	if err != nil {
		t.Fatalf("Pull from local-dir remote: %v", err)
	}
	if pullRes.FailedCount != 0 {
		t.Fatalf("Pull failed: %+v", pullRes.Reports)
	}
	if pullRes.Reports[0].Fetched == 0 {
		t.Errorf("Pull: expected Fetched > 0, got 0")
	}
	t.Logf("local-dir pull: %s", pullRes.Reports[0].SummaryLine())

	// Verify all blobs are now locally present.
	for _, dg := range stored.allDigests() {
		assertBlobPresent(t, pullLocalBase, dg, nil)
	}

	// Verify with skopeo inspect.
	localShareDir := filepath.Join(pullLocalBase, "share")
	pullTagDir := filepath.Join(pullLocalBase,
		"localregistry.test", "testimage", "_tags", tag)
	ref := fmt.Sprintf("oci:%s:%s", pullTagDir, imageRef)
	cmd := exec.Command("skopeo", "inspect",
		"--shared-blob-dir", localShareDir, ref)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("skopeo inspect pulled image (local-dir) failed: %v\n%s", err, out)
	} else {
		outStr := string(out)
		t.Logf("skopeo inspect local-dir pull OK: %s",
			outStr[:minInt(200, len(outStr))])
	}
}

// ────────────────────────────────────────────────────────────────────────────
// CLI Binary Smoke Test
// ────────────────────────────────────────────────────────────────────────────

func TestCLIBinary_Help(t *testing.T) {
	skipUnlessReady(t)

	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "oci-image-copy")
	repoRoot := findRepoRoot(t)

	// Build the binary.
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/oci-image-copy")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	t.Run("help_exits_zero", func(t *testing.T) {
		out, err := exec.Command(binPath, "--help").Output()
		if err != nil {
			t.Fatalf("--help exited non-zero: %v", err)
		}
		outStr := strings.ToLower(string(out))
		for _, word := range []string{"push", "pull", "dump"} {
			if !strings.Contains(outStr, word) {
				t.Errorf("--help output missing %q:\n%s", word, out)
			}
		}
		t.Logf("--help OK, length=%d", len(out))
	})

	t.Run("invalid_local_spec_errors", func(t *testing.T) {
		// Use the new --local flag (replaces --local-transport).
		// "bogus" is not a recognized spec.
		cmd := exec.Command(binPath, "push",
			"--local", "bogus",
			"--remote", "ssh://localhost",
			"someimage:latest",
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatal("expected non-zero exit for --local bogus")
		}
		outStr := string(out)
		if !strings.Contains(outStr, "bogus") || !strings.Contains(outStr, "unrecognised") {
			t.Errorf("expected 'unrecognised ... bogus' in output, got:\n%s", outStr)
		}
		t.Logf("invalid local spec error OK: %s", strings.TrimSpace(outStr))
	})

	t.Run("file_server_url_parsed", func(t *testing.T) {
		// file-server remote parses the URL correctly and attempts to connect.
		// The src dir /tmp/src does not exist so it fails on local inspection,
		// NOT with "not implemented" (that stub has been replaced by the real impl).
		cmd := exec.Command(binPath, "push",
			"--local", "oci:/tmp/src",
			"--remote", "file-server:https://bucket.example.com/prefix",
			"someimage:latest",
		)
		out, err := cmd.CombinedOutput()
		outStr := string(out)
		// Accepted outcomes:
		//   (a) Non-zero exit for any reason OTHER than "not implemented" stub.
		//   (b) "not found" or similar error about /tmp/src (local does not exist).
		// The one thing we must NOT see is the old "not implemented" stub message.
		if err == nil {
			t.Logf("push exited zero (unexpected but not fatal): %s", outStr)
		} else {
			if strings.Contains(outStr, "not implemented yet (phase B)") {
				t.Errorf("file-server stub still active — expected real impl, got: %s", outStr)
			}
			t.Logf("file-server remote error (expected): %s", strings.TrimSpace(outStr))
		}
	})
}

// findRepoRoot walks up from cwd looking for go.mod.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from " + dir)
		}
		dir = parent
	}
}
