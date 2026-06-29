// Package remote holds the concrete [ociimagecopy.Remote] implementations:
// the SSH+SFTP-backed remote ([NewSSH]), the local-directory remote
// ([NewLocalDir]), and the HTTP file-server remote ([NewFileServer] /
// [NewFileServerFromSpec]).
//
// The [ociimagecopy.Remote] / [ociimagecopy.BlobStore] / [ociimagecopy.TagStoreV1]
// abstractions and the shared OCI-store helpers live in package ociimagecopy;
// the --remote spec types and parser live in this package (no import cycle:
// this package depends on ociimagecopy and never the other way around).
package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ngicks/go-fsys-helper/vroot"
	sftpfsadapter "github.com/ngicks/go-fsys-helper/vroot-adapter/sftpfs"
	"github.com/ngicks/oci-image-copy/pkg/cli"
	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
	"github.com/ngicks/oci-image-copy/pkg/cli/ssh"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/sftp"
)

// SSHConfig configures [NewSSH].
//
//   - Target is the SSH destination (required).
//   - Transport is required: one of [skopeo.TransportContainersStorage],
//     [skopeo.TransportDockerDaemon], or [skopeo.TransportOci].
//     For TransportOci, [ociimagecopy.Remote.LoadImage] is a no-op (the peer
//     has no live storage to load into).
//   - OCIPath is required when Transport == [skopeo.TransportOci];
//     it is the absolute path on the peer where the OCI store lives.
//   - Compression controls skopeo copy destination compression. The zero value
//     passes no compression flags (skopeo's own default); the CLI fills it from
//     config, which defaults to zstd level 20 with forced recompression.
type SSHConfig struct {
	Target      ssh.Target
	Transport   skopeo.Transport
	OCIPath     string
	Compression ociimagecopy.CompressionConfig
}

// Compile-time check: [*sshRemote] satisfies [ociimagecopy.Remote].
var _ ociimagecopy.Remote = (*sshRemote)(nil)

// sshRemote is the SSH+SFTP-backed [ociimagecopy.Remote]. SSH transport is
// delegated entirely to the system ssh binary; auth, host-key verification,
// ProxyCommand etc. flow through the user's ssh config.
type sshRemote struct {
	baseDir   string
	transport skopeo.Transport

	target  ssh.Target
	invoker *cli.SshInvoker

	mu      sync.Mutex
	sftpCmd *exec.Cmd
	sftp    *sftp.Client
	closed  bool
	// closedCh is closed (exactly once) when the graceful Close() path
	// finishes. startWatch selects on this to avoid a spurious forceClose
	// after a clean shutdown.
	closedCh chan struct{}

	cancelWatch context.CancelFunc

	skopeoCli ociimagecopy.SkopeoLike
	fs        vroot.Fs[vroot.File]
	dirs      *ociimagecopy.FsOciDirs
}

// NewSSH spawns `ssh -s sftp`, wires its pipes into a sftp client
// via [sftp.NewClientPipe], starts the force-close goroutine, then
// resolves BaseDir on the remote and builds an FS rooted at BaseDir
// plus the [ociimagecopy.FsOciDirs] store over it (parallelism =
// [ociimagecopy.DefaultRemoteParallelism]).
func NewSSH(ctx context.Context, cfg SSHConfig) (ociimagecopy.Remote, error) {
	if cfg.Transport == "" {
		return nil, errors.New("remote: transport unset")
	}
	var stderrBuf bytes.Buffer
	cmd, stdout, stdin, err := ssh.Subsystem(ctx, cfg.Target, &stderrBuf)
	if err != nil {
		return nil, err
	}
	sftpC, err := sftp.NewClientPipe(stdout, stdin)
	if err != nil {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return nil, fmt.Errorf("sftp: %w: %s", err, stderr)
		}
		return nil, fmt.Errorf("sftp: %w", err)
	}
	r := &sshRemote{
		transport: cfg.Transport,
		target:    cfg.Target,
		invoker:   cli.NewSshInvoker(cfg.Target),
		sftpCmd:   cmd,
		sftp:      sftpC,
		closedCh:  make(chan struct{}),
	}
	r.startWatch(ctx)

	base, err := r.resolveBaseDir(ctx, cfg.OCIPath)
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("remote: resolve base dir: %w", err)
	}
	r.baseDir = base
	_, posixRename := sftpC.HasExtension("posix-rename@openssh.com")
	r.fs = sftpfsadapter.New(sftpC, posixRename, base)
	r.skopeoCli = ociimagecopy.NewSkopeoWithCompression(cli.NewSshInvoker(cfg.Target), cfg.Compression)
	r.dirs = ociimagecopy.NewFsOciDirs(r.fs, ociimagecopy.DefaultRemoteParallelism)
	return r, nil
}

