package ocidirtest

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ngicks/oci-image-copy/ocidir"
)

// UbuntuDir returns the absolute path of the oci-dir `go generate` dumps,
// skipping t when it has not been generated.
func UbuntuDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
	ubuntu := filepath.Join(dir, "internal", "testdata", "ocidir", "ubuntu")
	if _, err := os.Stat(filepath.Join(ubuntu, ocispec.ImageIndexFile)); err != nil {
		t.Skipf("%s is not generated; run go generate ./...", ubuntu)
	}
	return ubuntu
}

// Write stores files into fsys, creating parent directories.
func Write[F vroot.File](fsys vroot.Fs[F], files map[string][]byte) error {
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := fsys.MkdirAll(path.Dir(name), fs.ModePerm); err != nil {
			return err
		}
		f, err := fsys.Create(name)
		if err != nil {
			return err
		}
		_, err = f.Write(files[name])
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// TestOciDir checks that dir serves exactly the files of f.
func TestOciDir(t *testing.T, dir ocidir.OciDir, f Fixture) {
	t.Helper()
	ctx := t.Context()

	layoutBin, err := dir.OciLayoutBin(ctx)
	if err != nil || !bytes.Equal(layoutBin, f.Files[ocispec.ImageLayoutFile]) {
		t.Errorf("OciLayoutBin = %q, %v", layoutBin, err)
	}
	layout, err := dir.OciLayout(ctx)
	if err != nil || layout.Version != ocispec.ImageLayoutVersion {
		t.Errorf("OciLayout = %#v, %v", layout, err)
	}
	indexBin, err := dir.IndexBin(ctx)
	if err != nil || !bytes.Equal(indexBin, f.Files[ocispec.ImageIndexFile]) {
		t.Errorf("IndexBin = %q, %v", indexBin, err)
	}
	idx, err := dir.Index(ctx)
	if err != nil || len(idx.Manifests) != 5 {
		t.Errorf("Index = %#v, %v", idx, err)
	}

	for name, want := range f.Files {
		dgst, ok := BlobDigest(name)
		if !ok {
			continue
		}
		r, err := dir.Blobs(ctx, dgst)
		if err != nil {
			t.Errorf("Blobs(%q): %v", dgst, err)
			continue
		}
		if r.Size() != int64(len(want)) {
			t.Errorf("Blobs(%q).Size() = %d, want %d", dgst, r.Size(), len(want))
		}
		off := int64(len(want) / 2)
		buf := make([]byte, len(want)-int(off))
		n, err := r.ReadAt(buf, off)
		if (err != nil && !errors.Is(err, io.EOF)) || !bytes.Equal(buf[:n], want[off:]) {
			t.Errorf(
				"Blobs(%q).ReadAt(off=%d) = %q, %v; want %q",
				dgst,
				off,
				buf[:n],
				err,
				want[off:],
			)
		}
		if err := r.Close(); err != nil {
			t.Errorf("Blobs(%q).Close(): %v", dgst, err)
		}
	}

	missing := digest.FromString("missing")
	if r, err := dir.Blobs(ctx, missing); !errors.Is(err, fs.ErrNotExist) {
		if r != nil {
			_ = r.Close()
		}
		t.Errorf("Blobs(missing): want fs.ErrNotExist, got %v", err)
	}
	if _, err := dir.Blobs(ctx, "sha256:../../index.json"); err == nil {
		t.Errorf("Blobs(invalid digest): want error")
	}
}

// TestSameAs checks that got serves the same images and blobs as want.
func TestSameAs(t *testing.T, want, got ocidir.OciDir) {
	t.Helper()
	ctx := t.Context()

	type image struct {
		Name  string
		Image ocispec.Image
	}
	images := func(dir ocidir.OciDir) []image {
		seq, err := ocidir.AllImages(ctx, dir)
		if err != nil {
			t.Fatalf("AllImages: %v", err)
		}
		var out []image
		for name, img := range seq {
			out = append(out, image{name, img})
		}
		return out
	}
	wantImages, gotImages := images(want), images(got)
	if len(wantImages) == 0 {
		t.Fatal("no images in want")
	}
	if !reflect.DeepEqual(gotImages, wantImages) {
		t.Errorf("AllImages mismatch:\ngot  = %#v\nwant = %#v", gotImages, wantImages)
	}

	blobs := func(dir ocidir.OciDir) map[digest.Digest][]byte {
		seq, err := ocidir.AllBlobsFromNames(ctx, dir)
		if err != nil {
			t.Fatalf("AllBlobsFromNames: %v", err)
		}
		out := map[digest.Digest][]byte{}
		for blob, err := range seq {
			if err != nil {
				t.Fatalf("blob %q: %v", blob.Name, err)
			}
			b, err := io.ReadAll(io.NewSectionReader(blob.Reader, 0, blob.Reader.Size()))
			_ = blob.Reader.Close()
			if err != nil {
				t.Fatalf("blob %q: %v", blob.Name, err)
			}
			if digest.FromBytes(b) != blob.Name {
				t.Errorf("blob %q: content digest mismatch", blob.Name)
			}
			out[blob.Name] = b
		}
		return out
	}
	wantBlobs, gotBlobs := blobs(want), blobs(got)
	if !maps.EqualFunc(gotBlobs, wantBlobs, bytes.Equal) {
		t.Errorf("blobs mismatch: got %d blobs, want %d", len(gotBlobs), len(wantBlobs))
	}
}

// BlobDigest parses an oci-dir relative path "blobs/<alg>/<hex>" into its digest.
func BlobDigest(name string) (digest.Digest, bool) {
	dir, enc := path.Split(name)
	alg := path.Base(path.Clean(dir))
	if path.Dir(path.Clean(dir)) != ocispec.ImageBlobsDir {
		return "", false
	}
	dgst := digest.NewDigestFromEncoded(digest.Algorithm(alg), enc)
	return dgst, dgst.Validate() == nil
}
