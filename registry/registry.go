// Package registry pulls single-platform images from registries into a blob cache,
// for entries a store lacks.
//
// It keeps original blobs byte-for-byte, so the pulled image has the same manifest digest as in the
// registry.
// It reads only; nothing is ever written to a registry.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/pkg/blobinfocache/none"
	imagetypes "go.podman.io/image/v5/types"

	"github.com/ngicks/oci-image-copy/blobcache"
	"github.com/ngicks/oci-image-copy/ocidir"
	"github.com/ngicks/oci-image-copy/store"
	"github.com/ngicks/oci-image-copy/transfer"
	"github.com/ngicks/oci-image-copy/types"
)

var _ transfer.Source = (*Source)(nil)

type Options struct {
	// Registries are tried after the entry's own repository.
	// Each replaces the repository's registry host, optionally with a path prefix:
	// "localhost:5000" turns docker.io/library/ubuntu into localhost:5000/library/ubuntu,
	// "mirror.example.com/cache" into mirror.example.com/cache/library/ubuntu.
	// Mirrors in registries.conf apply independently, through SystemContext.
	Registries []string
	// SystemContext carries registries.conf, auth.json and TLS settings; nil uses the host
	// defaults.
	SystemContext *imagetypes.SystemContext
}

type Source struct {
	cache *blobcache.Cache
	opt   Options
}

func New(cache *blobcache.Cache, opt *Options) *Source {
	s := &Source{cache: cache}
	if opt != nil {
		s.opt = *opt
	}
	return s
}

// Resolve pulls ref for p in full, keeping original blobs, and serves it as a single-manifest
// oci-dir.
// When every candidate repository fails, the error wraps fs.ErrNotExist.
func (s *Source) Resolve(
	ctx context.Context,
	ref types.Reference,
	p types.Platform,
) (store.Image, error) {
	var errs []error
	for _, repo := range s.candidates(ref.Repository) {
		img, err := s.pull(ctx, repo, ref, p)
		if err == nil {
			return img, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", repo, err))
		if ctx.Err() != nil {
			break
		}
	}
	return store.Image{}, fmt.Errorf(
		"registry: %s for %s: %w: %w",
		ref,
		p,
		fs.ErrNotExist,
		errors.Join(errs...),
	)
}

func (s *Source) candidates(repository string) []string {
	out := []string{repository}
	_, path, ok := strings.Cut(repository, "/")
	if !ok {
		return out
	}
	for _, r := range s.opt.Registries {
		out = append(out, strings.TrimSuffix(r, "/")+"/"+path)
	}
	return out
}

func (s *Source) pull(
	ctx context.Context,
	repo string,
	ref types.Reference,
	p types.Platform,
) (store.Image, error) {
	spec := repo
	if ref.Digest != "" {
		spec += "@" + ref.Digest.String()
	} else {
		spec += ":" + ref.Tag
	}
	named, err := reference.ParseNormalizedNamed(spec)
	if err != nil {
		return store.Image{}, err
	}
	dref, err := docker.NewReference(named)
	if err != nil {
		return store.Image{}, err
	}
	src, err := dref.NewImageSource(ctx, s.opt.SystemContext)
	if err != nil {
		return store.Image{}, err
	}
	defer func() { _ = src.Close() }()

	top, mime, err := src.GetManifest(ctx, nil)
	if err != nil {
		return store.Image{}, err
	}
	if ref.Digest != "" {
		if got := digest.FromBytes(top); got != ref.Digest {
			return store.Image{}, fmt.Errorf("manifest digest %s, want %s", got, ref.Digest)
		}
	}

	manifestBytes, manifestMIME := top, mime
	if manifest.MIMETypeIsMultiImage(mime) {
		instance, err := selectInstance(top, mime, p)
		if err != nil {
			return store.Image{}, err
		}
		manifestBytes, manifestMIME, err = src.GetManifest(ctx, &instance)
		if err != nil {
			return store.Image{}, err
		}
		if got := digest.FromBytes(manifestBytes); got != instance {
			return store.Image{}, fmt.Errorf("manifest digest %s, want %s", got, instance)
		}
	}

	var m ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return store.Image{}, err
	}
	cfgBytes, err := s.fetch(ctx, src, m.Config)
	if err != nil {
		return store.Image{}, err
	}
	var cfg ocispec.Image
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		return store.Image{}, err
	}
	if !p.Matches(cfg.Platform) {
		return store.Image{}, fmt.Errorf("image is %s/%s, not %s", cfg.OS, cfg.Architecture, p)
	}
	for _, l := range m.Layers {
		if _, err := s.fetch(ctx, src, l); err != nil {
			return store.Image{}, err
		}
	}
	md, err := s.cache.Put(ctx, manifestMIME, strings.NewReader(string(manifestBytes)))
	if err != nil {
		return store.Image{}, err
	}
	return store.Image{
		Ref:      ref,
		Platform: p,
		Manifest: md,
		Dir:      &cacheDir{cache: s.cache, platform: p, manifest: md},
	}, nil
}

