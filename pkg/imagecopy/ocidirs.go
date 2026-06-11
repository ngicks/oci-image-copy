package imagecopy

import (
	"context"
	"io"

	"github.com/ngicks/go-fsys-helper/fsutil"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
	"github.com/opencontainers/go-digest"
)

// OciDirs is a multi-image OCI store. It dispatches per-image reads
// via [OciDirs.Image] (returning an [ocidir.DirV1] view scoped to one
// image's tag dir) and exposes the shared blob pool — the union of
// every image's content-addressed storage — via [OciDirs.Blob].
//
// Write operations are driven by callers that obtain a [fsutil.ResumableSource]
// or [fsutil.ResumableSink] per blob and invoke the fsutil resumable
// Pull/Push primitives directly. [OciDirs.PutTagFile] writes a single
// small per-image metadata file.
type OciDirs interface {
	// Blob reads from the shared blob pool. size is the total blob
	// size (not bytes remaining from offset); callers comparing
	// against a descriptor compare descriptor.Size against size, not
	// against bytes consumed from rc. Returns [os.ErrNotExist] when
	// the blob is missing.
	Blob(
		ctx context.Context,
		d digest.Digest,
		offset int64,
	) (rc io.ReadCloser, size int64, err error)

	// Image returns an [ocidir.DirV1] view scoped to ref's tag dir
	// (its own index.json + oci-layout, blobs delegating to the
	// shared pool). Existence is not checked here; the first
	// Index() / Blob() call surfaces "not found".
	Image(ref imageref.ImageRef) ocidir.DirV1

	// BlobSource returns a [fsutil.ResumableSource] for the blob
	// identified by dgst and size. The source reads from this OciDirs'
	// shared blob pool. It is used by the pull direction (caller reads
	// from this store and writes to another).
	BlobSource(ctx context.Context, dgst digest.Digest, size int64) (fsutil.ResumableSource, error)

	// BlobSink returns a [fsutil.ResumableSink] for the blob identified
	// by dgst and size. The sink writes into this OciDirs' shared blob
	// pool. It is used by the push direction (caller writes into this
	// store from another source).
	BlobSink(ctx context.Context, dgst digest.Digest, size int64) (fsutil.ResumableSink, error)

	// MkdirBlobParent ensures the parent directory of the blob identified
	// by dgst exists in this store (e.g. share/sha256/). Must be called
	// before writing a blob via [BlobSink] or Pull, since the fsutil
	// primitives do not create parent directories.
	MkdirBlobParent(dgst digest.Digest) error

	// PutTagFile writes a single small per-image metadata file
	// (e.g. "index.json" or "oci-layout") under ref's tag dir. Used
	// for the two well-known files only; manifest / config / layer
	// blobs go through [BlobSource] / [BlobSink].
	PutTagFile(ctx context.Context, ref imageref.ImageRef, name string, data []byte) error
}

// PutBlobsResult summarizes a blob-transfer run.
type PutBlobsResult struct {
	Sent      int   // blobs actually transferred
	Reused    int   // blobs already complete at destination
	BytesSent int64 // sum of Size for transferred blobs
}
