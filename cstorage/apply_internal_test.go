package cstorage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage"

	"github.com/ngicks/oci-image-copy/blobcache"
)

// TestApplyLayer_duplicate stands in for a concurrent pull that created the layer
// between Pull's existence check and its PutLayer.
func TestApplyLayer_duplicate(t *testing.T) {
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	body := []byte("x\n")
	if err := tw.WriteHeader(
		&tar.Header{Name: "x", Mode: 0o644, Size: int64(len(body))},
	); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(body)
	_ = tw.Close()
	diffID := digest.FromBytes(tb.Bytes())
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(tb.Bytes())
	_ = zw.Close()

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
	cache, err := blobcache.New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	desc, err := cache.Put(t.Context(), ocispec.MediaTypeImageLayerGzip, &gz)
	if err != nil {
		t.Fatal(err)
	}

	id := diffID.Encoded()
	if _, _, err := st.PutLayer(
		id,
		"",
		nil,
		"",
		false,
		nil,
		bytes.NewReader(tb.Bytes()),
	); err != nil {
		t.Fatal(err)
	}
	s := New(st, cache, nil)
	if err := s.applyLayer(id, "", desc, diffID); err != nil {
		t.Errorf("existing layer with the same diffID: %v", err)
	}
	if err := s.applyLayer(id, "", desc, digest.FromString("other")); err == nil {
		t.Error("existing layer with another diffID: want error")
	}
}
