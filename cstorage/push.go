package cstorage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/archive"

	"github.com/ngicks/oci-image-copy/ocidir"
	"github.com/ngicks/oci-image-copy/store"
	"github.com/ngicks/oci-image-copy/transfer"
	"github.com/ngicks/oci-image-copy/types"
)

// PushSource binds the push source to dst, so layers whose original blob dst already holds stay
// as-is.
//
// The source serves each image as a single-manifest oci-dir.
// Each layer is taken as-is when its recorded compressed digest is in dst or the blob cache;
// otherwise its uncompressed diff is compressed with zstd into the blob cache.
// When every layer is as-is, the stored manifest is reused byte-for-byte; otherwise a new OCI
// manifest is written
// and Image.Manifest carries the new digest.
func (s *Storage) PushSource(dst *store.Store) transfer.Source {
	return &pushSource{s: s, dst: dst}
}

type pushSource struct {
	s   *Storage
	dst *store.Store
}

func (ps *pushSource) Resolve(
	ctx context.Context,
	ref types.Reference,
	p types.Platform,
) (store.Image, error) {
	rec, storedManifest, err := ps.s.find(ref, p)
	if err != nil {
		return store.Image{}, err
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(storedManifest, &m); err != nil {
		return store.Image{}, fmt.Errorf("cstorage: manifest of %s: %w", ref, err)
	}
	cfgBytes, err := ps.s.store.ImageBigData(rec.ID, m.Config.Digest.String())
	if err != nil {
		return store.Image{}, fmt.Errorf("cstorage: config of %s: %w", ref, err)
	}
	chain, err := ps.s.layerChain(rec.TopLayer)
	if err != nil {
		return store.Image{}, err
	}
	if len(chain) != len(m.Layers) {
		return store.Image{}, fmt.Errorf("cstorage: %s has %d layers but its manifest lists %d",
			ref, len(chain), len(m.Layers))
	}

	layers := make([]ocispec.Descriptor, len(chain))
	asIs := true
	for i, l := range chain {
		desc, ok, err := ps.asIs(ctx, m.Layers[i], l)
		if err != nil {
			return store.Image{}, err
		}
		asIs = asIs && ok && desc.Digest == m.Layers[i].Digest
		if !ok {
			desc, ok, err = ps.byDiffID(ctx, l.UncompressedDigest)
			if err != nil {
				return store.Image{}, err
			}
		}
		if !ok {
			desc, err = ps.s.exportZstd(ctx, l)
			if err != nil {
				return store.Image{}, err
			}
		}
		layers[i] = desc
	}

	cfg := m.Config
	if _, err := ps.s.cache.Put(ctx, cfg.MediaType, bytes.NewReader(cfgBytes)); err != nil {
		return store.Image{}, err
	}
	manifestBytes := storedManifest
	mediaType := m.MediaType
	if !asIs {
		mediaType = ocispec.MediaTypeImageManifest
		cfg.MediaType = ocispec.MediaTypeImageConfig
		manifestBytes, err = json.Marshal(ocispec.Manifest{
			SchemaVersion: 2,
			MediaType:     mediaType,
			Config:        cfg,
			Layers:        layers,
		})
		if err != nil {
			return store.Image{}, err
		}
	}
	md, err := ps.s.cache.Put(ctx, mediaType, bytes.NewReader(manifestBytes))
	if err != nil {
		return store.Image{}, err
	}
	blobs := func(ctx context.Context, d digest.Digest) (ocidir.ReadAtCloser, error) {
		r, err := ps.s.cache.Open(d)
		if errors.Is(err, fs.ErrNotExist) {
			return ps.dst.Blob(ctx, d)
		}
		return r, err
	}
	return store.Image{
		Ref:      ref,
		Platform: p,
		Manifest: md,
		Dir:      store.SingleManifestDir(blobs, p, md),
	}, nil
}

// find returns the image record for ref and p with its manifest: the one stored under ref's digest,
// or the default one for a tag.
func (s *Storage) find(ref types.Reference, p types.Platform) (*storage.Image, []byte, error) {
	var candidates []*storage.Image
	key := storage.ImageDigestBigDataKey
	if ref.Digest != "" {
		imgs, err := s.store.ImagesByDigest(ref.Digest)
		if err != nil {
			return nil, nil, err
		}
		candidates = imgs
		key = storage.ImageDigestManifestBigDataNamePrefix + "-" + ref.Digest.String()
		if len(imgs) == 0 {
			// An index pin pulled from a store has no index bytes to record, so Pull names the
			// image
			// repo@<index digest> instead; its own manifest is the default one.
			if img, err := s.store.Image(ref.String()); err == nil {
				candidates = []*storage.Image{img}
				key = storage.ImageDigestBigDataKey
			}
		}
	} else {
		img, err := s.store.Image(ref.String())
		if err != nil {
			return nil, nil, fmt.Errorf("cstorage: %s: %w: %w", ref, fs.ErrNotExist, err)
		}
		candidates = []*storage.Image{img}
	}
	for _, img := range candidates {
		b, err := s.store.ImageBigData(img.ID, key)
		if err != nil {
			continue
		}
		// podman keeps the index a tag resolved to under manifest-<index digest>; use its platform
		// child.
		if child, ok := indexChild(b, p); ok {
			b, err = s.store.ImageBigData(
				img.ID,
				storage.ImageDigestManifestBigDataNamePrefix+"-"+child.String(),
			)
			if err != nil {
				continue
			}
		}
		ok, err := s.platformOf(img, b, p)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			return img, b, nil
		}
	}
	return nil, nil, fmt.Errorf("cstorage: no %s image for %s: %w", p, ref, fs.ErrNotExist)
}

