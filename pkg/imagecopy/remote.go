package imagecopy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os/exec"
	"path"
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
	"github.com/opencontainers/go-digest"
	"github.com/pkg/sftp"
)

// ErrReadOnly is returned by [Remote.LoadImage], [Remote.DumpImage], and the
// write side of [OciDirs] when the peer is read-only.
var ErrReadOnly = errors.New("remote: read-only")

// Remote is an OCI store the orchestrator can read from and (when not
// read-only) write to. The SSH+SFTP-backed implementation returned by
// [NewRemote] satisfies it; custom transports (S3, an HTTP mirror, an
// in-memory test double) plug in by implementing this interface.
//
// Read-only implementations return true from [Remote.ReadOnly].
// Mutating operations on read-only peers return [ErrReadOnly].
type Remote interface {
	// Close releases any subsystem resources (e.g. the ssh+sftp
	// subprocess for [NewRemote]). Safe to call multiple times.
	Close() error

	// ReadOnly reports whether mutating operations targeting this peer
	// should be rejected.
	ReadOnly() bool

	// Dir returns the multi-image OCI store this Remote backs.
	Dir() OciDirs

	// ListBlobs enumerates every content-addressed blob the peer
	// holds: image manifests, image configs, and fs layers across all
	// images stored in this Remote. Order is unspecified.
	ListBlobs(ctx context.Context) iter.Seq2[digest.Digest, error]

	// ListImages enumerates the image refs this Remote hosts. Use
	// Dir().Image(ref) to read each image's per-image OCI layout.
	ListImages(ctx context.Context) iter.Seq2[imageref.ImageRef, error]

	// LoadImage tells the peer to load ref's content from its OCI
	// mirror into its live storage (containers-storage / docker-
	// daemon / etc.). Returns [ErrReadOnly] when the peer is read-
	// only; returns nil (no-op) when the peer has no live storage to
	// load into (e.g., a pure OCI mirror).
	LoadImage(ctx context.Context, ref imageref.ImageRef) error

	// DumpImage materializes ref from the peer's live storage into the
	// peer's content-addressable store (the mirror), so that
	// Dir().Image(ref) and the blob set behind it become readable.
	// It is the inverse of LoadImage.
	//
	//   - Implementations backed by a live storage (containers-storage /
	//     docker-daemon over SSH) run the equivalent of
	//     `skopeo copy <transport>:<ref> oci:<tagDir>` with the shared
	//     blob pool on the peer.
	//   - Implementations whose store IS the live storage (pure oci:
	//     mirrors, S3-like stores) return nil without doing anything.
	//   - Read-only peers return [ErrReadOnly] (the pull orchestrator
	//     treats this as advisory: it logs and proceeds to the mirror
	//     read, which gives the definitive error if the content is
	//     genuinely absent).
	//
	// DumpImage is idempotent per (ref, content): re-dumping an
	// unchanged tag is cheap because skopeo skips blobs already present
	// in the shared pool. It is called for every pulled ref even when
	// the mirror already has the tag, because tags move; the live
	// storage is the source of truth on the peer.
	DumpImage(ctx context.Context, ref imageref.ImageRef) error

	// InspectImage returns the raw manifest bytes for ref as known to
	// the peer's source of truth, without mutating anything. Used by
	// pull --dry-run to compute the transfer plan without dumping on
	// the peer.
	//
	//   - Live-storage implementations run
	//     `skopeo inspect --raw <transport>:<ref>` on the peer.
	//   - Mirror-only implementations read the manifest from the store
	//     directly.
	InspectImage(ctx context.Context, ref imageref.ImageRef) ([]byte, error)
}

// RemoteConfig configures [NewRemote].
//
//   - Target is the SSH destination (required).
//   - Transport is required: one of [skopeo.TransportContainersStorage],
//     [skopeo.TransportDockerDaemon], or [skopeo.TransportOci].
//     For TransportOci, [Remote.LoadImage] is a no-op (the peer has
//     no live storage to load into).
//   - OCIPath is required when Transport == [skopeo.TransportOci];
//     it is the absolute path on the peer where the OCI store lives.
type RemoteConfig struct {
	Target    ssh.Target
	Transport skopeo.Transport
	OCIPath   string
}

