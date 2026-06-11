package imagecopy

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/ocidir"
	"github.com/opencontainers/go-digest"
)

// TestFsOciDirs_RoundTrip_Local walks every image in
// internal/testdata/ocidir/, builds an [FsOciDirs] over it, and
// exercises the full read surface: [OciDirs.ListImages],
// [OciDirs.Image] → [ocidir.ReadManifest], and [OciDirs.Blob] for
// every digest in the closure. Each blob's bytes are re-hashed and
// compared against the digest the manifest claims.
//
// Skipped in CI (testdata is generated locally by
// `go run ./internal/cmd/dumpimages`).
func TestFsOciDirs_RoundTrip_Local(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "internal", "testdata", "ocidir")
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		t.Skip("no internal/testdata/ocidir/ dir")
	}

	fsys, err := NewOsFs(root)
	if err != nil {
		t.Fatal(err)
	}
	dirs := NewFsOciDirs(fsys, 1)
	ctx := context.Background()

	var images int
	for ref, err := range listImagesFromFs(ctx, fsys) {
		if err != nil {
			t.Fatalf("listImagesFromFs: %v", err)
		}
		images++
		t.Run(ref.String(), func(t *testing.T) {
			img := dirs.Image(ref)
			mDesc, man, err := ocidir.ReadManifest(ctx, img)
			if err != nil {
				t.Fatalf("ReadManifest %s: %v", ref.String(), err)
			}
			if mDesc.Digest == "" {
				t.Fatal("manifest descriptor has empty digest")
			}
			if man.Config.Digest == "" {
				t.Fatal("manifest config has empty digest")
			}

			descs := ocidir.AllDescriptors(mDesc, man)
			for _, d := range descs {
				if d.Digest == "" {
					continue
				}
				rc, size, err := dirs.Blob(ctx, d.Digest, 0)
				if err != nil {
					t.Errorf("Blob %s: %v", d.Digest, err)
					continue
				}
				verifier := d.Digest.Verifier()
				n, err := io.Copy(verifier, rc)
				rc.Close()
				if err != nil {
					t.Errorf("read blob %s: %v", d.Digest, err)
					continue
				}
				if d.Size > 0 && size != d.Size {
					t.Errorf(
						"blob %s size mismatch: blob.Stat=%d, descriptor=%d",
						d.Digest,
						size,
						d.Size,
					)
				}
				if d.Size > 0 && n != d.Size {
					t.Errorf("blob %s read %d bytes, descriptor says %d", d.Digest, n, d.Size)
				}
				if !verifier.Verified() {
					t.Errorf("blob %s digest verification failed", d.Digest)
				}
			}
		})
	}
	if images == 0 {
		t.Skip("no images under internal/testdata/ocidir/; run `go run ./internal/cmd/dumpimages`")
	}
}

// TestFsOciDirs_BlobOffset_Local reads a real layer blob at various
// offsets and asserts the suffix matches a from-zero read.
func TestFsOciDirs_BlobOffset_Local(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "internal", "testdata", "ocidir")
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		t.Skip("no internal/testdata/ocidir/ dir")
	}
	fsys, err := NewOsFs(root)
	if err != nil {
		t.Fatal(err)
	}
	dirs := NewFsOciDirs(fsys, 1)
	ctx := context.Background()

	// Pick the first digest we find via ListBlobs.
	var pick digest.Digest
	for d, err := range listBlobsFromFs(ctx, fsys) {
		if err != nil {
			t.Fatal(err)
		}
		pick = d
		break
	}
	if pick == "" {
		t.Skip("no blobs in testdata")
	}

	rc, size, err := dirs.Blob(ctx, pick, 0)
	if err != nil {
		t.Fatal(err)
	}
	full, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(full)) != size {
		t.Fatalf("got %d bytes, size=%d", len(full), size)
	}

	for _, off := range []int64{0, 1, size / 2, size - 1, size} {
		if off < 0 || off > size {
			continue
		}
		rc, _, err := dirs.Blob(ctx, pick, off)
		if err != nil {
			t.Errorf("Blob at offset %d: %v", off, err)
			continue
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Errorf("read at offset %d: %v", off, err)
			continue
		}
		want := full[off:]
		if string(got) != string(want) {
			t.Errorf("offset %d: got %d bytes, want %d", off, len(got), len(want))
		}
	}
}