func (s *Storage) platformOf(
	img *storage.Image,
	manifestBytes []byte,
	p types.Platform,
) (bool, error) {
	var m ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return false, err
	}
	if m.Config.Digest == "" {
		return false, nil // an image index stored under the digest, not a platform manifest
	}
	b, err := s.store.ImageBigData(img.ID, m.Config.Digest.String())
	if err != nil {
		return false, err
	}
	var cfg ocispec.Image
	if err := json.Unmarshal(b, &cfg); err != nil {
		return false, err
	}
	return p.Matches(cfg.Platform), nil
}

// layerChain returns the layers from the bottom up to top.
func (s *Storage) layerChain(top string) ([]*storage.Layer, error) {
	var chain []*storage.Layer
	for id := top; id != ""; {
		l, err := s.store.Layer(id)
		if err != nil {
			return nil, err
		}
		chain = append(chain, l)
		id = l.Parent
	}
	slices.Reverse(chain)
	return chain, nil
}

// asIs returns the original compressed blob of a layer when dst or the blob cache holds it.
func (ps *pushSource) asIs(
	ctx context.Context,
	listed ocispec.Descriptor,
	l *storage.Layer,
) (ocispec.Descriptor, bool, error) {
	candidates := []ocispec.Descriptor{listed}
	if l.CompressedDigest != "" && l.CompressedDigest != listed.Digest {
		candidates = append(candidates, ocispec.Descriptor{
			MediaType: compressedMediaType(l.CompressionType),
			Digest:    l.CompressedDigest,
			Size:      l.CompressedSize,
		})
	}
	for _, c := range candidates {
		if c.Digest == l.UncompressedDigest {
			continue // an uncompressed layer as a local build lists it; it never was a compressed blob
		}
		if ps.s.cache.Has(c.Digest) {
			return c, true, nil
		}
		ok, err := ps.dst.HasBlob(ctx, c.Digest)
		if err != nil {
			return ocispec.Descriptor{}, false, err
		}
		if ok {
			return c, true, nil
		}
	}
	return ocispec.Descriptor{}, false, nil
}

func compressedMediaType(c archive.Compression) string {
	if c == archive.Zstd {
		return ocispec.MediaTypeImageLayerZstd
	}
	return ocispec.MediaTypeImageLayerGzip
}

// exportZstd compresses the layer's uncompressed diff with zstd into the blob cache,
// checking that the diff still hashes to the layer's diffID.
func (s *Storage) exportZstd(ctx context.Context, l *storage.Layer) (ocispec.Descriptor, error) {
	uncompressed := archive.Uncompressed
	diff, err := s.store.Diff("", l.ID, &storage.DiffOptions{Compression: &uncompressed})
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("cstorage: reading layer %s: %w", l.ID, err)
	}
	defer func() { _ = diff.Close() }()

	pr, pw := io.Pipe()
	digester := digest.Canonical.Digester()
	go func() {
		enc, err := zstd.NewWriter(pw, zstd.WithEncoderLevel(s.zstdLevel()))
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_, err = io.Copy(enc, io.TeeReader(diff, digester.Hash()))
		if cerr := enc.Close(); err == nil {
			err = cerr
		}
		_ = pw.CloseWithError(err)
	}()
	desc, err := s.cache.Put(ctx, ocispec.MediaTypeImageLayerZstd, pr)
	_ = pr.Close()
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("cstorage: compressing layer %s: %w", l.ID, err)
	}
	if got := digester.Digest(); got != l.UncompressedDigest {
		return ocispec.Descriptor{}, fmt.Errorf("cstorage: layer %s diff hashes to %s, recorded %s",
			l.ID, got, l.UncompressedDigest)
	}
	return desc, nil
}

// indexChild returns the manifest digest matching p when b is an image index.
func indexChild(b []byte, p types.Platform) (digest.Digest, bool) {
	var idx ocispec.Index
	if err := json.Unmarshal(b, &idx); err != nil || len(idx.Manifests) == 0 {
		return "", false
	}
	for _, d := range idx.Manifests {
		if d.Platform != nil && p.Matches(*d.Platform) {
			return d.Digest, true
		}
	}
	return "", false
}

// byDiffID returns a blob dst already records for diffID, so no new compressed variant is created.
// Its media type is read from the blob's magic number, because markers carry none.
func (ps *pushSource) byDiffID(
	ctx context.Context,
	diffID digest.Digest,
) (ocispec.Descriptor, bool, error) {
	blobs, err := ps.dst.DiffIDBlobs(ctx, diffID)
	if errors.Is(err, store.ErrNotListable) {
		return ocispec.Descriptor{}, false, nil
	}
	if err != nil {
		return ocispec.Descriptor{}, false, err
	}
	for _, b := range blobs {
		r, err := ps.dst.Blob(ctx, b)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return ocispec.Descriptor{}, false, err
		}
		var magic [4]byte
		n, _ := r.ReadAt(magic[:], 0)
		size := r.Size()
		_ = r.Close()
		mediaType, ok := layerMediaType(magic[:n])
		if !ok {
			continue
		}
		return ocispec.Descriptor{MediaType: mediaType, Digest: b, Size: size}, true, nil
	}
	return ocispec.Descriptor{}, false, nil
}

func layerMediaType(magic []byte) (string, bool) {
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		return ocispec.MediaTypeImageLayerGzip, true
	case bytes.HasPrefix(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return ocispec.MediaTypeImageLayerZstd, true
	}
	return "", false
}
