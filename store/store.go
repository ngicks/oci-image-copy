// Package store keeps many single-platform images in one logical tree:
//
//	shared/blobs/<alg>/<hex>
//	image_refs/<repo>/_tag/<tag>/<os>/<arch>[/<variant>]/{oci-layout,index.json[,blobs/]}
//	image_refs/<repo>/_digest/<alg>/<hex>/<os>/<arch>[/<variant>]/{oci-layout,index.json[,blobs/]}
//
// Each leaf is an OCI image layout whose index.json points at one image manifest,
// and whose blobs live in shared/blobs.
// Every digest a stored file refers to is present in the store, and leaves never change once
// written.
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"path"
	"slices"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ngicks/oci-image-copy/ocidir"
	"github.com/ngicks/oci-image-copy/types"
)

var (
	ErrConflict     = errors.New("store: leaf exists with a different manifest")
	ErrNotListable  = errors.New("store: backend cannot list")
	ErrForeignLayer = errors.New("store: non-distributable layer")
	ErrPlatform     = errors.New("store: config does not match the named platform")
	ErrAmbiguous    = errors.New("store: several index entries match the platform")
)

// Backend reads and writes entries of the logical tree by slash-separated path,
// e.g. "shared/blobs/sha256/<hex>" or
// "image_refs/docker.io/library/ubuntu/_tag/26.04/linux/amd64/index.json".
// Missing entries return an error wrapping fs.ErrNotExist.
type Backend interface {
	Open(ctx context.Context, name string) (ocidir.ReadAtCloser, error)
	// Create writes name only when it is absent and returns an error wrapping fs.ErrExist
	// otherwise.
	// A non-empty dgst is verified against the written content.
	Create(ctx context.Context, name string, r io.Reader, size int64, dgst digest.Digest) error
}

// Lister is implemented by backends that can list, used by Walk.
// List yields the names of all entries under prefix, recursively.
type Lister interface {
	List(ctx context.Context, prefix string) iter.Seq2[string, error]
}

// DirMaker is implemented by backends with real directories; Commit uses it for the leaf's empty
// blobs/.
type DirMaker interface {
	MkdirAll(ctx context.Context, name string) error
}

const (
	sharedBlobsDir = "shared/blobs"
	imageRefsDir   = "image_refs"
	tagMarker      = "_tag"
	digestMarker   = "_digest"
)

func BlobPath(dgst digest.Digest) string {
	return path.Join(sharedBlobsDir, dgst.Algorithm().String(), dgst.Encoded())
}

// LeafPath returns the leaf directory for ref and p; a ref with a digest uses the digest.
func LeafPath(ref types.Reference, p types.Platform) string {
	key := path.Join(tagMarker, ref.Tag)
	if ref.Digest != "" {
		key = path.Join(digestMarker, ref.Digest.Algorithm().String(), ref.Digest.Encoded())
	}
	return path.Join(imageRefsDir, ref.Repository, key, p.String())
}

// Image is one single-platform image.
type Image struct {
	Ref      types.Reference
	Platform types.Platform
	Manifest ocispec.Descriptor
	Dir      ocidir.OciDir // index.json points at Manifest
}

type Store struct {
	b Backend
}

func New(b Backend) *Store {
	return &Store{b: b}
}

func (s *Store) HasBlob(ctx context.Context, dgst digest.Digest) (bool, error) {
	r, err := s.Blob(ctx, dgst)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_ = r.Close()
	return true, nil
}

func (s *Store) Blob(ctx context.Context, dgst digest.Digest) (ocidir.ReadAtCloser, error) {
	if err := dgst.Validate(); err != nil {
		return nil, err
	}
	return s.b.Open(ctx, BlobPath(dgst))
}

// PutBlob stores r under desc unless the blob already exists; the backend verifies the digest.
func (s *Store) PutBlob(ctx context.Context, desc ocispec.Descriptor, r io.Reader) error {
	if err := desc.Digest.Validate(); err != nil {
		return err
	}
	if ok, err := s.HasBlob(ctx, desc.Digest); err != nil || ok {
		return err
	}
	err := s.b.Create(ctx, BlobPath(desc.Digest), r, desc.Size, desc.Digest)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	return err
}

