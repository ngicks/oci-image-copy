package imagecopy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"

	"github.com/ngicks/go-common/contextkey"
	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// SkopeoInspector is the [Skopeo] subset used by enumeration. The
// interface lets tests substitute a fake without spinning up a fake
// skopeo on $PATH.
type SkopeoInspector interface {
	Inspect(
		ctx context.Context,
		src skopeo.TransportRef,
		raw bool,
		sharedBlobDir string,
		extraArgs ...string,
	) ([]byte, error)
}

// PodmanLister is the [Podman] subset used by enumeration.
type PodmanLister interface {
	ImageLs(ctx context.Context) ([]string, error)
}

// DockerLister is the [Docker] subset used by enumeration.
type DockerLister interface {
	ImageLs(ctx context.Context) ([]string, error)
}

// EnumerateConfig is the bundle of inputs to [EnumerateConfig.Enumerate].
type EnumerateConfig struct {
	// Transport selects the enumerator. One of
	// [skopeo.TransportContainersStorage], [skopeo.TransportDockerDaemon],
	// or [skopeo.TransportOci].
	Transport skopeo.Transport

	// Skopeo is required for containers-storage / docker-daemon to
	// turn refs into manifests.
	Skopeo SkopeoInspector

	// Podman is required when Transport == containers-storage.
	Podman PodmanLister

	// Docker is required when Transport == docker-daemon.
	Docker DockerLister

	// Fs is the filesystem holding the share/ pool (for all three
	// transports) and, for OCI, the layout root rooted at BaseDir.
	Fs vroot.Fs[vroot.File]

	// BaseDir is the application's data dir on this side
	// (`<base>` from PLAN §3). The enumerator unions
	// `<BaseDir>/share/sha256/*` filenames into the result.
	BaseDir string
}

// Enumerate dispatches to the right enumerator. The result is the
// union described by PLAN §4.2: `manifest digests` + `config digests`
// + `layer digests` reachable from peer refs, plus the filename set of
// `<peer-base>/share/`.
func (cfg EnumerateConfig) Enumerate(ctx context.Context) (map[string]struct{}, error) {
	switch cfg.Transport {
	case skopeo.TransportContainersStorage:
		return enumerateViaSkopeoInspect(ctx, cfg, cfg.Podman, skopeo.TransportContainersStorage)
	case skopeo.TransportDockerDaemon:
		return enumerateViaSkopeoInspect(ctx, cfg, cfg.Docker, skopeo.TransportDockerDaemon)
	case skopeo.TransportOci:
		return enumerateOCI(ctx, cfg)
	default:
		return nil, fmt.Errorf("enumerate: unsupported transport %q", cfg.Transport)
	}
}

// Lister is the minimal interface used to enumerate live image refs
// via the docker / podman CLI. It unifies [PodmanLister] and
// [DockerLister]; concrete types satisfying either also satisfy
// [Lister] structurally.
type Lister interface {
	ImageLs(ctx context.Context) ([]string, error)
}

// listAt dispatches to [EnumerateConfig.Enumerate] using the right lister for transport.
func listAt(
	ctx context.Context,
	transport skopeo.Transport,
	sk SkopeoLike,
	fs vroot.Fs[vroot.File],
	baseDir string,
	lister Lister,
) (map[string]struct{}, error) {
	cfg := EnumerateConfig{
		Transport: transport,
		Skopeo:    sk,
		Fs:        fs,
		BaseDir:   baseDir,
	}
	switch transport {
	case skopeo.TransportContainersStorage:
		cfg.Podman = lister
	case skopeo.TransportDockerDaemon:
		cfg.Docker = lister
	}
	return cfg.Enumerate(ctx)
}

func enumerateViaSkopeoInspect(
	ctx context.Context,
	cfg EnumerateConfig,
	lister Lister,
	transport skopeo.Transport,
) (map[string]struct{}, error) {
	if lister == nil {
		return nil, fmt.Errorf("enumerate %s: missing lister", transport)
	}
	if cfg.Skopeo == nil {
		return nil, fmt.Errorf("enumerate %s: missing skopeo", transport)
	}

	logger := contextkey.ValueSlogLoggerDefault(ctx)
	out := map[string]struct{}{}

	refs, err := lister.ImageLs(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerate %s: list images: %w", transport, err)
	}

	for _, ref := range refs {
		raw, err := cfg.Skopeo.Inspect(ctx, skopeo.TransportRef{
			Transport: transport,
			Arg1:      ref,
		}, true, "")
		if err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "enumerate.inspect.skip",
				slog.String("transport", string(transport)),
				slog.String("ref", ref),
				slog.Any("err", err),
			)
			continue
		}
		man, err := ocidir.ParseManifest(raw)
		if err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "enumerate.parse.skip",
				slog.String("transport", string(transport)),
				slog.String("ref", ref),
				slog.Any("err", err),
			)
			continue
		}
		out[ocidir.DigestBytes(raw)] = struct{}{}
		out[string(man.Config.Digest)] = struct{}{}
		for _, l := range man.Layers {
			out[string(l.Digest)] = struct{}{}
		}
	}

	if cfg.Fs != nil {
		if err := unionShareInventory(out, cfg.Fs, "share"); err != nil {
			return nil, fmt.Errorf("enumerate %s: share/ inventory: %w", transport, err)
		}
	}

	return out, nil
}

