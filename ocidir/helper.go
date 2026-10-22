package ocidir

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"slices"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Docker media types still appear in layouts written by older tools.
// Their JSON shape is compatible with the OCI counterparts.
const (
	mediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
)

func isIndex(mediaType string) bool {
	return mediaType == ocispec.MediaTypeImageIndex || mediaType == mediaTypeDockerManifestList
}

func isManifest(mediaType string) bool {
	return mediaType == ocispec.MediaTypeImageManifest || mediaType == mediaTypeDockerManifest
}

type Blob struct {
	Name   digest.Digest
	Reader ReadAtCloser
}

// AllImages lists all images from Index, keyed by the image ref name annotation.
//
// Index entries without the annotation are skipped.
// An entry pointing at an image index yields one config per platform manifest under the same key.
// Every manifest and config is read before AllImages returns.
func AllImages(ctx context.Context, dir OciDir) (iter.Seq2[string, ocispec.Image], error) {
	idx, err := dir.Index(ctx)
	if err != nil {
		return nil, err
	}

	type named struct {
		name string
		img  ocispec.Image
	}
	var images []named
	for _, desc := range idx.Manifests {
		name, ok := desc.Annotations[ocispec.AnnotationRefName]
		if !ok {
			continue
		}
		err := walkManifests(ctx, dir, desc, func(m ocispec.Manifest) error {
			var img ocispec.Image
			if err := readJSON(ctx, dir, m.Config.Digest, &img); err != nil {
				return err
			}
			images = append(images, named{name, img})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("ocidir: image %q: %w", name, err)
		}
	}

	return func(yield func(string, ocispec.Image) bool) {
		for _, n := range images {
			if !yield(n.name, n.img) {
				return
			}
		}
	}, nil
}

// AllBlobsFromNames yields every blob the images named by refNames depend on.
// Empty refNames means every image in Index, including unnamed entries.
//
// See [AllBlobsFromDigests] for the order and failure semantics.
func AllBlobsFromNames(
	ctx context.Context,
	dir OciDir,
	refNames ...string,
) (iter.Seq2[Blob, error], error) {
	idx, err := dir.Index(ctx)
	if err != nil {
		return nil, err
	}

	roots := idx.Manifests
	if len(refNames) > 0 {
		roots = nil
		for _, name := range refNames {
			i := slices.IndexFunc(idx.Manifests, func(d ocispec.Descriptor) bool {
				return d.Annotations[ocispec.AnnotationRefName] == name
			})
			if i < 0 {
				return nil, fmt.Errorf("ocidir: ref name %q: %w", name, fs.ErrNotExist)
			}
			roots = append(roots, idx.Manifests[i])
		}
	}

	return blobsFrom(ctx, dir, roots)
}

// AllBlobsFromDigests yields every blob the images selected by digests depend on.
// A digest selects a descriptor in Index or in an image index reachable from it.
// Empty digests means every image in Index.
//
// Blobs are yielded once each, dependencies first:
// layers, config, manifest, then the image index referring to them.
// Unknown digests fail the call before the iterator is returned.
// Each yielded Reader is owned by the caller.
func AllBlobsFromDigests(
	ctx context.Context,
	dir OciDir,
	digests ...digest.Digest,
) (iter.Seq2[Blob, error], error) {
	idx, err := dir.Index(ctx)
	if err != nil {
		return nil, err
	}

	roots := idx.Manifests
	if len(digests) > 0 {
		known := map[digest.Digest]ocispec.Descriptor{}
		if err := collectDescriptors(ctx, dir, idx.Manifests, known); err != nil {
			return nil, err
		}
		roots = nil
		for _, dgst := range digests {
			desc, ok := known[dgst]
			if !ok {
				return nil, fmt.Errorf("ocidir: digest %q: %w", dgst, fs.ErrNotExist)
			}
			roots = append(roots, desc)
		}
	}

	return blobsFrom(ctx, dir, roots)
}

func blobsFrom(
	ctx context.Context,
	dir OciDir,
	roots []ocispec.Descriptor,
) (iter.Seq2[Blob, error], error) {
	seen := map[digest.Digest]bool{}
	var ordered []digest.Digest
	add := func(dgst digest.Digest) {
		if !seen[dgst] {
			seen[dgst] = true
			ordered = append(ordered, dgst)
		}
	}
	for _, root := range roots {
		if err := walkBlobs(ctx, dir, root, add); err != nil {
			return nil, err
		}
	}

	return func(yield func(Blob, error) bool) {
		for _, dgst := range ordered {
			r, err := dir.Blobs(ctx, dgst)
			if !yield(Blob{Name: dgst, Reader: r}, err) {
				return
			}
		}
	}, nil
}

// walkBlobs calls add for desc and all of its descendants, children before parents.
func walkBlobs(
	ctx context.Context,
	dir OciDir,
	desc ocispec.Descriptor,
	add func(digest.Digest),
) error {
	switch {
	case isIndex(desc.MediaType):
		var idx ocispec.Index
		if err := readJSON(ctx, dir, desc.Digest, &idx); err != nil {
			return err
		}
		for _, child := range idx.Manifests {
			if err := walkBlobs(ctx, dir, child, add); err != nil {
				return err
			}
		}
	case isManifest(desc.MediaType):
		var m ocispec.Manifest
		if err := readJSON(ctx, dir, desc.Digest, &m); err != nil {
			return err
		}
		for _, l := range m.Layers {
			add(l.Digest)
		}
		add(m.Config.Digest)
	}
	add(desc.Digest)
	return nil
}

// walkManifests calls fn for every image manifest reachable from desc.
func walkManifests(
	ctx context.Context,
	dir OciDir,
	desc ocispec.Descriptor,
	fn func(ocispec.Manifest) error,
) error {
	switch {
	case isIndex(desc.MediaType):
		var idx ocispec.Index
		if err := readJSON(ctx, dir, desc.Digest, &idx); err != nil {
			return err
		}
		for _, child := range idx.Manifests {
			if err := walkManifests(ctx, dir, child, fn); err != nil {
				return err
			}
		}
	case isManifest(desc.MediaType):
		var m ocispec.Manifest
		if err := readJSON(ctx, dir, desc.Digest, &m); err != nil {
			return err
		}
		return fn(m)
	}
	return nil
}

// collectDescriptors records descs and every descriptor of the image indexes reachable from them.
func collectDescriptors(
	ctx context.Context,
	dir OciDir,
	descs []ocispec.Descriptor,
	known map[digest.Digest]ocispec.Descriptor,
) error {
	for _, desc := range descs {
		if _, ok := known[desc.Digest]; ok {
			continue
		}
		known[desc.Digest] = desc
		if !isIndex(desc.MediaType) {
			continue
		}
		var idx ocispec.Index
		if err := readJSON(ctx, dir, desc.Digest, &idx); err != nil {
			return err
		}
		if err := collectDescriptors(ctx, dir, idx.Manifests, known); err != nil {
			return err
		}
	}
	return nil
}

func readJSON(ctx context.Context, dir OciDir, dgst digest.Digest, v any) error {
	r, err := dir.Blobs(ctx, dgst)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
	if err != nil {
		return fmt.Errorf("ocidir: reading blob %q: %w", dgst, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("ocidir: decoding blob %q: %w", dgst, err)
	}
	return nil
}