func (s *Store) readAll(ctx context.Context, name string) ([]byte, error) {
	r, err := s.b.Open(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
}

func (s *Store) readJSON(ctx context.Context, dgst digest.Digest, v any) ([]byte, error) {
	b, err := s.readAll(ctx, BlobPath(dgst))
	if err != nil {
		return nil, err
	}
	if digest.FromBytes(b) != dgst {
		return nil, fmt.Errorf("store: blob %s does not match its digest", dgst)
	}
	return b, json.Unmarshal(b, v)
}

func isManifest(mediaType string) bool {
	return mediaType == ocispec.MediaTypeImageManifest ||
		mediaType == "application/vnd.docker.distribution.manifest.v2+json"
}

// isForeign spells out the media types because the image-spec constants for them are deprecated,
// and these layers must still be recognized to be refused.
func isForeign(mediaType string) bool {
	switch mediaType {
	case "application/vnd.oci.image.layer.nondistributable.v1.tar",
		"application/vnd.oci.image.layer.nondistributable.v1.tar+gzip",
		"application/vnd.oci.image.layer.nondistributable.v1.tar+zstd",
		"application/vnd.docker.image.rootfs.foreign.diff.tar.gzip":
		return true
	}
	return false
}

// Commit writes the leaf for ref and p pointing at manifest.
//
// It checks that the manifest, its config and every layer are stored, refuses non-distributable
// layers,
// and refuses a config whose platform does not match p.
// Committing an existing leaf with the same manifest is a no-op; a different manifest returns
// ErrConflict.
func (s *Store) Commit(
	ctx context.Context,
	ref types.Reference,
	p types.Platform,
	manifest ocispec.Descriptor,
) error {
	if !isManifest(manifest.MediaType) {
		return fmt.Errorf("store: %s: media type %q is not a single-platform image manifest",
			manifest.Digest, manifest.MediaType)
	}
	var m ocispec.Manifest
	if _, err := s.readJSON(ctx, manifest.Digest, &m); err != nil {
		return fmt.Errorf("store: manifest %s: %w", manifest.Digest, err)
	}
	for _, l := range m.Layers {
		if isForeign(l.MediaType) {
			return fmt.Errorf("%w: %s (%s)", ErrForeignLayer, l.Digest, l.MediaType)
		}
	}
	var cfg ocispec.Image
	if _, err := s.readJSON(ctx, m.Config.Digest, &cfg); err != nil {
		return fmt.Errorf("store: config %s: %w", m.Config.Digest, err)
	}
	if !p.Matches(cfg.Platform) {
		return fmt.Errorf("%w: named %s, config says %s/%s/%s",
			ErrPlatform, p, cfg.OS, cfg.Architecture, cfg.Variant)
	}
	for _, l := range m.Layers {
		ok, err := s.HasBlob(ctx, l.Digest)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf(
				"store: layer %s of manifest %s: %w",
				l.Digest,
				manifest.Digest,
				fs.ErrNotExist,
			)
		}
	}

	leaf := LeafPath(ref, p)
	if err := s.createOnce(
		ctx,
		path.Join(leaf, ocispec.ImageLayoutFile),
		layoutJSON(),
	); err != nil {
		return err
	}
	if dm, ok := s.b.(DirMaker); ok {
		if err := dm.MkdirAll(ctx, path.Join(leaf, ocispec.ImageBlobsDir)); err != nil {
			return err
		}
	}
	index, err := indexJSON(ref, p, manifest)
	if err != nil {
		return err
	}
	name := path.Join(leaf, ocispec.ImageIndexFile)
	err = s.b.Create(ctx, name, bytes.NewReader(index), int64(len(index)), "")
	if !errors.Is(err, fs.ErrExist) {
		return err
	}
	existing, err := s.leafManifest(ctx, leaf)
	if err != nil {
		return err
	}
	if existing.Digest != manifest.Digest {
		return fmt.Errorf(
			"%w: %s holds %s, not %s",
			ErrConflict,
			leaf,
			existing.Digest,
			manifest.Digest,
		)
	}
	return nil
}

func (s *Store) createOnce(ctx context.Context, name string, b []byte) error {
	err := s.b.Create(ctx, name, bytes.NewReader(b), int64(len(b)), "")
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	return err
}

