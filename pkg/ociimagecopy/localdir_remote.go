package ociimagecopy

// localdir_remote.go implements the local-directory Remote variant
// (--remote oci:/path/to/base/dir).
//
// Use case (documented per GOAL3.md / PLAN3-fileserver-remote.md):
//
//   An external process stages blobs into a pool at a local directory (e.g.
//   an NFS mount, a fuse-mounted bucket, or a directory another tool
//   populates). oci-image-copy is then used against that directory to
//   (re)process or normalize images uniformly — e.g. consistent
//   recompression through the same skopeo dump pipeline — without any
//   network hop. The local-directory remote is also the cheapest hermetic
//   test target: no sshd required, no SFTP dial, pure os-filesystem.
//
// Construction mirrors how sshRemote builds its FsOciDirs: create an
// osfs-backed vroot.Fs rooted at the path, wire it into a FsOciDirs with
// DefaultRemoteParallelism. ListBlobs and ListImages reuse the shared
// listBlobsFromFs / listImagesFromFs helpers that sshRemote also uses;
// InspectImage mirrors the sshRemote oci-transport branch (read the raw
// manifest blob from the mirror). LoadImage and DumpImage are no-ops
// (this transport IS the store — identical to sshRemote's oci: behaviour).

import (
	"context"
	"fmt"
	"iter"

	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
	"github.com/opencontainers/go-digest"
)

// Compile-time check: [*localDirRemote] satisfies [Remote].
var _ Remote = (*localDirRemote)(nil)

// localDirRemote is a [Remote] backed by a locally-reachable directory.
// It constructs the same [FsOciDirs] the SSH remote uses but over an
// osfs-based [vroot.Fs] rooted at the configured path.
type localDirRemote struct {
	baseDir string
	fs      vroot.Fs[vroot.File]
	dirs    *FsOciDirs
}

// NewLocalDirRemote creates a local-directory [Remote] rooted at baseDir.
// The directory does not need to exist at construction time; the first
// write will create it via the store layout helpers.
//
// Use case: an external process stages blobs into a pool at baseDir;
// oci-image-copy is then used against this remote to normalize or
// recompress images uniformly without a network hop.
func NewLocalDirRemote(baseDir string) (Remote, error) {
	fsys, err := NewOsFs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("local-dir remote: %w", err)
	}
	return &localDirRemote{
		baseDir: baseDir,
		fs:      fsys,
		dirs:    NewFsOciDirs(fsys, DefaultRemoteParallelism),
	}, nil
}

// Close is a no-op; the OS filesystem has no resources to release.
func (r *localDirRemote) Close() error { return nil }

// ReadOnly always returns false; the local-directory remote is writable.
func (r *localDirRemote) ReadOnly() bool { return false }

// Dir returns the [OciDirs] view over the local directory.
func (r *localDirRemote) Dir() OciDirs { return r.dirs }

// ListBlobs implements [Remote]: walks share/sha256/* and yields each digest.
// Reuses the shared [listBlobsFromFs] helper (same implementation as sshRemote).
func (r *localDirRemote) ListBlobs(ctx context.Context) iter.Seq2[digest.Digest, error] {
	return listBlobsFromFs(ctx, r.fs)
}

// ListImages implements [Remote]: walks the per-image dump dirs and yields
// each parsed [imageref.ImageRef].
// Reuses the shared [listImagesFromFs] helper (same implementation as sshRemote).
func (r *localDirRemote) ListImages(ctx context.Context) iter.Seq2[imageref.ImageRef, error] {
	return listImagesFromFs(ctx, r.fs)
}

// LoadImage is a no-op: for a local-directory remote the directory IS the
// store (analogous to sshRemote with oci transport). There is no separate
// live container runtime to load into.
func (r *localDirRemote) LoadImage(_ context.Context, _ imageref.ImageRef) error { return nil }

// DumpImage is a no-op: for a local-directory remote the directory IS the
// store (analogous to sshRemote with oci transport). There is no separate
// live container runtime to dump from.
func (r *localDirRemote) DumpImage(_ context.Context, _ imageref.ImageRef) error { return nil }

// InspectImage reads the raw manifest blob from the local mirror and returns
// the exact bytes. This mirrors the sshRemote oci-transport branch so that
// sha256(returned bytes) == manifest digest in index.json.
func (r *localDirRemote) InspectImage(ctx context.Context, ref imageref.ImageRef) ([]byte, error) {
	// Read the raw, digest-verified manifest bytes via the shared ocidir
	// choke point: it enforces the single-manifest contract (no unguarded
	// Manifests[0]) and guarantees sha256(returned bytes) == manifest digest.
	_, data, err := ocidir.ReadRawManifest(ctx, r.dirs.Image(ref))
	if err != nil {
		return nil, fmt.Errorf("local-dir remote: inspect %s: %w", ref.String(), err)
	}
	return data, nil
}
