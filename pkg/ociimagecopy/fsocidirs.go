package ociimagecopy

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"

	"github.com/ngicks/go-fsys-helper/fsutil"
	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
	"github.com/opencontainers/go-digest"
)

// DefaultRemoteParallelism is the default upload concurrency for the
// SSH-backed [Remote] (matches `podman image pull --pull=missing`'s
// default of 3 simultaneous transfers).
const DefaultRemoteParallelism = 3

// FsOciDirs is a [StoreV1] backed by a [vroot.Fs[vroot.File]] rooted at the
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

var _ StoreV1 = (*FsOciDirs)(nil)

// Stat implements [BlobStore.Stat]. It reports how much of the blob is present
// in share/<algo>/<hex> by reusing [fsutil.FsSink.State] against the share
// path:
//
//   - committed final file present → complete (CurrentSize == Size);
//   - only a .part file with fewer bytes than the total → partial;
//   - neither present (and no usable partial) → fs.ErrNotExist.
//
// A full-size (or oversize) but UNCOMMITTED .part is deliberately reported as
// absent rather than as a partial: [fsutil.FsSink.State] returns
// {Offset: partSize, Complete: false} for such a file (Complete is true only
// once the committed final file exists), so reporting CurrentSize == size would
// wrongly satisfy the "complete ⟺ CurrentSize == Size" skip test and let
// unverified bytes be reused. Treating it as absent routes the caller back
// through the transfer, whose sha256 pre-commit hook validates (and on mismatch
// discards) the full-size .part — the data-integrity backstop the resume path
// depends on.
func (d *FsOciDirs) Stat(
	ctx context.Context,
	dgst digest.Digest,
	size int64,
) (BlobInfo, error) {
	blobPath, err := d.blobPath(dgst)
	if err != nil {
		return BlobInfo{}, fmt.Errorf("stat: %w", err)
	}
	sink := fsutil.NewFsSink[vroot.Fs[vroot.File], vroot.File](d.fs, blobPath, 0o644)
	st, err := sink.State(ctx)
	if err != nil {
		return BlobInfo{}, fmt.Errorf("stat %s: %w", dgst, err)
	}
	if st.Complete {
		return BlobInfo{CurrentSize: st.Offset, Size: size}, nil
	}
	if st.Offset > 0 && st.Offset < size {
		return BlobInfo{CurrentSize: st.Offset, Size: size}, nil
	}
	return BlobInfo{}, fmt.Errorf("stat %s: %w", dgst, fs.ErrNotExist)
}

// PrepDownload implements [BlobStore.PrepDownload]. It returns a
// [fsutil.ResumableSource] that reads from share/<algo>/<hex> using the
// configured filesystem. The ETag is set to dgst.String() so the resumable
// machinery can track identity across interrupted transfers.
func (d *FsOciDirs) PrepDownload(
	ctx context.Context,
	dgst digest.Digest,
	size int64,
) (fsutil.ResumableSource, error) {
	_ = ctx
	blobPath, err := d.blobPath(dgst)
	if err != nil {
		return nil, fmt.Errorf("prep-download: %w", err)
	}
	return fsutil.NewFsSource[vroot.Fs[vroot.File], vroot.File](d.fs, blobPath, dgst.String()), nil
}

// PrepUpload implements [BlobStore.PrepUpload]. It ensures the parent
// directory (share/<algo>/) exists, then returns a [fsutil.ResumableSink] that
// writes into share/<algo>/<hex> using .part + sidecar + atomic-rename
// semantics. The parent-dir creation is folded in here because the fsutil
// Push primitive does not create parent directories.
func (d *FsOciDirs) PrepUpload(
	ctx context.Context,
	dgst digest.Digest,
	size int64,
) (fsutil.ResumableSink, error) {
	_ = ctx
	algo, hex, err := ocidir.SplitDigest(string(dgst))
	if err != nil {
		return nil, fmt.Errorf("prep-upload: %w", err)
	}
	if err := d.fs.MkdirAll(path.Join(RelSharePath(), algo), 0o755); err != nil {
		return nil, fmt.Errorf("prep-upload: mkdir blob parent: %w", err)
	}
	blobPath := path.Join(RelSharePath(), algo, hex)
	return fsutil.NewFsSink[vroot.Fs[vroot.File], vroot.File](d.fs, blobPath, 0o644), nil
}

// GetIndex implements [TagStoreV1.GetIndex], returning the verbatim
// index.json bytes from ref's dump dir. The not-exist error from ReadFile
// already wraps [fs.ErrNotExist].
func (d *FsOciDirs) GetIndex(ctx context.Context, ref imageref.ImageRef) ([]byte, error) {
	_ = ctx
	rel, err := RelDumpDir(ref)
	if err != nil {
		return nil, err
	}
	return vroot.ReadFile(d.fs, path.Join(rel, "index.json"))
}

// GetOciLayout implements [TagStoreV1.GetOciLayout], returning the verbatim
// oci-layout bytes from ref's dump dir. The not-exist error from ReadFile
// already wraps [fs.ErrNotExist].
func (d *FsOciDirs) GetOciLayout(ctx context.Context, ref imageref.ImageRef) ([]byte, error) {
	_ = ctx
	rel, err := RelDumpDir(ref)
	if err != nil {
		return nil, err
	}
	return vroot.ReadFile(d.fs, path.Join(rel, "oci-layout"))
}

// PutIndex implements [TagStoreV1.PutIndex], writing the verbatim index.json
// bytes into ref's dump dir.
func (d *FsOciDirs) PutIndex(ctx context.Context, ref imageref.ImageRef, raw []byte) error {
	return d.putTagFile(ctx, ref, "index.json", raw)
}

// PutOciLayout implements [TagStoreV1.PutOciLayout], writing the verbatim
// oci-layout bytes into ref's dump dir.
func (d *FsOciDirs) PutOciLayout(ctx context.Context, ref imageref.ImageRef, raw []byte) error {
	return d.putTagFile(ctx, ref, "oci-layout", raw)
}

// putTagFile writes a single small per-image metadata file (index.json /
// oci-layout) under ref's dump dir via [fsutil.SafeWrite] (tmp + atomic
// rename), creating the dump dir if needed.
func (d *FsOciDirs) putTagFile(
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

// blobPath returns the FS-relative share/<algo>/<hex> path for dgst.
func (d *FsOciDirs) blobPath(dgst digest.Digest) (string, error) {
	algo, hex, err := ocidir.SplitDigest(string(dgst))
	if err != nil {
		return "", err
	}
	return path.Join(RelSharePath(), algo, hex), nil
}