// Compile-time check: [*sshRemote] satisfies [Remote].
var _ Remote = (*sshRemote)(nil)

// sshRemote is the SSH+SFTP-backed [Remote]. SSH transport is delegated
// entirely to the system ssh binary; auth, host-key verification,
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

	skopeoCli SkopeoLike
	fs        vroot.Fs[vroot.File]
	dirs      *FsOciDirs
}

// NewRemote spawns `ssh -s sftp`, wires its pipes into a sftp client
// via [sftp.NewClientPipe], starts the force-close goroutine, then
// resolves BaseDir on the remote and builds an FS rooted at BaseDir
// plus the [OciDirs] view over it (parallelism =
// [DefaultRemoteParallelism]).
func NewRemote(ctx context.Context, cfg RemoteConfig) (Remote, error) {
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
	r.skopeoCli = &skopeo.Skopeo{Invoker: cli.NewSshInvoker(cfg.Target)}
	r.dirs = NewFsOciDirs(r.fs, DefaultRemoteParallelism)
	return r, nil
}

// startWatch installs a goroutine that, when ctx is cancelled, waits
// ForceCloseGrace and then force-closes the underlying SFTP client and
// kills the ssh subprocess — unblocking any pending Read/Write that
// didn't honor the cooperative per-read cancellation.
//
// A cooperative [Close] call cancels the watch context immediately
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

// ForceCloseGrace is the grace period before the SSH-backed [Remote]
// hard-closes its SFTP client / ssh subprocess on context cancellation.
var ForceCloseGrace = 2 * time.Second