func layoutJSON() []byte {
	b, _ := json.Marshal(ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion})
	return b
}

func indexJSON(ref types.Reference, p types.Platform, manifest ocispec.Descriptor) ([]byte, error) {
	desc := ocispec.Descriptor{
		MediaType: manifest.MediaType,
		Digest:    manifest.Digest,
		Size:      manifest.Size,
		Platform:  &ocispec.Platform{OS: p.OS, Architecture: p.Architecture, Variant: p.Variant},
	}
	if ref.Digest == "" {
		desc.Annotations = map[string]string{ocispec.AnnotationRefName: ref.Tag}
	}
	return json.Marshal(ocispec.Index{
		SchemaVersion: 2,
		MediaType:     ocispec.MediaTypeImageIndex,
		Manifests:     []ocispec.Descriptor{desc},
	})
}

func (s *Store) leafManifest(ctx context.Context, leaf string) (ocispec.Descriptor, error) {
	b, err := s.readAll(ctx, path.Join(leaf, ocispec.ImageIndexFile))
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	var idx ocispec.Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("store: %s/index.json: %w", leaf, err)
	}
	if len(idx.Manifests) != 1 {
		return ocispec.Descriptor{}, fmt.Errorf("store: %s/index.json holds %d manifests, want 1",
			leaf, len(idx.Manifests))
	}
	return idx.Manifests[0], nil
}

// Resolve finds the leaf for ref and p.
// A manifest-digest ref without a leaf resolves from shared/blobs; that path trusts the write order
// (layers, config, manifest blob) and does not re-check the config and layers.
func (s *Store) Resolve(ctx context.Context, ref types.Reference, p types.Platform) (Image, error) {
	leaf := LeafPath(ref, p)
	manifest, err := s.leafManifest(ctx, leaf)
	if err == nil {
		return s.image(ref, p, manifest, leaf), nil
	}
	if !errors.Is(err, fs.ErrNotExist) || ref.Digest == "" {
		return Image{}, fmt.Errorf("store: resolving %s for %s: %w", ref, p, err)
	}

	var m struct {
		MediaType string `json:"mediaType"`
	}
	b, berr := s.readJSON(ctx, ref.Digest, &m)
	if berr != nil || !isManifest(m.MediaType) {
		return Image{}, fmt.Errorf("store: resolving %s for %s: %w", ref, p, err)
	}
	manifest = ocispec.Descriptor{MediaType: m.MediaType, Digest: ref.Digest, Size: int64(len(b))}
	return s.image(ref, p, manifest, ""), nil
}

func (s *Store) image(
	ref types.Reference,
	p types.Platform,
	manifest ocispec.Descriptor,
	leaf string,
) Image {
	var dir ocidir.OciDir = &leafDir{s: s, leaf: leaf}
	if leaf == "" {
		dir = SingleManifestDir(s.Blob, p, manifest)
	}
	return Image{Ref: ref, Platform: p, Manifest: manifest, Dir: dir}
}

// Walk yields every leaf; it yields ErrNotListable when the backend cannot list.
func (s *Store) Walk(ctx context.Context) iter.Seq2[Image, error] {
	return func(yield func(Image, error) bool) {
		l, ok := s.b.(Lister)
		if !ok {
			yield(Image{}, ErrNotListable)
			return
		}
		for name, err := range l.List(ctx, imageRefsDir+"/") {
			if err != nil {
				if !yield(Image{}, err) {
					return
				}
				continue
			}
			leaf, ok := strings.CutSuffix(name, "/"+ocispec.ImageIndexFile)
			if !ok {
				continue
			}
			ref, p, err := parseLeaf(leaf)
			if err != nil {
				continue // not a leaf of this tree
			}
			manifest, err := s.leafManifest(ctx, leaf)
			if err != nil {
				if !yield(Image{}, err) {
					return
				}
				continue
			}
			if !yield(s.image(ref, p, manifest, leaf), nil) {
				return
			}
		}
	}
}