// startWatch installs a goroutine that, when ctx is cancelled, waits
// ForceCloseGrace and then force-closes the underlying SFTP client and
// kills the ssh subprocess — unblocking any pending Read/Write that
// didn't honor the cooperative per-read cancellation.
//
// A cooperative [sshRemote.Close] call cancels the watch context immediately
// (via r.cancelWatch), so after a clean Close the goroutine exits
// without reaching forceClose.
func (r *sshRemote) startWatch(parent context.Context) {
	wctx, cancel := context.WithCancel(parent)
	r.cancelWatch = cancel
	go func() {
		<-wctx.Done()
		// wctx is either the parent context (external cancellation) or
		// has been cancelled by Close().  In either case we give in-flight
		// operations ForceCloseGrace to complete before hard-killing.
		select {
		case <-time.After(ForceCloseGrace):
			r.forceClose()
		case <-r.closedCh:
			// graceful Close() finished; nothing left to do.
		}
	}()
}

// ForceCloseGrace is the grace period before the SSH-backed remote
// hard-closes its SFTP client / ssh subprocess on context cancellation.
var ForceCloseGrace = 2 * time.Second

// Close implements [ociimagecopy.Remote]. It signals the watch goroutine via
// closedCh so that it exits without invoking forceClose, then releases
// resources.
func (r *sshRemote) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	// Signal the watch goroutine that we are doing a graceful close so
	// it does not race us to forceClose.
	if r.closedCh != nil {
		close(r.closedCh)
	}
	if r.cancelWatch != nil {
		r.cancelWatch()
	}
	var firstErr error
	if r.sftp != nil {
		if err := r.sftp.Close(); err != nil {
			firstErr = err
		}
	}
	if r.sftpCmd != nil && r.sftpCmd.Process != nil {
		if err := r.waitCmd(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *sshRemote) forceClose() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	if r.sftp != nil {
		_ = r.sftp.Close()
	}
	if r.sftpCmd != nil && r.sftpCmd.Process != nil {
		_ = r.sftpCmd.Process.Kill()
		_ = r.waitCmd()
	}
}

// waitCmd waits for the ssh subprocess to exit, capping the wait at
// ForceCloseGrace before SIGKILL'ing it. Discards the well-known
// "signal: killed" error that we induce ourselves.
func (r *sshRemote) waitCmd() error {
	done := make(chan error, 1)
	go func() { done <- r.sftpCmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil
		}
		return err
	case <-time.After(ForceCloseGrace):
		_ = r.sftpCmd.Process.Kill()
		<-done
		return nil
	}
}

// runRemote runs argv on the remote by spawning a fresh
// `ssh ... -- <argv>` subprocess and returns the captured stdout.
// argv must be non-empty; argv[0] is the executable.
func (r *sshRemote) runRemote(ctx context.Context, argv []string) ([]byte, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("remote: closed")
	}
	r.mu.Unlock()
	if len(argv) == 0 {
		return nil, errors.New("remote: empty argv")
	}
	return r.invoker.Command(ctx, argv[0], argv[1:]...).Output()
}

// resolveBaseDir returns the on-peer base dir. For transport != oci it
// is `${XDG_DATA_HOME:-$HOME/.local/share}/oci-image-copy`. For
// transport == oci it is the explicit OCIPath.
func (r *sshRemote) resolveBaseDir(ctx context.Context, ociPath string) (string, error) {
	if r.transport == skopeo.TransportOci {
		if ociPath == "" {
			return "", errors.New("remote: oci transport requires OCIPath")
		}
		return ociPath, nil
	}
	out, err := r.runRemote(ctx, []string{
		"sh", "-c",
		`printf %s "${XDG_DATA_HOME:-$HOME/.local/share}/` + ociimagecopy.AppDirName + `"`,
	})
	if err != nil {
		return "", err
	}
	base := strings.TrimSpace(string(out))
	if base == "" {
		return "", errors.New("remote: empty base dir")
	}
	return base, nil
}

// ReadOnly implements [ociimagecopy.Remote]. The SSH-backed remote always
// reports false; build a custom [ociimagecopy.Remote] to surface a read-only
// peer.
func (r *sshRemote) ReadOnly() bool { return false }

// Blobs implements [ociimagecopy.Remote].
func (r *sshRemote) Blobs() ociimagecopy.BlobStore { return r.dirs }

// Tags implements [ociimagecopy.Remote].
func (r *sshRemote) Tags() ociimagecopy.TagStoreV1 { return r.dirs }

