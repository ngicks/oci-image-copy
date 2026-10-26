// Package httpdir implements [ocidir.OciDir] over HTTP.
//
// Blobs are read through Range-Requests, so the server must support them.
package httpdir

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"path"
	"strings"

	"github.com/ngicks/go-fsys-helper/stream/httprange"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ngicks/oci-image-copy/ocidir"
)

var _ ocidir.OciDir = (*Dir)(nil)

// Namer maps each oci-dir entry to a URL.
type Namer struct {
	OciLayout func() string
	Index     func() string
	Blob      func(dgst digest.Digest) string
}

// BaseURL maps entries under base: base+"/oci-layout", base+"/index.json",
// base+"/blobs/<alg>/<hex>".
func BaseURL(base string) Namer {
	base = strings.TrimSuffix(base, "/")
	return Namer{
		OciLayout: func() string { return base + "/" + ocispec.ImageLayoutFile },
		Index:     func() string { return base + "/" + ocispec.ImageIndexFile },
		Blob: func(dgst digest.Digest) string {
			return base + "/" + path.Join(
				ocispec.ImageBlobsDir,
				dgst.Algorithm().String(),
				dgst.Encoded(),
			)
		},
	}
}

type Dir struct {
	namer Namer
	cfg   httprange.Config
}

// New returns a Dir reading entries at the URLs namer gives.
//
// cfg may be nil; cfg.Client also serves the index.json and oci-layout GETs.
func New(namer Namer, cfg *httprange.Config) *Dir {
	d := &Dir{namer: namer}
	if cfg != nil {
		d.cfg = *cfg
	}
	return d
}

type reader struct {
	*httprange.ReaderAt
	size int64
}

func (r reader) Size() int64 {
	return r.size
}

func (d *Dir) Blobs(ctx context.Context, dgst digest.Digest) (ocidir.ReadAtCloser, error) {
	if err := dgst.Validate(); err != nil {
		return nil, err
	}
	return openURL(ctx, "blob "+dgst.String(), d.namer.Blob(dgst), &d.cfg)
}

// openURL probes url so the reader knows its size; name labels errors instead of the URL.
func openURL(
	ctx context.Context,
	name, url string,
	cfg *httprange.Config,
) (ocidir.ReadAtCloser, error) {
	r, err := httprange.New(ctx, url, cfg)
	if err != nil {
		return nil, fmt.Errorf("httpdir: %s: %w", name, err)
	}
	if err := r.Probe(ctx); err != nil {
		_ = r.Close()
		return nil, wrapErr(name, err)
	}
	meta, ok := r.Metadata()
	if !ok {
		_ = r.Close()
		return nil, fmt.Errorf("httpdir: %s: size unknown after probe", name)
	}
	return reader{ReaderAt: r, size: meta.Size}, nil
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
	return d.get(ctx, ocispec.ImageIndexFile, d.namer.Index())
}

func (d *Dir) OciLayoutBin(ctx context.Context) ([]byte, error) {
	return d.get(ctx, ocispec.ImageLayoutFile, d.namer.OciLayout())
}

func (d *Dir) get(ctx context.Context, name, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("httpdir: %s: %w", name, err)
	}
	maps.Copy(req.Header, d.cfg.Header)
	var client httprange.Doer = http.DefaultClient
	if d.cfg.Client != nil {
		client = d.cfg.Client
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpdir: %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, wrapErr(name, &httprange.StatusCodeError{Code: resp.StatusCode})
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("httpdir: %s: %w", name, err)
	}
	return b, nil
}

// wrapErr names the oci-dir entry instead of the URL, which may carry credentials.
func wrapErr(name string, err error) error {
	var sce *httprange.StatusCodeError
	if errors.As(err, &sce) && sce.NotFound() {
		return fmt.Errorf("httpdir: %s: %w: %w", name, fs.ErrNotExist, err)
	}
	return fmt.Errorf("httpdir: %s: %w", name, err)
}