// parseLeaf inverts LeafPath. Repository components start with [a-z0-9],
// so the first _tag or _digest segment is the marker.
func parseLeaf(leaf string) (types.Reference, types.Platform, error) {
	rest, ok := strings.CutPrefix(leaf, imageRefsDir+"/")
	if !ok {
		return types.Reference{}, types.Platform{}, errors.New("not under image_refs")
	}
	segs := strings.Split(rest, "/")
	i := slices.IndexFunc(segs, func(s string) bool { return s == tagMarker || s == digestMarker })
	if i < 1 {
		return types.Reference{}, types.Platform{}, errors.New("no marker")
	}
	ref := types.Reference{Repository: strings.Join(segs[:i], "/")}
	var platformSegs []string
	switch segs[i] {
	case tagMarker:
		if len(segs) < i+2 {
			return types.Reference{}, types.Platform{}, errors.New("short tag leaf")
		}
		ref.Tag = segs[i+1]
		platformSegs = segs[i+2:]
	case digestMarker:
		if len(segs) < i+3 {
			return types.Reference{}, types.Platform{}, errors.New("short digest leaf")
		}
		ref.Digest = digest.NewDigestFromEncoded(digest.Algorithm(segs[i+1]), segs[i+2])
		if err := ref.Digest.Validate(); err != nil {
			return types.Reference{}, types.Platform{}, err
		}
		platformSegs = segs[i+3:]
	}
	p, err := types.ParsePlatform(strings.Join(platformSegs, "/"))
	return ref, p, err
}

// leafDir is the ocidir view of one leaf.
type leafDir struct {
	s    *Store
	leaf string
}

var _ ocidir.OciDir = (*leafDir)(nil)

func (d *leafDir) Blobs(ctx context.Context, dgst digest.Digest) (ocidir.ReadAtCloser, error) {
	return d.s.Blob(ctx, dgst)
}

func (d *leafDir) Index(ctx context.Context) (ocispec.Index, error) {
	var idx ocispec.Index
	b, err := d.IndexBin(ctx)
	if err != nil {
		return idx, err
	}
	return idx, json.Unmarshal(b, &idx)
}

func (d *leafDir) OciLayout(ctx context.Context) (ocispec.ImageLayout, error) {
	var l ocispec.ImageLayout
	b, err := d.OciLayoutBin(ctx)
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(b, &l)
}

func (d *leafDir) IndexBin(ctx context.Context) ([]byte, error) {
	return d.s.readAll(ctx, path.Join(d.leaf, ocispec.ImageIndexFile))
}

func (d *leafDir) OciLayoutBin(ctx context.Context) ([]byte, error) {
	return d.s.readAll(ctx, path.Join(d.leaf, ocispec.ImageLayoutFile))
}

// BlobOpener opens blobs by digest, e.g. (*Store).Blob or (*blobcache.Cache).Open.
type BlobOpener func(ctx context.Context, dgst digest.Digest) (ocidir.ReadAtCloser, error)

// SingleManifestDir views blobs as an oci-dir whose index.json holds only manifest, for platform p.
func SingleManifestDir(
	blobs BlobOpener,
	p types.Platform,
	manifest ocispec.Descriptor,
) ocidir.OciDir {
	return &singleManifestDir{blobs: blobs, platform: p, manifest: manifest}
}

type singleManifestDir struct {
	blobs    BlobOpener
	platform types.Platform
	manifest ocispec.Descriptor
}

func (d *singleManifestDir) Blobs(
	ctx context.Context,
	dgst digest.Digest,
) (ocidir.ReadAtCloser, error) {
	return d.blobs(ctx, dgst)
}

func (d *singleManifestDir) Index(ctx context.Context) (ocispec.Index, error) {
	var idx ocispec.Index
	b, err := d.IndexBin(ctx)
	if err != nil {
		return idx, err
	}
	return idx, json.Unmarshal(b, &idx)
}

func (d *singleManifestDir) OciLayout(context.Context) (ocispec.ImageLayout, error) {
	return ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion}, nil
}

func (d *singleManifestDir) IndexBin(context.Context) ([]byte, error) {
	return indexJSON(types.Reference{Digest: d.manifest.Digest}, d.platform, d.manifest)
}

func (d *singleManifestDir) OciLayoutBin(context.Context) ([]byte, error) {
	return layoutJSON(), nil
}