// ListBlobsByImage implements [ociimagecopy.Remote]: reads ref's manifest
// closure from the peer's mirror and yields each blob digest. An absent image
// (the manifest read returns [fs.ErrNotExist] or
// [ocidir.ErrMissingManifestBlob]) yields nothing; any other error is yielded
// once.
func (r *sshRemote) ListBlobsByImage(
	ctx context.Context,
	ref imageref.ImageRef,
) iter.Seq2[digest.Digest, error] {
	return listBlobsByImageFromMirror(ctx, r.dirs, ref)
}

// LoadImage implements [ociimagecopy.Remote] by running
// `skopeo copy oci:<dump-dir> <transport>:<ref>` on the peer. No-op when
// transport == oci.
func (r *sshRemote) LoadImage(ctx context.Context, ref imageref.ImageRef) error {
	if r.transport == skopeo.TransportOci {
		return nil
	}
	rel, err := ociimagecopy.RelDumpDir(ref)
	if err != nil {
		return err
	}
	tagDirAbs := filepath.ToSlash(filepath.Join(r.baseDir, filepath.FromSlash(rel)))
	shareAbs := filepath.ToSlash(filepath.Join(r.baseDir, "share"))
	if err := r.skopeoCli.Copy(ctx,
		skopeo.TransportRef{Transport: skopeo.TransportOci, Arg1: tagDirAbs, Arg2: ref.String()},
		skopeo.TransportRef{Transport: r.transport, Arg1: ref.String()},
		shareAbs,
	); err != nil {
		return fmt.Errorf("remote: load image %s: %w", ref.String(), err)
	}
	return nil
}

// DumpImage implements [ociimagecopy.Remote] by running
// `skopeo copy <transport>:<ref> oci:<tagDir>:<ref>` on the peer, depositing
// blobs into the shared pool. No-op (nil) when transport == oci (the mirror IS
// the live store). Returns [ociimagecopy.ErrReadOnly] when the peer is
// read-only.
func (r *sshRemote) DumpImage(ctx context.Context, ref imageref.ImageRef) error {
	if r.transport == skopeo.TransportOci {
		return nil
	}
	if r.ReadOnly() {
		return ociimagecopy.ErrReadOnly
	}
	rel, err := ociimagecopy.RelDumpDir(ref)
	if err != nil {
		return err
	}
	tagDirAbs := filepath.ToSlash(filepath.Join(r.baseDir, filepath.FromSlash(rel)))
	shareAbs := filepath.ToSlash(filepath.Join(r.baseDir, "share"))
	// Ensure the tag-dir parents exist on the peer (skopeo requires the
	// destination OCI dir to exist before writing index.json / oci-layout).
	if err := r.fs.MkdirAll(rel, 0o755); err != nil {
		return fmt.Errorf("remote: dump image %s: mkdir %s: %w", ref.String(), rel, err)
	}
	src, dst, sharedBlobDir := ociimagecopy.DumpArgv(r.transport, ref, tagDirAbs, shareAbs)
	if err := r.skopeoCli.Copy(ctx, src, dst, sharedBlobDir); err != nil {
		return fmt.Errorf("remote: dump image %s: %w", ref.String(), err)
	}
	return nil
}

// InspectImage implements [ociimagecopy.Remote]. For live transports it runs
// `skopeo inspect --raw <transport>:<ref>` on the peer. For oci transport
// it reads the manifest from the mirror via [ocidir.ReadRawManifest] and
// returns the raw manifest bytes.
func (r *sshRemote) InspectImage(ctx context.Context, ref imageref.ImageRef) ([]byte, error) {
	if r.transport == skopeo.TransportOci {
		// Read the raw, digest-verified manifest blob bytes directly from the
		// mirror via the shared ocidir choke point. We must return the raw
		// bytes (not re-marshalled JSON) so the sha256 digest of the returned
		// bytes equals the manifest digest in index.json; ReadRawManifest also
		// enforces the single-manifest contract (no unguarded Manifests[0]).
		_, data, err := ocidir.ReadRawManifest(ctx, ociimagecopy.NewImageView(ctx, r.dirs, r.dirs, ref))
		if err != nil {
			return nil, fmt.Errorf("remote: inspect image %s: %w", ref.String(), err)
		}
		return data, nil
	}
	raw, err := r.skopeoCli.Inspect(ctx,
		skopeo.TransportRef{Transport: r.transport, Arg1: ref.String()},
		true, "",
	)
	if err != nil {
		return nil, fmt.Errorf("remote: inspect image %s: %w", ref.String(), err)
	}
	return raw, nil
}
