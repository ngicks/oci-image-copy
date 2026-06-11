package fileserver

// adapter.go provides thin wrappers that adapt stream/fileserver's
// ChunkedSource and ChunkedSink to the fsutil.ResumableSource and
// fsutil.ResumableSink interfaces respectively.
//
// ChunkedSource.Open(ctx, offset) (io.ReadCloser, error) must become
// ResumableSource.Open(ctx, offset) (io.ReadCloser, ContentInfo, error).
//
// ChunkedSink.State(ctx) (offset, complete, err) must become
// ResumableSink.State(ctx) (SinkState, error), and
// ChunkedSink.Append(ctx, offset, r) must become
// ResumableSink.Append(ctx, info, offset, r).
//
// The adapter supplies the digest string as the ETag because content-addressed
// names make the ETag definitionally correct and stable.
//
// Compile-time interface assertions live in this file so that compilation
// fails immediately if the stream/fileserver types diverge from the required
// structural shape.

import (
	"context"
	"io"

	"github.com/ngicks/go-fsys-helper/fsutil"
	"github.com/ngicks/go-fsys-helper/stream/fileserver"
	"github.com/opencontainers/go-digest"
)

// ChunkedSourceAdapter wraps a [fileserver.ChunkedSource] and satisfies
// [fsutil.ResumableSource]. The ETag is the blob digest string so that
// the fsutil resumable machinery can track identity across interrupted
// transfers without any additional state.
type ChunkedSourceAdapter struct {
	Src  *fileserver.ChunkedSource
	etag string
	size int64
}

// NewChunkedSourceAdapter creates a [fsutil.ResumableSource] backed by a
// [fileserver.ChunkedSource].
//
//   - c: the file-server client.
//   - naming: provides the chunk name function for this blob.
//   - dgst: the blob digest (used as ETag).
//   - size: total blob size.
//   - chunkSize: fixed chunk size.
func NewChunkedSourceAdapter(
	c fileserver.Client,
	naming NamingConvention,
	dgst digest.Digest,
	size, chunkSize int64,
) *ChunkedSourceAdapter {
	nameFn := func(i int) string { return naming.BlobChunk(dgst, i) }
	src := fileserver.NewChunkedSource(c, nameFn, size, chunkSize)
	return &ChunkedSourceAdapter{
		Src:  src,
		etag: dgst.String(),
		size: size,
	}
}

// Open implements [fsutil.ResumableSource].
func (a *ChunkedSourceAdapter) Open(
	ctx context.Context,
	offset int64,
) (io.ReadCloser, fsutil.ContentInfo, error) {
	rc, err := a.Src.Open(ctx, offset)
	if err != nil {
		return nil, fsutil.ContentInfo{}, err
	}
	return rc, fsutil.ContentInfo{ETag: a.etag, Size: a.size}, nil
}

// Compile-time assertion: ChunkedSourceAdapter implements fsutil.ResumableSource.
var _ fsutil.ResumableSource = (*ChunkedSourceAdapter)(nil)

// ChunkedSinkAdapter wraps a [fileserver.ChunkedSink] and satisfies
// [fsutil.ResumableSink]. The ETag is always the blob digest string.
//
// State maps (offset, complete) from ChunkedSink to fsutil.SinkState:
//
//	{Offset: offset, ETag: dgst.String(), Complete: complete}
//
// Append ignores the ContentInfo parameter (the ETag is embedded in the
// content-addressed chunk name; no additional tracking needed).
// Commit delegates to [fileserver.ChunkedSink.Commit], which is a no-op at
// this layer (the real commit is the metadata object written by the caller).
type ChunkedSinkAdapter struct {
	sink *fileserver.ChunkedSink
	etag string
}

// NewChunkedSinkAdapter creates a [fsutil.ResumableSink] backed by a
// [fileserver.ChunkedSink].
//
//   - c: the file-server client.
//   - naming: provides the chunk name function for this blob.
//   - dgst: the blob digest (used as ETag in SinkState).
//   - size: total blob size.
//   - chunkSize: fixed chunk size.
func NewChunkedSinkAdapter(
	c fileserver.Client,
	naming NamingConvention,
	dgst digest.Digest,
	size, chunkSize int64,
) *ChunkedSinkAdapter {
	nameFn := func(i int) string { return naming.BlobChunk(dgst, i) }
	sink := fileserver.NewChunkedSink(c, nameFn, size, chunkSize)
	return &ChunkedSinkAdapter{
		sink: sink,
		etag: dgst.String(),
	}
}

// State implements [fsutil.ResumableSink].
func (a *ChunkedSinkAdapter) State(ctx context.Context) (fsutil.SinkState, error) {
	offset, complete, err := a.sink.State(ctx)
	if err != nil {
		return fsutil.SinkState{}, err
	}
	return fsutil.SinkState{
		Offset:   offset,
		ETag:     a.etag,
		Complete: complete,
	}, nil
}

// Append implements [fsutil.ResumableSink].
// info is accepted for interface compatibility but not used at this layer:
// ETag identity is embedded in the content-addressed chunk names.
func (a *ChunkedSinkAdapter) Append(
	ctx context.Context,
	_ fsutil.ContentInfo,
	offset int64,
	r io.Reader,
) error {
	return a.sink.Append(ctx, offset, r)
}

// Commit implements [fsutil.ResumableSink].
// Delegates to [fileserver.ChunkedSink.Commit], which is a no-op: the
// real commit is the metadata object written by the caller.
func (a *ChunkedSinkAdapter) Commit(ctx context.Context) error {
	return a.sink.Commit(ctx)
}

// Compile-time assertion: ChunkedSinkAdapter implements fsutil.ResumableSink.
var _ fsutil.ResumableSink = (*ChunkedSinkAdapter)(nil)
