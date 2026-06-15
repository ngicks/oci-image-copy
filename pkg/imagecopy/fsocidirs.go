package imagecopy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"

	"github.com/ngicks/go-fsys-helper/fsutil"
	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// DefaultRemoteParallelism is the default upload concurrency for the
// SSH-backed [Remote] (matches `podman image pull --pull=missing`'s
// default of 3 simultaneous transfers).
const DefaultRemoteParallelism = 3

// FsOciDirs is an [OciDirs] backed by a [vroot.Fs[vroot.File]] rooted at the
// [Store] base directory. Per-image dump dirs live under
// `<host>/<repo>/_tags/<tag>` (or `_digests/<hex>`); blobs live in
// the shared pool under `share/<algo>/<hex>`.
type FsOciDirs struct {
	fs vroot.Fs[vroot.File]
	// worker pool concurrency limit (retained for informational purposes;
	// parallelism is now managed by the caller via errgroup)
	limit int
}

// NewFsOciDirs returns an [FsOciDirs] over fs (rooted at the
// [Store] base). parallelism caps concurrent uploads; values ≤ 0 default to 1.
func NewFsOciDirs(fs vroot.Fs[vroot.File], limit int) *FsOciDirs {
	if limit <= 0 {
		limit = 1
	}
	return &FsOciDirs{fs: fs, limit: limit}
}

// Limit returns the configured concurrency limit.
func (d *FsOciDirs) Limit() int { return d.limit }

var _ OciDirs = (*FsOciDirs)(nil)

// Blob implements [OciDirs.Blob], reading from share/<algo>/<hex>.
func (d *FsOciDirs) Blob(
	ctx context.Context,
	dg digest.Digest,
	offset int64,
) (io.ReadCloser, int64, error) {
	_ = ctx
	algo, hex, err := ocidir.SplitDigest(string(dg))
	if err != nil {
		return nil, 0, err
	}
	return ocidir.OpenSeekedBlob(d.fs, path.Join(RelSharePath(), algo, hex), offset)
}

// Image implements [OciDirs.Image]: a tag-dir-scoped [ocidir.DirV1]
// view that reads index.json/oci-layout from ref's dump dir and blobs
// from the shared pool.
func (d *FsOciDirs) Image(ref imageref.ImageRef) ocidir.DirV1 {
	rel, err := RelDumpDir(ref)
	if err != nil {
		return errDirV1{err: fmt.Errorf("ocidir: image ref: %w", err)}
	}
	return sharedDir{fs: d.fs, dumpDir: rel, shareDir: RelSharePath()}
}

// BlobSource implements [OciDirs.BlobSource]. It returns a
// [fsutil.ResumableSource] that reads from share/<algo>/<hex> using
// the configured filesystem. The ETag is set to dgst.String() so the
// resumable machinery can track identity across interrupted transfers.
func (d *FsOciDirs) BlobSource(
	ctx context.Context,
	dgst digest.Digest,
	size int64,
) (fsutil.ResumableSource, error) {
	_ = ctx
	algo, hex, err := ocidir.SplitDigest(string(dgst))
	if err != nil {
		return nil, fmt.Errorf("blob-source: %w", err)
	}
	blobPath := path.Join(RelSharePath(), algo, hex)
	return fsutil.NewFsSource[vroot.Fs[vroot.File], vroot.File](d.fs, blobPath, dgst.String()), nil
}

// BlobSink implements [OciDirs.BlobSink]. It returns a
// [fsutil.ResumableSink] that writes into share/<algo>/<hex> in the
// configured filesystem, using .part + sidecar + atomic-rename semantics.
// The caller must ensure the parent directory (share/<algo>/) exists
// before calling the sink's Append method.
func (d *FsOciDirs) BlobSink(
	ctx context.Context,
	dgst digest.Digest,
	size int64,
) (fsutil.ResumableSink, error) {
	_ = ctx
	algo, hex, err := ocidir.SplitDigest(string(dgst))
	if err != nil {
		return nil, fmt.Errorf("blob-sink: %w", err)
	}
	blobPath := path.Join(RelSharePath(), algo, hex)
	return fsutil.NewFsSink[vroot.Fs[vroot.File], vroot.File](d.fs, blobPath, 0o644), nil
}

// PutTagFile implements [OciDirs.PutTagFile] using [fsutil.SafeWrite]
// (tmp + atomic rename). Used for index.json / oci-layout only.
func (d *FsOciDirs) PutTagFile(
	ctx context.Context,
	ref imageref.ImageRef,
	name string,
	data []byte,
) error {
	_ = ctx
	rel, err := RelDumpDir(ref)
	if err != nil {
		return err
	}
	if err := d.fs.MkdirAll(rel, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", rel, err)
	}
	return safeWriteOpt.Copy(
		d.fs,
		path.Join(rel, name),
		bytes.NewReader(data),
		os.ModePerm,
		nil, // preHooks: none for small metadata files
		nil, // postHooks: none for small metadata files
	)
}

// errDirV1 is a stub [ocidir.DirV1] returned by [FsOciDirs.Image]
// when the ref is malformed; every method surfaces err. This keeps
// Image's signature error-free per the [OciDirs] contract while still
// reporting the ref problem on first use.
type errDirV1 struct{ err error }

func (e errDirV1) Index() (v1.Index, error)             { return v1.Index{}, e.err }
func (e errDirV1) ImageLayout() (v1.ImageLayout, error) { return v1.ImageLayout{}, e.err }
func (e errDirV1) Blob(context.Context, digest.Digest, int64) (io.ReadCloser, int64, error) {
	return nil, 0, e.err
}

// MkdirBlobParent ensures that share/<algo>/ directory exists so that
// BlobSink / Pull can write into it. It is a no-op when the directory
// already exists.
func (d *FsOciDirs) MkdirBlobParent(dgst digest.Digest) error {
	algo, _, err := ocidir.SplitDigest(string(dgst))
	if err != nil {
		return err
	}
	dir := path.Join(RelSharePath(), algo)
	if _, err := d.fs.Stat(dir); err == nil {
		return nil
	} else if !isNotExist(err) {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	return d.fs.MkdirAll(dir, 0o755)
}

func isNotExist(err error) bool {
	return err != nil && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist))
}
