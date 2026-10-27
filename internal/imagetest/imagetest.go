// Package imagetest writes small, real images into stores for tests that import them into
// containers-storage.
package imagetest

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/ngicks/go-fsys-helper/vroot/osfs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ngicks/oci-image-copy/ocidir/fsdir"
	"github.com/ngicks/oci-image-copy/store"
	"github.com/ngicks/oci-image-copy/types"
)

var Amd64 = types.Platform{OS: "linux", Architecture: "amd64"}

// TarLayer returns an uncompressed tar holding one file; equal input gives equal bytes.
func TarLayer(t *testing.T, name, body string) []byte {
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

// WriteStore commits ref for linux/amd64 into the filesystem store at dir, with one uncompressed
// layer whose content depends on ref only. It returns the config digest, which containers-storage
// uses as the image ID.
func WriteStore(t *testing.T, dir, ref string) digest.Digest {
	t.Helper()
	ctx := t.Context()
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
	ld := put(ocispec.MediaTypeImageLayer, TarLayer(t, "etc/hello", ref+"\n"))
	cfg, _ := json.Marshal(ocispec.Image{
		OS:           Amd64.OS,
		Architecture: Amd64.Architecture,
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
	if err := st.Commit(ctx, r, Amd64, md); err != nil {
		t.Fatal(err)
	}
	return cd.Digest
}
