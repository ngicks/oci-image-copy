package ociimagecopy

import (
	"context"
	"fmt"
	"io"

	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// NewImageView returns a per-ref [ocidir.DirV1] (and [ocidir.RawAccessor])
// reconstructed from the split store: index.json / oci-layout are served
// verbatim via tags, and the manifest blob is served via blobs. It is the
// adapter that keeps the [ocidir.ReadManifest] / [ocidir.ReadRawManifest]
// choke points working after the store was split into [TagStoreV1] +
// [BlobStore].
//
// The returned view captures ctx in the struct. This is a deliberate, scoped
// exception to the repo's "don't stash a context in a struct" rule: the view
// is a short-lived per-call adapter created immediately before a
// ReadManifest / ReadRawManifest call (the only consumers), and the DirV1
// Index / ImageLayout methods take no ctx of their own. Blob still takes (and
// uses) its own ctx parameter.
func NewImageView(
	ctx context.Context,
	tags TagStoreV1,
	blobs BlobStore,
	ref imageref.ImageRef,
) ocidir.DirV1 {
	return &imageView{ctx: ctx, tags: tags, blobs: blobs, ref: ref}
}

// imageView implements [ocidir.DirV1] and [ocidir.RawAccessor] over a split
// store scoped to one image ref.
type imageView struct {
	ctx   context.Context
	tags  TagStoreV1
	blobs BlobStore
	ref   imageref.ImageRef
}

var _ ocidir.RawAccessor = (*imageView)(nil)

// Index implements [ocidir.DirV1]: parses the verbatim index.json bytes.
func (v *imageView) Index() (v1.Index, error) {
	raw, err := v.tags.GetIndex(v.ctx, v.ref)
	if err != nil {
		return v1.Index{}, fmt.Errorf("imageview: read index.json: %w", err)
	}
	idx, err := ocidir.ParseIndex(raw)
	if err != nil {
		return v1.Index{}, fmt.Errorf("imageview: parse index.json: %w", err)
	}
	return idx, nil
}

// ImageLayout implements [ocidir.DirV1]: parses the verbatim oci-layout bytes.
func (v *imageView) ImageLayout() (v1.ImageLayout, error) {
	raw, err := v.tags.GetOciLayout(v.ctx, v.ref)
	if err != nil {
		return v1.ImageLayout{}, fmt.Errorf("imageview: read oci-layout: %w", err)
	}
	l, err := ocidir.ParseImageLayout(raw)
	if err != nil {
		return v1.ImageLayout{}, fmt.Errorf("imageview: parse oci-layout: %w", err)
	}
	return l, nil
}

// RawIndex implements [ocidir.RawAccessor]: returns the verbatim index.json
// bytes (no re-marshal).
func (v *imageView) RawIndex() ([]byte, error) {
	raw, err := v.tags.GetIndex(v.ctx, v.ref)
	if err != nil {
		return nil, fmt.Errorf("imageview: read index.json: %w", err)
	}
	return raw, nil
}

// RawImageLayout implements [ocidir.RawAccessor]: returns the verbatim
// oci-layout bytes (no re-marshal).
func (v *imageView) RawImageLayout() ([]byte, error) {
	raw, err := v.tags.GetOciLayout(v.ctx, v.ref)
	if err != nil {
		return nil, fmt.Errorf("imageview: read oci-layout: %w", err)
	}
	return raw, nil
}

// Blob implements [ocidir.DirV1]. The only blob [ocidir.ReadManifest] reads
// through this view is the manifest blob; its size is resolved from the parsed
// index's manifest descriptor.
//
// GetIndex is read FIRST: for the file-server backend that priming call loads
// the meta (and its chunkSize) so the subsequent PrepDownload uses the correct
// chunk size — order matters.
func (v *imageView) Blob(
	ctx context.Context,
	dgst digest.Digest,
	offset int64,
) (io.ReadCloser, int64, error) {
	raw, err := v.tags.GetIndex(v.ctx, v.ref)
	if err != nil {
		return nil, 0, fmt.Errorf("imageview: read index.json: %w", err)
	}
	idx, err := ocidir.ParseIndex(raw)
	if err != nil {
		return nil, 0, fmt.Errorf("imageview: parse index.json: %w", err)
	}

	var size int64 = -1
	for _, m := range idx.Manifests {
		if m.Digest == dgst {
			size = m.Size
			break
		}
	}
	if size < 0 {
		return nil, 0, fmt.Errorf(
			"imageview: blob %s not found in index for %s", dgst, v.ref.String(),
		)
	}

	src, err := v.blobs.PrepDownload(ctx, dgst, size)
	if err != nil {
		return nil, 0, fmt.Errorf("imageview: prep download %s: %w", dgst, err)
	}
	rc, _, err := src.Open(ctx, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("imageview: open blob %s: %w", dgst, err)
	}
	return rc, size, nil
}
