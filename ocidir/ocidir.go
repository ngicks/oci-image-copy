// Package ocidir reads an OCI image layout through a storage-agnostic interface.
//
// An OCI image layout holds three kinds of entries:
// "oci-layout", "index.json" and "blobs/<alg>/<hex>".
// [OciDir] exposes each of them; backends live in subpackages.
package ocidir

import (
	"context"
	"io"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type ReadAtCloser interface {
	io.ReaderAt
	io.Closer
	Size() int64
}

// OciDir is a read-only view of an OCI image layout.
//
// Implementations return an error wrapping [io/fs.ErrNotExist]
// when the requested entry is missing.
type OciDir interface {
	// Blobs opens the blob dgst.
	//
	// Remote implementations keep ctx for the reader's later ReadAt calls,
	// because io.ReaderAt takes no context;
	// cancelling ctx fails reads on the returned reader.
	Blobs(ctx context.Context, dgst digest.Digest) (ReadAtCloser, error)
	Index(ctx context.Context) (ocispec.Index, error)
	OciLayout(ctx context.Context) (ocispec.ImageLayout, error)

	// opens those files but no parse.
	IndexBin(ctx context.Context) ([]byte, error)
	OciLayoutBin(ctx context.Context) ([]byte, error)
}
