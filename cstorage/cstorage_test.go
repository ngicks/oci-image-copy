package cstorage_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ngicks/go-fsys-helper/vroot/memfs"
	"github.com/ngicks/go-fsys-helper/vroot/osfs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/signature"
	istorage "go.podman.io/image/v5/storage"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/reexec"

	"github.com/ngicks/oci-image-copy/blobcache"
	"github.com/ngicks/oci-image-copy/cstorage"
	"github.com/ngicks/oci-image-copy/internal/ocidirtest"
	"github.com/ngicks/oci-image-copy/ocidir/fsdir"
	"github.com/ngicks/oci-image-copy/store"
	"github.com/ngicks/oci-image-copy/transfer"
	"github.com/ngicks/oci-image-copy/types"
)

func TestMain(m *testing.M) {
	if reexec.Init() {
		return
	}
	os.Exit(m.Run())
}

var amd64 = types.Platform{OS: "linux", Architecture: "amd64"}

func tarLayer(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range files {
			if !yield(k) {
				return
			}
		}
	}) {
		body := files[name]
		hdr := &tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)),
			ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg, Format: tar.FormatPAX,
		}
		if err := w.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// gz compresses b; a non-empty name lands in the gzip header, changing the blob but not the diffID.
func gz(t *testing.T, b []byte, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Name = name
	w.ModTime = time.Unix(0, 0)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type image struct {
	ref      types.Reference
	dir      string // oci-dir on disk
	manifest ocispec.Descriptor
	layers   []ocispec.Descriptor
	diffIDs  []digest.Digest
}

// build writes a single-image oci-dir whose index.json names it refName.
func build(t *testing.T, refName string, tars [][]byte, gzName string) image {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]byte{}
	put := func(mediaType string, b []byte) ocispec.Descriptor {
		d := digest.FromBytes(b)
		files[ocidirtest.BlobPath(d)] = b
		return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(b))}
	}
	img := image{dir: dir}
	for _, tb := range tars {
		img.diffIDs = append(img.diffIDs, digest.FromBytes(tb))
		img.layers = append(img.layers, put(ocispec.MediaTypeImageLayerGzip, gz(t, tb, gzName)))
	}
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cfg, _ := json.Marshal(ocispec.Image{
		Created: &created,
		OS:      "linux", Architecture: "amd64",
		RootFS: ocispec.RootFS{Type: "layers", DiffIDs: img.diffIDs},
	})
	cd := put(ocispec.MediaTypeImageConfig, cfg)
	m, _ := json.Marshal(ocispec.Manifest{
		SchemaVersion: 2,
		MediaType:     ocispec.MediaTypeImageManifest,
		Config:        cd,
		Layers:        img.layers,
	})
	img.manifest = put(ocispec.MediaTypeImageManifest, m)
	idx := img.manifest
	idx.Annotations = map[string]string{ocispec.AnnotationRefName: refName}
	files[ocispec.ImageIndexFile], _ = json.Marshal(ocispec.Index{
		SchemaVersion: 2,
		MediaType:     ocispec.MediaTypeImageIndex,
		Manifests:     []ocispec.Descriptor{idx},
	})
	files[ocispec.ImageLayoutFile], _ = json.Marshal(
		ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion},
	)

	root, err := osfs.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := ocidirtest.Write(root, files); err != nil {
		t.Fatal(err)
	}
	img.ref, err = types.ParseReference(refName)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func newVFS(t *testing.T) storage.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := storage.GetStore(storage.StoreOptions{
		GraphRoot:       filepath.Join(dir, "graph"),
		RunRoot:         filepath.Join(dir, "run"),
		GraphDriverName: "vfs",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.Shutdown(true) })
	return st
}

