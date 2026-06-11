package imagecopy

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/oci-image-copy/pkg/cli"
	"github.com/ngicks/oci-image-copy/pkg/cli/docker"
	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
)

// localTransportRef builds the skopeo source [skopeo.TransportRef] for the
// given transport, ociPath, and image ref.
//
// For containers-storage and docker-daemon, the ref is represented as
// "transport:ref.String()" — the standard form those transports accept.
// For oci, the form is "oci:<ociPath>:<ref.String()>", which skopeo requires.
// Passing an empty ociPath with TransportOci is invalid and should be caught by
// [NewLocal] before reaching here.
func localTransportRef(
	transport skopeo.Transport,
	ociPath string,
	ref imageref.ImageRef,
) skopeo.TransportRef {
	if transport == skopeo.TransportOci {
		return skopeo.TransportRef{
			Transport: skopeo.TransportOci,
			Arg1:      ociPath,
			Arg2:      ref.String(),
		}
	}
	return skopeo.TransportRef{
		Transport: transport,
		Arg1:      ref.String(),
	}
}

// DumpArgv is the single source of truth for the skopeo copy argument
// triple used when dumping a live-storage image into an OCI mirror.
// It returns:
//
//   - src: the transport ref for the live-storage source (e.g.
//     containers-storage:<ref> or docker-daemon:<ref>). Callers that
//     need to dump FROM an oci: source must build their own src ref via
//     [localTransportRef] because that case requires an extra path arg.
//   - dst: the oci: destination ref (oci:<tagDirAbs>:<ref>).
//   - sharedBlobDir: the absolute path to the shared blob pool.
//
// Both [Local.Dump] and [sshRemote.DumpImage] call this function so that
// their skopeo argv constructions cannot diverge.
func DumpArgv(
	transport skopeo.Transport,
	ref imageref.ImageRef,
	tagDirAbs string,
	shareAbs string,
) (src skopeo.TransportRef, dst skopeo.TransportRef, sharedBlobDir string) {
	src = skopeo.TransportRef{Transport: transport, Arg1: ref.String()}
	dst = skopeo.TransportRef{
		Transport: skopeo.TransportOci,
		Arg1:      tagDirAbs,
		Arg2:      ref.String(),
	}
	sharedBlobDir = shareAbs
	return src, dst, sharedBlobDir
}

// Local is the local-side push/pull endpoint. It owns the resolved
// base data dir, the local skopeo / podman / docker wrappers, and an
// [vroot.Fs[vroot.File]] rooted at BaseDir. Build via [NewLocal].
//
// [Local.Push] and [Local.Pull] drive a transfer against any [Remote]
// (typically the SSH-backed implementation from [NewRemote]).
type Local struct {
	baseDir   string
	transport skopeo.Transport
	ociPath   string

	skopeoCli SkopeoLike
	lister    Lister
	fs        vroot.Fs[vroot.File]
	dirs      *FsOciDirs

	validateOnce sync.Once
	validateErr  error
}

// DefaultLocalParallelism is the default upload concurrency for the
// local-side [OciDirs] (used when sourcing blobs into the local mirror
// during pull).
const DefaultLocalParallelism = 4

// LocalConfig configures [NewLocal].
//
//   - BaseDir is optional; an empty value falls back to [DefaultBaseDir].
//   - Transport is required: one of [skopeo.TransportContainersStorage],
//     [skopeo.TransportDockerDaemon], [skopeo.TransportOci], or
//     [skopeo.TransportDocker]. The docker (registry) transport can only act
//     as a dump/push source: it has no lister, so [Local.List] (needed by
//     pull) returns an error for it.
//   - OCIPath is required when Transport == [skopeo.TransportOci].
type LocalConfig struct {
	BaseDir   string
	Transport skopeo.Transport
	OCIPath   string
}

// SupportedLocalTransports is the set of transports accepted by [NewLocal].
// [skopeo.TransportDocker] is dump/push-source only (see [LocalConfig]).
var SupportedLocalTransports = []skopeo.Transport{
	skopeo.TransportContainersStorage,
	skopeo.TransportDockerDaemon,
	skopeo.TransportOci,
	skopeo.TransportDocker,
}

// NewLocal resolves BaseDir, ensures the on-disk layout, and builds
// the local skopeo wrapper plus an [vroot.Fs[vroot.File]] rooted at BaseDir. A
// transport-appropriate lister (podman / docker) is wired up
// automatically.
func NewLocal(ctx context.Context, cfg LocalConfig) (*Local, error) {
	if cfg.Transport == "" {
		return nil, errors.New("local: transport unset")
	}
	switch cfg.Transport {
	case skopeo.TransportContainersStorage,
		skopeo.TransportDockerDaemon,
		skopeo.TransportOci,
		skopeo.TransportDocker:
		// ok; docker (registry) has no lister and is dump/push-source only.
	default:
		const transports = "containers-storage, docker-daemon, oci, docker"
		return nil, fmt.Errorf(
			"local: unsupported transport %q (want one of: %s)",
			cfg.Transport,
			transports,
		)
	}
	base := cfg.BaseDir
	if base == "" {
		var err error
		base, err = DefaultBaseDir()
		if err != nil {
			return nil, err
		}
	}
	if err := NewStore(base).EnsureLayout(ctx); err != nil {
		return nil, fmt.Errorf("local: ensure layout: %w", err)
	}
	fs, err := NewOsFs(base)
	if err != nil {
		return nil, fmt.Errorf("local: %w", err)
	}
	l := &Local{
		baseDir:   base,
		transport: cfg.Transport,
		ociPath:   cfg.OCIPath,
		skopeoCli: &skopeo.Skopeo{Invoker: cli.NewLocalInvoker()},
		fs:        fs,
		dirs:      NewFsOciDirs(fs, DefaultLocalParallelism),
	}
	switch cfg.Transport {
	case skopeo.TransportContainersStorage:
		l.lister = docker.NewPodman(cli.NewLocalInvoker())
	case skopeo.TransportDockerDaemon:
		l.lister = docker.NewDocker(cli.NewLocalInvoker())
	}
	return l, nil
}

