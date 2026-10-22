// Package s3dir implements [ocidir.OciDir] over an S3-compatible object storage.
package s3dir

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ngicks/oci-image-copy/ocidir"
)

var _ ocidir.OciDir = (*Dir)(nil)

// Namer maps each oci-dir entry to a bucket and key.
type Namer struct {
	OciLayout func() (bucket, key string)
	Index     func() (bucket, key string)
	Blob      func(dgst digest.Digest) (bucket, key string)
}

// Flat keeps the oci-dir layout under prefix in one bucket.
// prefix is prepended verbatim; include a trailing "/" to make it a directory.
func Flat(bucket, prefix string) Namer {
	return Namer{
		OciLayout: func() (string, string) { return bucket, prefix + ocispec.ImageLayoutFile },
		Index:     func() (string, string) { return bucket, prefix + ocispec.ImageIndexFile },
		Blob: func(dgst digest.Digest) (string, string) {
			return bucket, prefix + path.Join(
				ocispec.ImageBlobsDir,
				dgst.Algorithm().String(),
				dgst.Encoded(),
			)
		},
	}
}

// Client is the subset of [*s3.Client] Dir uses.
type Client interface {
	GetObject(
		ctx context.Context,
		in *s3.GetObjectInput,
		optFns ...func(*s3.Options),
	) (*s3.GetObjectOutput, error)
	HeadObject(
		ctx context.Context,
		in *s3.HeadObjectInput,
		optFns ...func(*s3.Options),
	) (*s3.HeadObjectOutput, error)
}

type Dir struct {
	client Client
	namer  Namer
}

func New(client Client, namer Namer) *Dir {
	return &Dir{client: client, namer: namer}
}

// Blobs returns a reader issuing one ranged GetObject per ReadAt.
//
// The reader keeps ctx for its reads because [io.ReaderAt] takes no context.
// Reads fail once the object's ETag changes.
func (d *Dir) Blobs(ctx context.Context, dgst digest.Digest) (ocidir.ReadAtCloser, error) {
	if err := dgst.Validate(); err != nil {
		return nil, err
	}
	name := "blob " + dgst.String()
	bucket, key := d.namer.Blob(dgst)
	out, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &key})
	if err != nil {
		return nil, wrapErr(name, err)
	}
	if out.ContentLength == nil {
		return nil, fmt.Errorf("s3dir: %s: HeadObject returned no Content-Length", name)
	}
	return &reader{
		ctx:    ctx,
		client: d.client,
		name:   name,
		bucket: bucket,
		key:    key,
		etag:   out.ETag,
		size:   *out.ContentLength,
	}, nil
}

func (d *Dir) Index(ctx context.Context) (ocispec.Index, error) {
	var idx ocispec.Index
	b, err := d.IndexBin(ctx)
	if err != nil {
		return idx, err
	}
	return idx, json.Unmarshal(b, &idx)
}

func (d *Dir) OciLayout(ctx context.Context) (ocispec.ImageLayout, error) {
	var l ocispec.ImageLayout
	b, err := d.OciLayoutBin(ctx)
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(b, &l)
}

func (d *Dir) IndexBin(ctx context.Context) ([]byte, error) {
	bucket, key := d.namer.Index()
	return d.get(ctx, ocispec.ImageIndexFile, bucket, key)
}

func (d *Dir) OciLayoutBin(ctx context.Context) ([]byte, error) {
	bucket, key := d.namer.OciLayout()
	return d.get(ctx, ocispec.ImageLayoutFile, bucket, key)
}

func (d *Dir) get(ctx context.Context, name, bucket, key string) ([]byte, error) {
	out, err := d.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key})
	if err != nil {
		return nil, wrapErr(name, err)
	}
	defer func() { _ = out.Body.Close() }()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("s3dir: %s: %w", name, err)
	}
	return b, nil
}

type reader struct {
	ctx    context.Context
	client Client
	name   string
	bucket string
	key    string
	etag   *string
	size   int64
}

func (r *reader) Size() int64 {
	return r.size
}

func (r *reader) Close() error {
	return nil
}

func (r *reader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("s3dir: %s: negative offset %d", r.name, off)
	}
	if off >= r.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	end := min(off+int64(len(p)), r.size)
	rng := fmt.Sprintf("bytes=%d-%d", off, end-1)
	out, err := r.client.GetObject(r.ctx, &s3.GetObjectInput{
		Bucket:  &r.bucket,
		Key:     &r.key,
		Range:   &rng,
		IfMatch: r.etag,
	})
	if err != nil {
		return 0, wrapErr(r.name, err)
	}
	defer func() { _ = out.Body.Close() }()
	n, err := io.ReadFull(out.Body, p[:end-off])
	if err != nil {
		return n, fmt.Errorf("s3dir: %s: %w", r.name, err)
	}
	if end == r.size && n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// wrapErr maps 404 responses to [fs.ErrNotExist].
// HeadObject has no body, so its 404 carries no typed NoSuchKey error; the status code is what both
// share.
func wrapErr(name string, err error) error {
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) && status.HTTPStatusCode() == http.StatusNotFound {
		return fmt.Errorf("s3dir: %s: %w: %w", name, fs.ErrNotExist, err)
	}
	return fmt.Errorf("s3dir: %s: %w", name, err)
}