func selectInstance(blob []byte, mime string, p types.Platform) (digest.Digest, error) {
	list, err := manifest.ListFromBlob(blob, mime)
	if err != nil {
		return "", err
	}
	var matches []digest.Digest
	for _, d := range list.Instances() {
		info, err := list.Instance(d)
		if err != nil {
			return "", err
		}
		if info.ReadOnly.Platform == nil {
			continue
		}
		if p.Matches(*info.ReadOnly.Platform) {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no %s manifest in the index", p)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%w: %v", store.ErrAmbiguous, matches)
	}
}

// fetch stores d into the cache unless it is there, verifying digest and size, and returns small
// blobs' bytes.
func (s *Source) fetch(
	ctx context.Context,
	src imagetypes.ImageSource,
	d ocispec.Descriptor,
) ([]byte, error) {
	if !s.cache.Has(d.Digest) {
		rc, _, err := src.GetBlob(
			ctx,
			imagetypes.BlobInfo{Digest: d.Digest, Size: d.Size},
			none.NoCache,
		)
		if err != nil {
			return nil, err
		}
		got, err := s.cache.Put(ctx, d.MediaType, rc)
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		if got.Digest != d.Digest || got.Size != d.Size {
			return nil, fmt.Errorf("%w: blob %s (%d bytes) arrived as %s (%d bytes)",
				blobcache.ErrMismatch, d.Digest, d.Size, got.Digest, got.Size)
		}
	}
	if d.MediaType == ocispec.MediaTypeImageConfig ||
		d.MediaType == "application/vnd.docker.container.image.v1+json" {
		r, err := s.cache.Open(d.Digest)
		if err != nil {
			return nil, err
		}
		defer func() { _ = r.Close() }()
		return io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
	}
	return nil, nil
}

// cacheDir serves a pulled image from the blob cache as a single-manifest oci-dir.
type cacheDir struct {
	cache    *blobcache.Cache
	platform types.Platform
	manifest ocispec.Descriptor
}

var _ ocidir.OciDir = (*cacheDir)(nil)

func (d *cacheDir) Blobs(_ context.Context, dgst digest.Digest) (ocidir.ReadAtCloser, error) {
	return d.cache.Open(dgst)
}

func (d *cacheDir) Index(context.Context) (ocispec.Index, error) {
	m := d.manifest
	m.Platform = &ocispec.Platform{
		OS:           d.platform.OS,
		Architecture: d.platform.Architecture,
		Variant:      d.platform.Variant,
	}
	return ocispec.Index{
		SchemaVersion: 2,
		MediaType:     ocispec.MediaTypeImageIndex,
		Manifests:     []ocispec.Descriptor{m},
	}, nil
}

func (d *cacheDir) OciLayout(context.Context) (ocispec.ImageLayout, error) {
	return ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion}, nil
}

func (d *cacheDir) IndexBin(ctx context.Context) ([]byte, error) {
	idx, _ := d.Index(ctx)
	return json.Marshal(idx)
}

func (d *cacheDir) OciLayoutBin(ctx context.Context) ([]byte, error) {
	l, _ := d.OciLayout(ctx)
	return json.Marshal(l)
}
