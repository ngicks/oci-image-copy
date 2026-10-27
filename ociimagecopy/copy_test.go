package ociimagecopy_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngicks/go-fsys-helper/vroot/osfs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/reexec"

	"github.com/ngicks/oci-image-copy/ocidir/fsdir"
	"github.com/ngicks/oci-image-copy/ociimagecopy"
	"github.com/ngicks/oci-image-copy/store"
	"github.com/ngicks/oci-image-copy/types"
)

func TestMain(m *testing.M) {
	if reexec.Init() {
		return
	}
	os.Exit(m.Run())
}

var amd64 = types.Platform{OS: "linux", Architecture: "amd64"}

type noFallback struct{}

func (noFallback) Resolve(
	_ context.Context,
	ref types.Reference,
	p types.Platform,
) (store.Image, error) {
	return store.Image{}, fmt.Errorf("no fallback for %s %s: %w", ref, p, fs.ErrNotExist)
}

// newService returns a Service over the vfs containers-storage under graph, with a blob cache of
// its own.
func newService(t *testing.T, graph string) *ociimagecopy.Service {
	t.Helper()
	cfg := ociimagecopy.DefaultConfig()
	cfg.CacheDir = t.TempDir()
	svc, err := ociimagecopy.New(cfg, &ociimagecopy.Options{
		AdjustStorage: func(o *storage.StoreOptions) {
			*o = vfsOptions(graph)
		},
		Fallback: noFallback{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func vfsOptions(graph string) storage.StoreOptions {
	return storage.StoreOptions{
		GraphRoot:       filepath.Join(graph, "graph"),
		RunRoot:         filepath.Join(graph, "run"),
		GraphDriverName: "vfs",
	}
}

// images lists the images under graph; the Services using graph must be closed.
func images(t *testing.T, graph string) []storage.Image {
	t.Helper()
	st, err := storage.GetStore(vfsOptions(graph))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = st.Shutdown(true) }()
	imgs, err := st.Images()
	if err != nil {
		t.Fatal(err)
	}
	return imgs
}

func tarLayer(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	if err := w.WriteHeader(&tar.Header{
		Name: name, Mode: 0o644, Size: int64(len(body)),
		ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg, Format: tar.FormatPAX,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// storeDir creates a filesystem store holding ref with one uncompressed tar layer and returns its
// location and the config digest (the containers-storage image ID).
func storeDir(t *testing.T, ref string) (ociimagecopy.Location, digest.Digest) {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	root, err := osfs.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	st := store.New(fsdir.NewStore(root))
	put := func(mediaType string, b []byte) ocispec.Descriptor {
		d := ocispec.Descriptor{
			MediaType: mediaType,
			Digest:    digest.FromBytes(b),
			Size:      int64(len(b)),
		}
		if err := st.PutBlob(ctx, d, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
		return d
	}
	layer := tarLayer(t, "etc/hello", ref+"\n")
	ld := put(ocispec.MediaTypeImageLayer, layer)
	cfg, _ := json.Marshal(ocispec.Image{
		OS:           "linux",
		Architecture: "amd64",
		RootFS:       ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{ld.Digest}},
	})
	cd := put(ocispec.MediaTypeImageConfig, cfg)
	m, _ := json.Marshal(ocispec.Manifest{
		SchemaVersion: 2,
		MediaType:     ocispec.MediaTypeImageManifest,
		Config:        cd,
		Layers:        []ocispec.Descriptor{ld},
	})
	md := put(ocispec.MediaTypeImageManifest, m)
	r, err := types.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Commit(ctx, r, amd64, md); err != nil {
		t.Fatal(err)
	}
	loc, err := ociimagecopy.ParseLocation("store:" + dir)
	if err != nil {
		t.Fatal(err)
	}
	return loc, cd.Digest
}

func emptyStore(t *testing.T) ociimagecopy.Location {
	t.Helper()
	loc, err := ociimagecopy.ParseLocation("store:" + filepath.Join(t.TempDir(), "new"))
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

var cs = ociimagecopy.Location{Kind: ociimagecopy.KindContainersStorage}

func set(refs ...string) types.ImageSetManifest {
	return types.ImageSetManifest{Images: refs}
}

func manifestLayers(
	t *testing.T,
	svc *ociimagecopy.Service,
	loc ociimagecopy.Location,
) []ocispec.Descriptor {
	t.Helper()
	var out []ocispec.Descriptor
	for img, err := range svc.List(t.Context(), loc) {
		if err != nil {
			t.Fatal(err)
		}
		r, err := img.Dir.Blobs(t.Context(), img.Manifest.Digest)
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, r.Size())
		_, err = r.ReadAt(b, 0)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		var m ocispec.Manifest
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m.Layers...)
	}
	return out
}

func TestCopy_roundTrip(t *testing.T) {
	ctx := t.Context()
	const ref = "example.com/hello:1"
	src, imageId := storeDir(t, ref)
	opt := ociimagecopy.CopyOptions{Platforms: []types.Platform{amd64}}

	first := t.TempDir()
	svc := newService(t, first)
	res, err := svc.Copy(ctx, src, cs, set(ref), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Missing == 0 || res.Entries[0].Err != nil {
		t.Fatalf("pull result %+v, want one entry with missing bytes", res.Entries)
	}
	res, err = svc.Copy(ctx, src, cs, set(ref), opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Entries[0].Missing != 0 || res.Plan.Bytes() != 0 {
		t.Errorf("re-pull plans %d bytes, want none", res.Plan.Bytes())
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	// A Service with a new blob cache no longer reaches the uncompressed original, so push
	// compresses the layer anew.
	mid := emptyStore(t)
	svc = newService(t, first)
	res, err = svc.Copy(ctx, cs, mid, set(ref), opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Entries[0].Manifest == "" || res.Entries[0].Missing == 0 {
		t.Errorf("push result %+v, want a manifest digest and missing bytes", res.Entries[0])
	}
	layers := manifestLayers(t, svc, mid)
	if len(layers) != 1 || layers[0].MediaType != ocispec.MediaTypeImageLayerZstd {
		t.Errorf("stored layers %+v, want one zstd layer", layers)
	}

	second := t.TempDir()
	back := newService(t, second)
	if _, err := back.Copy(ctx, mid, cs, set(ref), opt); err != nil {
		t.Fatal(err)
	}
	if err := back.Close(); err != nil {
		t.Fatal(err)
	}
	imgs := images(t, second)
	if len(imgs) != 1 || imgs[0].ID != imageId.Encoded() {
		t.Errorf("round-tripped images %+v, want ID %s", imgs, imageId.Encoded())
	}
}

func TestCopy_dryRun(t *testing.T) {
	ctx := t.Context()
	const ref = "example.com/hello:1"
	src, _ := storeDir(t, ref)
	opt := ociimagecopy.CopyOptions{Platforms: []types.Platform{amd64}, DryRun: true}

	graph := t.TempDir()
	svc := newService(t, graph)
	res, err := svc.Copy(ctx, src, cs, set(ref), opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan.Bytes() == 0 {
		t.Error("dry-run pull planned nothing")
	}
	opt.DryRun = false
	if _, err := svc.Copy(ctx, src, cs, set(ref), opt); err != nil {
		t.Fatal(err)
	}

	dst := emptyStore(t)
	opt.DryRun = true
	res, err = svc.Copy(ctx, cs, dst, set(ref), opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan.Bytes() == 0 {
		t.Error("dry-run push planned nothing")
	}
	for img, err := range svc.List(ctx, dst) {
		t.Errorf("dry-run push stored %v (%v)", img.Ref, err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	fresh := t.TempDir()
	dry := newService(t, fresh)
	opt.DryRun = true
	if _, err := dry.Copy(ctx, src, cs, set(ref), opt); err != nil {
		t.Fatal(err)
	}
	if err := dry.Close(); err != nil {
		t.Fatal(err)
	}
	if imgs := images(t, fresh); len(imgs) != 0 {
		t.Errorf("dry-run pull imported %d images", len(imgs))
	}
}

func TestCopy_partialFailure(t *testing.T) {
	ctx := t.Context()
	const ref = "example.com/hello:1"
	src, _ := storeDir(t, ref)
	svc := newService(t, t.TempDir())
	res, err := svc.Copy(ctx, src, cs, set("example.com/absent:1", ref),
		ociimagecopy.CopyOptions{Platforms: []types.Platform{amd64}})
	if err == nil || ociimagecopy.IsUsage(err) {
		t.Fatalf("want a non-usage error, got %v", err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(res.Entries))
	}
	if res.Entries[0].Err == nil || !errors.Is(res.Entries[0].Err, fs.ErrNotExist) {
		t.Errorf("absent entry: Err = %v, want fs.ErrNotExist", res.Entries[0].Err)
	}
	if res.Entries[1].Err != nil || res.Entries[1].Manifest == "" {
		t.Errorf("present entry: %+v, want success", res.Entries[1])
	}
}

func TestCopy_usage(t *testing.T) {
	ctx := t.Context()
	svc := newService(t, t.TempDir())
	httpStore, err := ociimagecopy.ParseLocation("store:https://mirror.example/images")
	if err != nil {
		t.Fatal(err)
	}
	fileStore := emptyStore(t)
	for _, tc := range []struct {
		name     string
		src, dst ociimagecopy.Location
		set      types.ImageSetManifest
		want     error
	}{
		{"http destination", cs, httpStore, set("a:1"), ociimagecopy.ErrReadOnly},
		{"store to store", fileStore, fileStore, set("a:1"), ociimagecopy.ErrUnsupportedDirection},
		{"cs to cs", cs, cs, set("a:1"), ociimagecopy.ErrUnsupportedDirection},
		{"bad reference", cs, fileStore, set("UPPER/case:1"), ociimagecopy.ErrInvalidReference},
		{"no images", cs, fileStore, set(), ociimagecopy.ErrInvalidReference},
		{
			"hand-built location",
			ociimagecopy.Location{}, fileStore, set("a:1"), ociimagecopy.ErrInvalidLocation,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Copy(ctx, tc.src, tc.dst, tc.set, ociimagecopy.CopyOptions{})
			if !errors.Is(err, tc.want) || !ociimagecopy.IsUsage(err) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}