// enumerateOCI walks the on-disk layout under the FS's root, runs
// the closure walker on every tag/digest dump found, and unions the
// share pool's filename set.
func enumerateOCI(ctx context.Context, cfg EnumerateConfig) (map[string]struct{}, error) {
	if cfg.Fs == nil {
		return nil, errors.New("enumerate oci: nil Fs")
	}

	out := map[string]struct{}{}

	dumps, err := walkDumpDirs(cfg.Fs, ".")
	if err != nil {
		return nil, fmt.Errorf("enumerate oci: walk: %w", err)
	}

	logger := contextkey.ValueSlogLoggerDefault(ctx)
	for _, d := range dumps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		mDesc, man, err := ocidir.ReadManifest(ctx, sharedDir{
			fs:       cfg.Fs,
			dumpDir:  d,
			shareDir: "share",
		})
		if err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "enumerate.oci.closure.skip",
				slog.String("dump", d),
				slog.Any("err", err),
			)
			continue
		}
		for _, desc := range ocidir.AllDescriptors(mDesc, man) {
			out[string(desc.Digest)] = struct{}{}
		}
	}

	if err := unionShareInventory(out, cfg.Fs, "share"); err != nil {
		return nil, fmt.Errorf("enumerate oci: share/ inventory: %w", err)
	}
	return out, nil
}

// sharedDir is a [ocidir.DirV1] over a single base-rooted [vroot.Fs[vroot.File]]
// with FS-relative dumpDir + shareDir paths. Used by the orchestrator
// because SFTP-backed FSes can't be cheaply sub-rooted; the public
// [ocidir.SharedFsDir] (which composes a [ocidir.DirV1] + a separate
// [vroot.Fs[vroot.File]]) is the right shape when sub-FSes are easy.
type sharedDir struct {
	fs       vroot.Fs[vroot.File]
	dumpDir  string
	shareDir string
}

func (d sharedDir) Index() (v1.Index, error) {
	data, err := vroot.ReadFile(d.fs, path.Join(d.dumpDir, "index.json"))
	if err != nil {
		return v1.Index{}, fmt.Errorf("ocidir: read index.json: %w", err)
	}
	idx, err := ocidir.ParseIndex(data)
	if err != nil {
		return v1.Index{}, fmt.Errorf("ocidir: parse index.json: %w", err)
	}
	return idx, nil
}

func (d sharedDir) ImageLayout() (v1.ImageLayout, error) {
	data, err := vroot.ReadFile(d.fs, path.Join(d.dumpDir, v1.ImageLayoutFile))
	if err != nil {
		return v1.ImageLayout{}, fmt.Errorf("ocidir: read %s: %w", v1.ImageLayoutFile, err)
	}
	l, err := ocidir.ParseImageLayout(data)
	if err != nil {
		return v1.ImageLayout{}, fmt.Errorf("ocidir: parse %s: %w", v1.ImageLayoutFile, err)
	}
	return l, nil
}

func (d sharedDir) Blob(
	ctx context.Context,
	dg digest.Digest,
	offset int64,
) (io.ReadCloser, int64, error) {
	_ = ctx
	algo, hex, err := ocidir.SplitDigest(string(dg))
	if err != nil {
		return nil, 0, err
	}
	file, err := d.fs.OpenFile(path.Join(d.shareDir, algo, hex), os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return nil, 0, os.ErrNotExist
		}
		return nil, 0, err
	}
	fi, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	size := fi.Size()
	if offset < 0 || offset > size {
		file.Close()
		return nil, 0, fmt.Errorf("ocidir: offset %d out of range for blob size %d", offset, size)
	}
	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			file.Close()
			return nil, 0, err
		}
	}
	return file, size, nil
}

// walkDumpDirs walks the FS root listing immediate children, then
// for each non-reserved child recurses looking for `_tags`/`_digests`
// marker dirs. The returned paths are FS-relative leaf dump dirs.
func walkDumpDirs(fsys vroot.Fs[vroot.File], root string) ([]string, error) {
	hosts, err := vroot.ReadDir(fsys, root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, h := range hosts {
		if !h.IsDir() {
			continue
		}
		name := h.Name()
		if name == "share" {
			continue
		}
		var prefix string
		if root == "." || root == "" {
			prefix = name
		} else {
			prefix = path.Join(root, name)
		}
		if err := walkRepoTree(fsys, prefix, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// walkRepoTree recursively descends dir, treating any direct child
// named `_tags` or `_digests` as a marker dir whose own children are
// dump leaves.
func walkRepoTree(fsys vroot.Fs[vroot.File], dir string, out *[]string) error {
	entries, err := vroot.ReadDir(fsys, dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		switch name {
		case "_tags", "_digests":
			markerDir := path.Join(dir, name)
			leaves, err := vroot.ReadDir(fsys, markerDir)
			if err != nil {
				return err
			}
			for _, l := range leaves {
				if l.IsDir() {
					*out = append(*out, path.Join(markerDir, l.Name()))
				}
			}
		default:
			if err := walkRepoTree(fsys, path.Join(dir, name), out); err != nil {
				return err
			}
		}
	}
	return nil
}

// unionShareInventory adds every blob found under shareDir/sha256/ to
// dst. Missing share/ is treated as empty inventory.
func unionShareInventory(dst map[string]struct{}, f vroot.Fs[vroot.File], shareDir string) error {
	algoDir := path.Join(shareDir, "sha256")
	entries, err := vroot.ReadDir(f, algoDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) != digest.SHA256.Size()*2 {
			continue
		}
		dst[digest.SHA256.String()+":"+name] = struct{}{}
	}
	return nil
}