func newCache(t *testing.T) *blobcache.Cache {
	t.Helper()
	c, err := blobcache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// copyIn imports img with go.podman.io/image's copy.Image, as podman pull would.
func copyIn(t *testing.T, st storage.Store, img image) {
	t.Helper()
	src, err := layout.NewReference(img.dir, "")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := istorage.Transport.ParseStoreReference(st, img.ref.String())
	if err != nil {
		t.Fatal(err)
	}
	pc, err := signature.NewPolicyContext(&signature.Policy{
		Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Destroy() }()
	if _, err := copy.Image(t.Context(), pc, dst, src, &copy.Options{}); err != nil {
		t.Fatal(err)
	}
}

// storeWith pushes imgs into a fresh in-memory store.
func storeWith(t *testing.T, imgs ...image) *store.Store {
	t.Helper()
	s := store.New(fsdir.NewStore(memfs.New("store")))
	for _, img := range imgs {
		root, err := osfs.NewRoot(img.dir)
		if err != nil {
			t.Fatal(err)
		}
		src := transfer.FromOciDir(fsdir.New(root, nil))
		set := types.ImageSetManifest{Images: []string{img.ref.String()}}
		plan, err := transfer.PlanPush(t.Context(), src, s, set, []types.Platform{amd64})
		if err == nil {
			err = transfer.Push(t.Context(), plan, s, nil)
		}
		_ = root.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// record renders an image record and its layer chain for comparison.
func record(t *testing.T, st storage.Store, name string) string {
	t.Helper()
	img, err := st.Image(name)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := st.ListImageBigData(img.ID)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(keys)
	digests := slices.Clone(img.Digests)
	slices.Sort(digests)
	var b strings.Builder
	fmt.Fprintf(
		&b,
		"id=%s top=%s digest=%s digests=%v bigdata=%v metadata=%q created=%s\n",
		img.ID,
		img.TopLayer,
		img.Digest,
		digests,
		keys,
		img.Metadata,
		img.Created.UTC().Format(time.RFC3339),
	)
	for id := img.TopLayer; id != ""; {
		l, err := st.Layer(id)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "layer %s parent=%s compressed=%s uncompressed=%s\n",
			l.ID, l.Parent, l.CompressedDigest, l.UncompressedDigest)
		id = l.Parent
	}
	return b.String()
}

func pull(t *testing.T, cs *cstorage.Storage, src *store.Store, refs ...string) transfer.Plan {
	t.Helper()
	plan, err := cs.PlanPull(t.Context(), src, types.ImageSetManifest{Images: refs}, amd64)
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.Pull(t.Context(), plan, src, nil); err != nil {
		t.Fatal(err)
	}
	return plan
}

var (
	baseFiles    = []map[string]string{{"etc/os-release": "base\n"}, {"usr/bin/tool": "tool\n"}}
	derivedFiles = map[string]string{"app/main": "app\n"}
)

func TestPull_parity(t *testing.T) {
	base := build(
		t,
		"example.com/base:1",
		[][]byte{tarLayer(t, baseFiles[0]), tarLayer(t, baseFiles[1])},
		"",
	)
	src := storeWith(t, base)

	ours := newVFS(t)
	pull(t, cstorage.New(ours, newCache(t), nil), src, base.ref.String())
	theirs := newVFS(t)
	copyIn(t, theirs, base)

	if got, want := record(
		t,
		ours,
		base.ref.String(),
	), record(
		t,
		theirs,
		base.ref.String(),
	); got != want {
		t.Errorf("image record differs from copy.Image:\nours:\n%s\ncopy.Image:\n%s", got, want)
	}
}

func TestPull_skipsPresentPrefix(t *testing.T) {
	baseTars := [][]byte{tarLayer(t, baseFiles[0]), tarLayer(t, baseFiles[1])}
	derived := build(
		t,
		"example.com/app:1",
		append(slices.Clone(baseTars), tarLayer(t, derivedFiles)),
		"",
	)

	for _, tc := range []struct {
		name   string
		gzName string // the host's copy of the base compressed differently when non-empty
	}{
		{"same blobs", ""},
		{"same diffIDs, other compressed blobs", "other-compressor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostBase := build(t, "example.com/base:1", baseTars, tc.gzName)
			if tc.gzName != "" && hostBase.layers[0].Digest == derived.layers[0].Digest {
				t.Fatal("fixture: alternative compression produced the same blob")
			}
			st := newVFS(t)
			copyIn(t, st, hostBase)

			cache := newCache(t)
			cs := cstorage.New(st, cache, nil)
			plan := pull(t, cs, storeWith(t, derived), derived.ref.String())

			e := plan.Entries[0]
			if len(e.Present) != 2 || len(e.Missing) != 1 ||
				e.Missing[0].Digest != derived.layers[2].Digest {
				t.Fatalf(
					"plan: present %d, missing %v; want the 2 base layers present and only the top missing",
					len(e.Present),
					e.Missing,
				)
			}
			for _, l := range derived.layers[:2] {
				if cache.Has(l.Digest) {
					t.Errorf("base layer %s was downloaded", l.Digest)
				}
			}
			img, err := st.Image(derived.ref.String())
			if err != nil {
				t.Fatal(err)
			}
			if img.ID != derived.manifestConfig(t).Encoded() {
				t.Errorf("image ID %s, want the config digest", img.ID)
			}
		})
	}
}

func (img image) manifestConfig(t *testing.T) digest.Digest {
	t.Helper()
	b, err := os.ReadFile(
		filepath.Join(img.dir, filepath.FromSlash(ocidirtest.BlobPath(img.manifest.Digest))),
	)
	if err != nil {
		t.Fatal(err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m.Config.Digest
}

func TestPull_fallback(t *testing.T) {
	base := build(t, "example.com/base:1", [][]byte{tarLayer(t, baseFiles[0])}, "")
	root, err := osfs.NewRoot(base.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	src := store.New(fsdir.NewStore(memfs.New("store"))) // lacks the image

	st := newVFS(t)
	cs := cstorage.New(
		st,
		newCache(t),
		&cstorage.Options{Fallback: transfer.FromOciDir(fsdir.New(root, nil))},
	)
	plan := pull(t, cs, src, base.ref.String())
	if !plan.Entries[0].Fallback {
		t.Error("entry not marked as resolved through the fallback")
	}
	if _, err := st.Image(base.ref.String()); err != nil {
		t.Errorf("image not imported: %v", err)
	}
	if img, err := src.Resolve(
		t.Context(),
		base.ref,
		amd64,
	); err != nil ||
		img.Manifest.Digest != base.manifest.Digest {
		t.Errorf("store did not keep the fallback image: %v, %v", img.Manifest.Digest, err)
	}

	missing := cstorage.New(newVFS(t), newCache(t), nil)
	if _, err := missing.PlanPull(
		t.Context(),
		src,
		types.ImageSetManifest{Images: []string{"example.com/none:1"}},
		amd64,
	); err == nil {
		t.Error("PlanPull of an absent entry without fallback: want error")
	}
}

func TestPush(t *testing.T) {
	base := build(
		t,
		"example.com/base:1",
		[][]byte{tarLayer(t, baseFiles[0]), tarLayer(t, baseFiles[1])},
		"",
	)
	ctx := t.Context()

	pushFrom := func(
		t *testing.T,
		cs *cstorage.Storage,
		ref types.Reference,
	) (store.Image, *store.Store) {
		t.Helper()
		dst := store.New(fsdir.NewStore(memfs.New("dst")))
		set := types.ImageSetManifest{Images: []string{ref.String()}}
		plan, err := transfer.PlanPush(ctx, cs.PushSource(dst), dst, set, []types.Platform{amd64})
		if err != nil {
			t.Fatal(err)
		}
		if err := transfer.Push(ctx, plan, dst, nil); err != nil {
			t.Fatal(err)
		}
		img, err := dst.Resolve(ctx, ref, amd64)
		if err != nil {
			t.Fatal(err)
		}
		return img, dst
	}

	t.Run("as-is after a pull through the library", func(t *testing.T) {
		st := newVFS(t)
		cs := cstorage.New(st, newCache(t), nil)
		pull(t, cs, storeWith(t, base), base.ref.String())
		img, _ := pushFrom(t, cs, base.ref)
		if img.Manifest.Digest != base.manifest.Digest {
			t.Errorf(
				"pushed manifest %s, want the original %s",
				img.Manifest.Digest,
				base.manifest.Digest,
			)
		}
	})

	t.Run("lost blobs re-compress with zstd", func(t *testing.T) {
		st := newVFS(t)
		copyIn(t, st, base) // the cache never saw the original blobs
		img, dst := pushFrom(t, cstorage.New(st, newCache(t), nil), base.ref)
		if img.Manifest.Digest == base.manifest.Digest {
			t.Fatal("pushed the original manifest although its blobs were unreachable")
		}
		m := manifestOf(t, dst, img)
		for _, l := range m.Layers {
			if l.MediaType != ocispec.MediaTypeImageLayerZstd {
				t.Errorf("layer %s is %s, want zstd", l.Digest, l.MediaType)
			}
		}

		// The re-compressed image pulls back with the same diffIDs and image ID.
		back := newVFS(t)
		pull(t, cstorage.New(back, newCache(t), nil), dst, base.ref.String())
		got, err := back.Image(base.ref.String())
		if err != nil {
			t.Fatal(err)
		}
		if want := base.manifestConfig(t).Encoded(); got.ID != want {
			t.Errorf("image ID after round trip %s, want %s", got.ID, want)
		}
	})

	t.Run("lost blobs reuse a stored variant by diffID", func(t *testing.T) {
		tars := [][]byte{tarLayer(t, baseFiles[0]), tarLayer(t, baseFiles[1])}
		alt := build(t, "example.com/base-alt:1", tars, "alt-compressor")
		dst := storeWith(t, alt) // holds the base diffIDs under other gzip blobs

		st := newVFS(t)
		copyIn(t, st, base) // original blobs unreachable
		cs := cstorage.New(st, newCache(t), nil)
		set := types.ImageSetManifest{Images: []string{base.ref.String()}}
		plan, err := transfer.PlanPush(ctx, cs.PushSource(dst), dst, set, []types.Platform{amd64})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range plan.Entries[0].Missing {
			if d.MediaType == ocispec.MediaTypeImageLayerGzip ||
				d.MediaType == ocispec.MediaTypeImageLayerZstd {
				t.Errorf("layer %s (%s) would be uploaded", d.Digest, d.MediaType)
			}
		}
		if err := transfer.Push(ctx, plan, dst, nil); err != nil {
			t.Fatal(err)
		}
		img, err := dst.Resolve(ctx, base.ref, amd64)
		if err != nil {
			t.Fatal(err)
		}
		m := manifestOf(t, dst, img)
		for i, l := range m.Layers {
			if l.Digest != alt.layers[i].Digest || l.MediaType != ocispec.MediaTypeImageLayerGzip {
				t.Errorf(
					"layer %d = %s (%s), want the stored variant %s",
					i,
					l.Digest,
					l.MediaType,
					alt.layers[i].Digest,
				)
			}
		}
	})

	t.Run("local layer compresses with zstd", func(t *testing.T) {
		st := newVFS(t)
		cs := cstorage.New(st, newCache(t), nil)
		pull(t, cs, storeWith(t, base), base.ref.String())
		local := commitLocal(t, st, base)
		img, dst := pushFrom(t, cs, local)
		m := manifestOf(t, dst, img)
		if len(m.Layers) != 3 {
			t.Fatalf("pushed %d layers, want 3", len(m.Layers))
		}
		for i, l := range m.Layers[:2] {
			if l.Digest != base.layers[i].Digest {
				t.Errorf(
					"base layer %d pushed as %s, want the original %s",
					i,
					l.Digest,
					base.layers[i].Digest,
				)
			}
		}
		if m.Layers[2].MediaType != ocispec.MediaTypeImageLayerZstd {
			t.Errorf("local layer is %s, want zstd", m.Layers[2].MediaType)
		}
	})
}

// TestIndexPin_roundTrip pulls an index-pin alias leaf and pushes it back out under the same pin.
func TestIndexPin_roundTrip(t *testing.T) {
	ctx := t.Context()
	base := build(t, "example.com/base:1", [][]byte{tarLayer(t, baseFiles[0])}, "")
	src := storeWith(t, base)
	m := base.manifest
	m.Platform = &ocispec.Platform{OS: "linux", Architecture: "amd64"}
	idx, _ := json.Marshal(ocispec.Index{
		SchemaVersion: 2,
		MediaType:     ocispec.MediaTypeImageIndex,
		Manifests:     []ocispec.Descriptor{m},
	})
	pin := types.Reference{Repository: "example.com/base", Digest: digest.FromBytes(idx)}
	if err := src.Commit(ctx, pin, amd64, base.manifest); err != nil {
		t.Fatal(err)
	}

	st := newVFS(t)
	cs := cstorage.New(st, newCache(t), nil)
	pull(t, cs, src, pin.String())
	rec, err := st.Image(pin.String())
	if err != nil {
		t.Fatalf("pulled image not found by its index pin: %v", err)
	}
	if !slices.Contains(rec.Digests, base.manifest.Digest) {
		t.Errorf("Digests %v lack the manifest digest", rec.Digests)
	}

	dst := store.New(fsdir.NewStore(memfs.New("dst")))
	set := types.ImageSetManifest{Images: []string{pin.String()}}
	plan, err := transfer.PlanPush(ctx, cs.PushSource(dst), dst, set, []types.Platform{amd64})
	if err != nil {
		t.Fatal(err)
	}
	if err := transfer.Push(ctx, plan, dst, nil); err != nil {
		t.Fatal(err)
	}
	img, err := dst.Resolve(ctx, pin, amd64)
	if err != nil || img.Manifest.Digest != base.manifest.Digest {
		t.Errorf("pushed index pin: %v, %v; want the original manifest", img.Manifest.Digest, err)
	}
}

func manifestOf(t *testing.T, s *store.Store, img store.Image) ocispec.Manifest {
	t.Helper()
	r, err := s.Blob(t.Context(), img.Manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var m ocispec.Manifest
	if err := json.NewDecoder(io.NewSectionReader(r, 0, r.Size())).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// commitLocal adds a never-compressed layer on top of base, recorded the way a local build records
// it:
// the manifest lists the layer by its diffID with the uncompressed media type.
func commitLocal(t *testing.T, st storage.Store, base image) types.Reference {
	t.Helper()
	tb := tarLayer(t, derivedFiles)
	baseRec, err := st.Image(base.ref.String())
	if err != nil {
		t.Fatal(err)
	}
	diffID := digest.FromBytes(tb)
	id := digest.Canonical.FromString(baseRec.TopLayer + "+" + diffID.Encoded()).Encoded()
	if _, _, err := st.PutLayer(
		id,
		baseRec.TopLayer,
		nil,
		"",
		false,
		nil,
		bytes.NewReader(tb),
	); err != nil {
		t.Fatal(err)
	}
	diffIDs := append(slices.Clone(base.diffIDs), diffID)
	cfg, _ := json.Marshal(ocispec.Image{
		OS: "linux", Architecture: "amd64",
		RootFS: ocispec.RootFS{Type: "layers", DiffIDs: diffIDs},
	})
	cd := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.FromBytes(cfg),
		Size:      int64(len(cfg)),
	}
	var layers []ocispec.Descriptor
	for _, d := range diffIDs {
		layers = append(
			layers,
			ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Digest: d},
		)
	}
	m, _ := json.Marshal(ocispec.Manifest{
		SchemaVersion: 2,
		MediaType:     ocispec.MediaTypeImageManifest,
		Config:        cd,
		Layers:        layers,
	})
	md := digest.FromBytes(m)
	_, err = st.CreateImage(
		cd.Digest.Encoded(),
		[]string{"localhost/local:1"},
		id,
		"",
		&storage.ImageOptions{
			BigData: []storage.ImageBigDataOption{
				{Key: cd.Digest.String(), Data: cfg, Digest: cd.Digest},
				{
					Key:    storage.ImageDigestManifestBigDataNamePrefix + "-" + md.String(),
					Data:   m,
					Digest: md,
				},
				{Key: storage.ImageDigestBigDataKey, Data: m, Digest: md},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := types.ParseReference("localhost/local:1")
	return ref
}