// BaseDir returns the resolved local data dir.
func (l *Local) BaseDir() string { return l.baseDir }

// Transport returns the canonical local transport.
func (l *Local) Transport() skopeo.Transport { return l.transport }

// OCIPath returns the configured `oci:<dir>` path (only meaningful
// when Transport == [skopeo.TransportOci]).
func (l *Local) OCIPath() string { return l.ociPath }

// Skopeo returns the local skopeo wrapper.
func (l *Local) Skopeo() SkopeoLike { return l.skopeoCli }

// FS returns the local [vroot.Fs[vroot.File]] rooted at BaseDir.
func (l *Local) FS() vroot.Fs[vroot.File] { return l.fs }

// Dir returns the local [OciDirs] view (the multi-image OCI store
// rooted at BaseDir).
func (l *Local) Dir() OciDirs { return l.dirs }

// Lister returns the local docker / podman wrapper, or nil for
// [skopeo.TransportOci].
func (l *Local) Lister() Lister { return l.lister }

// Validate runs sanity checks against the local environment — at the
// moment, that the local skopeo binary is present and runnable.
// Cached after the first invocation; safe to call from every entry
// point.
func (l *Local) Validate(ctx context.Context) error {
	l.validateOnce.Do(func() {
		if _, err := l.skopeoCli.Version(ctx); err != nil {
			l.validateErr = fmt.Errorf("local skopeo: %w", err)
		}
	})
	return l.validateErr
}

// Dump runs `skopeo copy <Transport>:<ref> oci:<store-tag-dir>`,
// staging ref into the local store layout (per-tag dump dir + the
// shared blob pool under BaseDir/share). Returns the absolute tag
// directory.
func (l *Local) Dump(ctx context.Context, ref imageref.ImageRef) (string, error) {
	if err := l.Validate(ctx); err != nil {
		return "", err
	}
	store := NewStore(l.baseDir)
	tagDirAbs, err := store.DumpDir(ref)
	if err != nil {
		return "", err
	}
	tagDirRel, err := RelDumpDir(ref)
	if err != nil {
		return "", err
	}
	if err := l.fs.MkdirAll(tagDirRel, 0o755); err != nil {
		return "", fmt.Errorf("dump: mkdir %s: %w", tagDirRel, err)
	}
	src, dst, sharedBlobDir := DumpArgv(l.transport, ref, tagDirAbs, store.ShareDir())
	// For the oci: source transport, the src ref needs the path arg that
	// DumpArgv does not set (DumpArgv is only used for live transports).
	// Override src with the full oci ref when needed.
	if l.transport == skopeo.TransportOci {
		src = localTransportRef(l.transport, l.ociPath, ref)
	}
	if err := l.skopeoCli.Copy(ctx, src, dst, sharedBlobDir); err != nil {
		return "", fmt.Errorf("dump: skopeo copy: %w", err)
	}
	return tagDirAbs, nil
}

// List returns the digest set of every blob reachable from this
// local's images, plus the share/ inventory.
func (l *Local) List(ctx context.Context) (map[string]struct{}, error) {
	return listAt(ctx, l.transport, l.skopeoCli, l.fs, l.baseDir, l.lister)
}

// LoadImage runs `skopeo copy oci:<dump-dir> <local-transport>:<ref>`,
// loading ref's content from the local OCI mirror into the local live
// storage (containers-storage / docker-daemon). No-op when local
// transport is oci.
func (l *Local) LoadImage(ctx context.Context, ref imageref.ImageRef) error {
	if l.transport == skopeo.TransportOci {
		return nil
	}
	if err := l.Validate(ctx); err != nil {
		return err
	}
	store := NewStore(l.baseDir)
	tagDirAbs, err := store.DumpDir(ref)
	if err != nil {
		return err
	}
	if err := l.skopeoCli.Copy(ctx,
		skopeo.TransportRef{Transport: skopeo.TransportOci, Arg1: tagDirAbs, Arg2: ref.String()},
		skopeo.TransportRef{Transport: l.transport, Arg1: ref.String()},
		store.ShareDir(),
	); err != nil {
		return fmt.Errorf("local: load image %s: %w", ref.String(), err)
	}
	return nil
}