// Close implements [Remote]. It signals the watch goroutine via closedCh
// so that it exits without invoking forceClose, then releases resources.
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
		var ee *exec.ExitError
		if errors.As(err, &ee) {
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
		`printf %s "${XDG_DATA_HOME:-$HOME/.local/share}/` + AppDirName + `"`,
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

// ReadOnly implements [Remote]. The SSH-backed remote always reports
// false; build a custom [Remote] to surface a read-only peer.
func (r *sshRemote) ReadOnly() bool { return false }

// Dir implements [Remote].
func (r *sshRemote) Dir() OciDirs { return r.dirs }

// ListBlobs implements [Remote]: walks `share/sha256/*` on the peer's
// FS and yields every digest found.
func (r *sshRemote) ListBlobs(ctx context.Context) iter.Seq2[digest.Digest, error] {
	return listBlobsFromFs(ctx, r.fs)
}

// ListImages implements [Remote]: walks the peer's per-image dump
// dirs and yields each parsed [imageref.ImageRef].
func (r *sshRemote) ListImages(ctx context.Context) iter.Seq2[imageref.ImageRef, error] {
	return listImagesFromFs(ctx, r.fs)
}

// LoadImage implements [Remote] by running `skopeo copy oci:<dump-dir>
// <transport>:<ref>` on the peer. No-op when transport == oci.
func (r *sshRemote) LoadImage(ctx context.Context, ref imageref.ImageRef) error {
	if r.transport == skopeo.TransportOci {
		return nil
	}
	rel, err := RelDumpDir(ref)
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

// DumpImage implements [Remote] by running `skopeo copy <transport>:<ref>
// oci:<tagDir>:<ref>` on the peer, depositing blobs into the shared pool.
// No-op (nil) when transport == oci (the mirror IS the live store).
// Returns [ErrReadOnly] when the peer is read-only.
func (r *sshRemote) DumpImage(ctx context.Context, ref imageref.ImageRef) error {
	if r.transport == skopeo.TransportOci {
		return nil
	}
	if r.ReadOnly() {
		return ErrReadOnly
	}
	rel, err := RelDumpDir(ref)
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
	src, dst, sharedBlobDir := DumpArgv(r.transport, ref, tagDirAbs, shareAbs)
	if err := r.skopeoCli.Copy(ctx, src, dst, sharedBlobDir); err != nil {
		return fmt.Errorf("remote: dump image %s: %w", ref.String(), err)
	}
	return nil
}

// InspectImage implements [Remote]. For live transports it runs
// `skopeo inspect --raw <transport>:<ref>` on the peer. For oci transport
// it reads the manifest from the mirror via [ocidir.ReadManifest] and
// returns the raw manifest bytes.
func (r *sshRemote) InspectImage(ctx context.Context, ref imageref.ImageRef) ([]byte, error) {
	if r.transport == skopeo.TransportOci {
		// Read the raw manifest blob bytes directly from the mirror. We must
		// return the raw bytes (not re-marshalled JSON) so the sha256 digest of
		// the returned bytes equals the manifest digest in index.json.
		dir := r.dirs.Image(ref)
		idx, err := dir.Index()
		if err != nil {
			return nil, fmt.Errorf("remote: inspect image %s: read index: %w", ref.String(), err)
		}
		mDesc := idx.Manifests[0]
		if mDesc.Digest == "" {
			return nil, fmt.Errorf("remote: inspect image %s: index has no manifest digest",
				ref.String())
		}
		rc, _, err := r.dirs.Blob(ctx, mDesc.Digest, 0)
		if err != nil {
			return nil, fmt.Errorf("remote: inspect image %s: read manifest blob: %w",
				ref.String(), err)
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, fmt.Errorf("remote: inspect image %s: read manifest: %w", ref.String(), err)
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

// listBlobsFromFs walks fs/share/sha256/* and yields each digest.
func listBlobsFromFs(
	ctx context.Context,
	fsys vroot.Fs[vroot.File],
) iter.Seq2[digest.Digest, error] {
	return func(yield func(digest.Digest, error) bool) {
		algoDir := path.Join(RelSharePath(), "sha256")
		entries, err := vroot.ReadDir(fsys, algoDir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return
			}
			yield(digest.Digest(""), err)
			return
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				yield(digest.Digest(""), err)
				return
			}
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if len(name) != digest.SHA256.Size()*2 {
				continue
			}
			if !yield(digest.Digest(digest.SHA256.String()+":"+name), nil) {
				return
			}
		}
	}
}

// listImagesFromFs walks fs for <host>/<repo>/_tags/<tag> and
// _digests/<hex> dump dirs and yields the parsed [imageref.ImageRef].
func listImagesFromFs(
	ctx context.Context,
	fsys vroot.Fs[vroot.File],
) iter.Seq2[imageref.ImageRef, error] {
	return func(yield func(imageref.ImageRef, error) bool) {
		dumps, err := walkDumpDirs(fsys, ".")
		if err != nil {
			yield(imageref.ImageRef{}, err)
			return
		}
		for _, d := range dumps {
			if err := ctx.Err(); err != nil {
				yield(imageref.ImageRef{}, err)
				return
			}
			ref, err := parseDumpDirRel(d)
			if err != nil {
				if !yield(imageref.ImageRef{}, fmt.Errorf("parse %q: %w", d, err)) {
					return
				}
				continue
			}
			if !yield(ref, nil) {
				return
			}
		}
	}
}

// parseDumpDirRel parses an FS-relative dump-dir path
// `<host>/<repo>/_tags/<tag>` or `<host>/<repo>/_digests/<hex>` into
// the corresponding [imageref.ImageRef].
func parseDumpDirRel(rel string) (imageref.ImageRef, error) {
	if marker, leaf, ok := splitOn(rel, "/_tags/"); ok {
		host, repoPath, ok := strings.Cut(marker, "/")
		if !ok || host == "" || repoPath == "" {
			return imageref.ImageRef{}, fmt.Errorf("missing host/path in %q", rel)
		}
		ref := imageref.ImageRef{Host: host, Path: repoPath, Tag: leaf}
		ref.Original = ref.String()
		return ref, nil
	}
	if marker, leaf, ok := splitOn(rel, "/_digests/"); ok {
		host, repoPath, ok := strings.Cut(marker, "/")
		if !ok || host == "" || repoPath == "" {
			return imageref.ImageRef{}, fmt.Errorf("missing host/path in %q", rel)
		}
		ref := imageref.ImageRef{Host: host, Path: repoPath, Digest: leaf}
		ref.Original = ref.String()
		return ref, nil
	}
	return imageref.ImageRef{}, fmt.Errorf("path has no _tags/_digests marker")
}

// splitOn splits s at sep, returning the (before, after, ok) triple.
// Like [strings.Cut] but for an arbitrary separator.
func splitOn(s, sep string) (before, after string, ok bool) {
	before, after, ok = strings.Cut(s, sep)
	if !ok {
		return "", "", false
	}
	return before, after, true
}
