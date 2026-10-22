// Package fsdir implements [ocidir.OciDir] over a [vroot.Fs] whose root is the oci-dir.
package fsdir

import (
	"context"
	"encoding/json"
	"io"
	"path"

	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ngicks/oci-image-copy/ocidir"
)

var _ ocidir.OciDir = (*Dir[vroot.File])(nil)

type Dir[F vroot.File] struct {
	fsys vroot.Fs[F]
}

func New[F vroot.File](fsys vroot.Fs[F]) *Dir[F] {
	return &Dir[F]{fsys: fsys}
}

type file[F vroot.File] struct {
	f    F
	size int64
}

func (f file[F]) ReadAt(p []byte, off int64) (int, error) {
	return f.f.ReadAt(p, off)
}

func (f file[F]) Close() error {
	return f.f.Close()
}

func (f file[F]) Size() int64 {
	return f.size
}

func (d *Dir[F]) Blobs(ctx context.Context, dgst digest.Digest) (ocidir.ReadAtCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := dgst.Validate(); err != nil {
		return nil, err
	}
	f, err := d.fsys.Open(
		path.Join(ocispec.ImageBlobsDir, dgst.Algorithm().String(), dgst.Encoded()),
	)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return file[F]{f: f, size: info.Size()}, nil
}

func (d *Dir[F]) Index(ctx context.Context) (ocispec.Index, error) {
	var idx ocispec.Index
	b, err := d.IndexBin(ctx)
	if err != nil {
		return idx, err
	}
	return idx, json.Unmarshal(b, &idx)
}

func (d *Dir[F]) OciLayout(ctx context.Context) (ocispec.ImageLayout, error) {
	var l ocispec.ImageLayout
	b, err := d.OciLayoutBin(ctx)
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(b, &l)
}

func (d *Dir[F]) IndexBin(ctx context.Context) ([]byte, error) {
	return d.readFile(ctx, ocispec.ImageIndexFile)
}

func (d *Dir[F]) OciLayoutBin(ctx context.Context) ([]byte, error) {
	return d.readFile(ctx, ocispec.ImageLayoutFile)
}

func (d *Dir[F]) readFile(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := d.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}
