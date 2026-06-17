package remote

// localdir.go implements the local-directory Remote variant
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
// DefaultRemoteParallelism. ListBlobsByImage shares the [listBlobsByImageFromMirror]
// helper that sshRemote also uses; InspectImage mirrors the sshRemote
// oci-transport branch (read the raw manifest blob from the mirror). LoadImage
// and DumpImage are no-ops (this transport IS the store — identical to
// sshRemote's oci: behaviour).

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"iter"

	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy"
	"github.com/opencontainers/go-digest"
)

// Compile-time check: [*localDirRemote] satisfies [ociimagecopy.Remote].
var _ ociimagecopy.Remote = (*localDirRemote)(nil)

// localDirRemote is a [ociimagecopy.Remote] backed by a locally-reachable
// directory. It constructs the same [ociimagecopy.FsOciDirs] the SSH remote
// uses but over an osfs-based [vroot.Fs] rooted at the configured path.
type localDirRemote struct {
	baseDir string
	fs      vroot.Fs[vroot.File]
	dirs    *ociimagecopy.FsOciDirs
}

// NewLocalDir creates a local-directory [ociimagecopy.Remote] rooted at baseDir.
// The directory does not need to exist at construction time; the first
// write will create it via the store layout helpers.
//
// Use case: an external process stages blobs into a pool at baseDir;
// oci-image-copy is then used against this remote to normalize or
// recompress images uniformly without a network hop.
func NewLocalDir(baseDir string) (ociimagecopy.Remote, error) {
	fsys, err := ociimagecopy.NewOsFs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("local-dir remote: %w", err)
	}
	return &localDirRemote{
		baseDir: baseDir,
		fs:      fsys,
		dirs:    ociimagecopy.NewFsOciDirs(fsys, ociimagecopy.DefaultRemoteParallelism),
	}, nil
}

// Close is a no-op; the OS filesystem has no resources to release.
func (r *localDirRemote) Close() error { return nil }

// ReadOnly always returns false; the local-directory remote is writable.
func (r *localDirRemote) ReadOnly() bool { return false }

// Blobs returns the [ociimagecopy.BlobStore] view over the local directory.
func (r *localDirRemote) Blobs() ociimagecopy.BlobStore { return r.dirs }

// Tags returns the [ociimagecopy.TagStoreV1] view over the local directory.
func (r *localDirRemote) Tags() ociimagecopy.TagStoreV1 { return r.dirs }

// ListBlobsByImage implements [ociimagecopy.Remote]: reads ref's manifest
// closure from the mirror and yields each blob digest. Shares the
// implementation with sshRemote via [listBlobsByImageFromMirror].
func (r *localDirRemote) ListBlobsByImage(
	ctx context.Context,
	ref imageref.ImageRef,
) iter.Seq2[digest.Digest, error] {
	return listBlobsByImageFromMirror(ctx, r.dirs, ref)
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
	_, data, err := ocidir.ReadRawManifest(ctx, ociimagecopy.NewImageView(ctx, r.dirs, r.dirs, ref))
	if err != nil {
		return nil, fmt.Errorf("local-dir remote: inspect %s: %w", ref.String(), err)
	}
	return data, nil
}

// listBlobsByImageFromMirror is the shared fs-backed [ociimagecopy.Remote.ListBlobsByImage]
// implementation for the SSH and local-directory remotes. It reads ref's
// manifest closure from the mirror via [ocidir.ReadManifest] over a
// [ociimagecopy.NewImageView] and yields each digest from
// [ocidir.AllDescriptors]. An absent image — a manifest read error that
// satisfies errors.Is(err, fs.ErrNotExist) OR
// errors.Is(err, ocidir.ErrMissingManifestBlob) — yields nothing; any other
// error is yielded once.
func listBlobsByImageFromMirror(
	ctx context.Context,
	dirs *ociimagecopy.FsOciDirs,
	ref imageref.ImageRef,
) iter.Seq2[digest.Digest, error] {
	return func(yield func(digest.Digest, error) bool) {
		mDesc, man, err := ocidir.ReadManifest(ctx, ociimagecopy.NewImageView(ctx, dirs, dirs, ref))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, ocidir.ErrMissingManifestBlob) {
				return
			}
			yield(digest.Digest(""), err)
			return
		}
		for _, d := range ocidir.AllDescriptors(mDesc, man) {
			if !yield(d.Digest, nil) {
				return
			}
		}
	}
}
