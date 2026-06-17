package ociimagecopy

import (
	"context"

	"github.com/ngicks/go-fsys-helper/fsutil"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/opencontainers/go-digest"
)

// The OCI store is split into two small consumer-side interfaces by concern:
//
//   - [BlobStore] is the content-addressed blob pool — large, streamed,
//     resumable bytes keyed by digest. It is version-agnostic by construction
//     (no V1 suffix): a plain content-addressable pool with no planned V2.
//   - [TagStoreV1] holds the per-tag pointer files (index.json / oci-layout) —
//     tiny, whole-file, verbatim bytes. The V1 suffix marks the
//     OCI-image-layout-v1-specific surface, leaving room for a V2 layout.
//
// [StoreV1] is the full OCI image-layout v1 store: both halves combined.

// BlobStore is the content-addressed blob pool: large, streamed, resumable
// bytes keyed by digest. Reads and writes obtain a resumable handle per blob
// and drive the fsutil Pull/Push primitives directly; [BlobStore.Stat] reports
// presence/resume state without transferring bytes.
type BlobStore interface {
	// Stat reports how much of the blob identified by dgst is present, modeled
	// on [os.Stat]. size is the total expected blob size (the caller supplies
	// it from the descriptor; a backend that cannot independently confirm the
	// total echoes it back in [BlobInfo.Size]).
	//
	//   - nothing present → an error satisfying errors.Is(err, fs.ErrNotExist);
	//   - partial → BlobInfo{CurrentSize, Size} with 0 < CurrentSize < Size;
	//   - complete → BlobInfo{CurrentSize: Size, Size}.
	//
	// CurrentSize doubles as the resume offset.
	Stat(ctx context.Context, dgst digest.Digest, size int64) (BlobInfo, error)

	// PrepDownload returns a [fsutil.ResumableSource] for the blob identified
	// by dgst and size. The source reads from this store's blob pool. Used by
	// the pull direction (caller reads from this store and writes to another).
	PrepDownload(ctx context.Context, dgst digest.Digest, size int64) (fsutil.ResumableSource, error)

	// PrepUpload returns a [fsutil.ResumableSink] for the blob identified by
	// dgst and size. The sink writes into this store's blob pool, creating any
	// parent directory the backend requires. Used by the push direction
	// (caller writes into this store from another source).
	PrepUpload(ctx context.Context, dgst digest.Digest, size int64) (fsutil.ResumableSink, error)
}

// TagStoreV1 holds the per-tag pointer files (index.json / oci-layout) as
// verbatim bytes — no parse/re-marshal round-trip, so a mirror preserves the
// source's exact bytes. index.json and oci-layout are separate method pairs
// because index.json is mutable (repeated saves to the same ref append
// manifests; merge is the caller's policy) while oci-layout is a write-once
// version marker.
type TagStoreV1 interface {
	// GetIndex returns the verbatim index.json bytes for ref, or an error
	// satisfying errors.Is(err, fs.ErrNotExist) when absent.
	GetIndex(ctx context.Context, ref imageref.ImageRef) ([]byte, error)
	// PutIndex writes the verbatim index.json bytes for ref.
	PutIndex(ctx context.Context, ref imageref.ImageRef, raw []byte) error
	// GetOciLayout returns the verbatim oci-layout bytes for ref, or an error
	// satisfying errors.Is(err, fs.ErrNotExist) when absent.
	GetOciLayout(ctx context.Context, ref imageref.ImageRef) ([]byte, error)
	// PutOciLayout writes the verbatim oci-layout bytes for ref.
	PutOciLayout(ctx context.Context, ref imageref.ImageRef, raw []byte) error
}

// StoreV1 is a full OCI image-layout v1 store: the content-addressed blob pool
// ([BlobStore]) plus the per-tag pointer files ([TagStoreV1]).
type StoreV1 interface {
	BlobStore
	TagStoreV1
}

// BlobInfo reports how much of a blob a [BlobStore] currently holds.
type BlobInfo struct {
	// CurrentSize is the number of bytes currently present at the store (the
	// resume point).
	CurrentSize int64
	// Size is the total expected blob size; the blob is complete iff
	// CurrentSize == Size.
	Size int64
}

// PutBlobsResult summarizes a blob-transfer run.
type PutBlobsResult struct {
	Sent      int   // blobs actually transferred
	Reused    int   // blobs already complete at destination
	BytesSent int64 // sum of Size for transferred blobs
}
