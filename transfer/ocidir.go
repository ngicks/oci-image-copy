package transfer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ngicks/oci-image-copy/ocidir"
	"github.com/ngicks/oci-image-copy/store"
	"github.com/ngicks/oci-image-copy/types"
)

// FromOciDir selects entries of dir by ref name annotation or digest, flattening image indexes by
// platform.
//
// A tag entry matches an index.json ref.name that parses to the same repository and tag,
// or that equals the bare tag (skopeo copy … oci:<dir>:<tag> writes only the tag).
// A digest entry matches a descriptor at the top level or inside nested indexes.
func FromOciDir(dir ocidir.OciDir) Source {
	return &ociDirSource{dir: dir}
}

type ociDirSource struct {
	dir ocidir.OciDir
}

func (s *ociDirSource) Resolve(
	ctx context.Context,
	ref types.Reference,
	p types.Platform,
) (store.Image, error) {
	idx, err := s.dir.Index(ctx)
	if err != nil {
		return store.Image{}, err
	}
	desc, err := s.find(ctx, idx.Manifests, ref)
	if err != nil {
		return store.Image{}, err
	}
	manifest, err := s.selectPlatform(ctx, desc, p)
	if err != nil {
		return store.Image{}, err
	}
	return store.Image{
		Ref:      ref,
		Platform: p,
		Manifest: manifest,
		Dir:      store.SingleManifestDir(s.dir.Blobs, p, manifest),
	}, nil
}

func (s *ociDirSource) find(
	ctx context.Context,
	descs []ocispec.Descriptor,
	ref types.Reference,
) (ocispec.Descriptor, error) {
	if ref.Digest == "" {
		for _, d := range descs {
			if nameMatches(d.Annotations[ocispec.AnnotationRefName], ref) {
				return d, nil
			}
		}
		return ocispec.Descriptor{}, fmt.Errorf("transfer: %s: %w", ref, fs.ErrNotExist)
	}
	for _, d := range descs {
		if d.Digest == ref.Digest {
			return d, nil
		}
		if isIndex(d.MediaType) {
			var child ocispec.Index
			if err := s.readJSON(ctx, d.Digest, &child); err != nil {
				return ocispec.Descriptor{}, err
			}
			if found, err := s.find(ctx, child.Manifests, ref); err == nil {
				return found, nil
			}
		}
	}
	return ocispec.Descriptor{}, fmt.Errorf("transfer: %s: %w", ref, fs.ErrNotExist)
}

func nameMatches(annotation string, ref types.Reference) bool {
	if annotation == "" {
		return false
	}
	if annotation == ref.Tag {
		return true
	}
	parsed, err := types.ParseReference(annotation)
	return err == nil && parsed.Repository == ref.Repository && parsed.Tag == ref.Tag &&
		parsed.Digest == ""
}

// selectPlatform returns desc itself for a manifest, or the single child of an index matching p.
func (s *ociDirSource) selectPlatform(
	ctx context.Context,
	desc ocispec.Descriptor,
	p types.Platform,
) (ocispec.Descriptor, error) {
	if !isIndex(desc.MediaType) {
		var m ocispec.Manifest
		if err := s.readJSON(ctx, desc.Digest, &m); err != nil {
			return ocispec.Descriptor{}, err
		}
		var cfg ocispec.Image
		if err := s.readJSON(ctx, m.Config.Digest, &cfg); err != nil {
			return ocispec.Descriptor{}, err
		}
		if !p.Matches(cfg.Platform) {
			return ocispec.Descriptor{}, fmt.Errorf("transfer: manifest %s is %s/%s, not %s: %w",
				desc.Digest, cfg.OS, cfg.Architecture, p, fs.ErrNotExist)
		}
		desc.Platform = nil
		desc.Annotations = nil
		return desc, nil
	}
	var idx ocispec.Index
	if err := s.readJSON(ctx, desc.Digest, &idx); err != nil {
		return ocispec.Descriptor{}, err
	}
	var matches []ocispec.Descriptor
	for _, child := range idx.Manifests {
		if child.Platform == nil {
			continue
		}
		if isIndex(child.MediaType) {
			continue
		}
		if p.Matches(*child.Platform) {
			matches = append(matches, child)
		}
	}
	switch len(matches) {
	case 0:
		return ocispec.Descriptor{}, fmt.Errorf("transfer: index %s has no %s manifest: %w",
			desc.Digest, p, fs.ErrNotExist)
	case 1:
		matches[0].Platform = nil
		return matches[0], nil
	default:
		var ds []digest.Digest
		for _, m := range matches {
			ds = append(ds, m.Digest)
		}
		return ocispec.Descriptor{}, fmt.Errorf(
			"%w: index %s for %s: %v",
			store.ErrAmbiguous,
			desc.Digest,
			p,
			ds,
		)
	}
}

func (s *ociDirSource) readJSON(ctx context.Context, dgst digest.Digest, v any) error {
	r, err := s.dir.Blobs(ctx, dgst)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
	if err != nil {
		return err
	}
	if digest.FromBytes(b) != dgst {
		return fmt.Errorf("transfer: blob %s does not match its digest", dgst)
	}
	return json.Unmarshal(b, v)
}

func isIndex(mediaType string) bool {
	return mediaType == ocispec.MediaTypeImageIndex ||
		mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
}
