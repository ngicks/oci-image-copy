package remote

// localdir_test.go exercises the localDirRemote implementation:
//   - NewLocalDir construction
//   - ListBlobsByImage — per-image blob closure (replaces global ListBlobs, which
//     was removed per D4/D5).
//   - ListImagesFromFs — via ociimagecopy.ListImagesFromFs (replaces
//     Remote.ListImages, which was removed per D4).
//   - InspectImage — raw manifest bytes from mirror
//   - LoadImage / DumpImage — no-op assertions
//   - Tags().PutOciLayout — tag-file write (replaces r.Dir().PutTagFile)
//   - ReadOnly false
//   - Close no-op
//
// DELETED (with reasons):
//   - TestLocalDirRemote_ListBlobs_Empty, TestLocalDirRemote_ListBlobs_Seeded:
//     global Remote.ListBlobs removed (D4). Intent re-expressed as
//     TestLocalDirRemote_ListBlobsByImage_Empty and
//     TestLocalDirRemote_ListBlobsByImage_Seeded below.
//   - TestLocalDirRemote_ListImages_Seeded: Remote.ListImages removed (D4).
//     Intent re-expressed as TestLocalDirRemote_ListImagesFromFs_Seeded using
//     ociimagecopy.ListImagesFromFs (the helper is kept and exported, per PLAN).
//   - TestLocalDirRemote_Dir_PutTagFile: Dir() removed (D12). Intent
//     re-expressed as TestLocalDirRemote_Tags_PutOciLayout using
//     r.Tags().PutOciLayout, which writes via FsOciDirs.PutOciLayout → the
//     same dump dir path the old Dir().PutTagFile used.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy"
)

// buildLocalDirRemote returns a [ociimagecopy.Remote] backed by a freshly
// initialised store at baseDir. The directory must already exist.
func buildLocalDirRemote(t *testing.T, baseDir string) ociimagecopy.Remote {
	t.Helper()
	r, err := NewLocalDir(baseDir)
	if err != nil {
		t.Fatalf("NewLocalDir: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// TestLocalDirRemote_ReadOnly verifies ReadOnly is always false.
func TestLocalDirRemote_ReadOnly(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	r := buildLocalDirRemote(t, base)
	if r.ReadOnly() {
		t.Error("ReadOnly() = true, want false")
	}
}

// TestLocalDirRemote_Close_NoOp verifies Close does not return an error.
func TestLocalDirRemote_Close_NoOp(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	r := buildLocalDirRemote(t, base)
	if err := r.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
	// Calling Close twice must also not error.
	if err := r.Close(); err != nil {
		t.Errorf("second Close() error = %v, want nil", err)
	}
}

// TestLocalDirRemote_LoadImage_NoOp verifies LoadImage always returns nil.
func TestLocalDirRemote_LoadImage_NoOp(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	r := buildLocalDirRemote(t, base)
	ref, _ := imageref.Parse("example.com/repo:v1")
	if err := r.LoadImage(context.Background(), ref); err != nil {
		t.Errorf("LoadImage() error = %v, want nil", err)
	}
}

// TestLocalDirRemote_DumpImage_NoOp verifies DumpImage always returns nil.
func TestLocalDirRemote_DumpImage_NoOp(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	r := buildLocalDirRemote(t, base)
	ref, _ := imageref.Parse("example.com/repo:v1")
	if err := r.DumpImage(context.Background(), ref); err != nil {
		t.Errorf("DumpImage() error = %v, want nil", err)
	}
}

// TestLocalDirRemote_ListBlobsByImage_Empty verifies ListBlobsByImage on an
// empty store (no image seeded) yields nothing and no error.
// (Replaces the deleted TestLocalDirRemote_ListBlobs_Empty, per D4/D5.)
func TestLocalDirRemote_ListBlobsByImage_Empty(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	r := buildLocalDirRemote(t, base)
	ref, _ := imageref.Parse("example.com/absent:v1")
	count := 0
	for _, err := range r.ListBlobsByImage(context.Background(), ref) {
		if err != nil {
			t.Fatalf("ListBlobsByImage: unexpected error: %v", err)
		}
		count++
	}
	if count != 0 {
		t.Errorf("ListBlobsByImage on absent image: got %d blobs, want 0", count)
	}
}

// TestLocalDirRemote_ListBlobsByImage_Seeded verifies ListBlobsByImage returns
// the blobs in the seeded image's manifest closure. (Replaces the deleted
// TestLocalDirRemote_ListBlobs_Seeded, per D4/D5.)
func TestLocalDirRemote_ListBlobsByImage_Seeded(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	tagDir := filepath.Join(base, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(base, "share")
	seedDump(t, tagDir, shareDir)

	r := buildLocalDirRemote(t, base)
	ref, _ := imageref.Parse("ghcr.io/a/b:v1")

	var digests []string
	for d, err := range r.ListBlobsByImage(context.Background(), ref) {
		if err != nil {
			t.Fatalf("ListBlobsByImage: %v", err)
		}
		digests = append(digests, string(d))
	}
	if len(digests) == 0 {
		t.Fatal("ListBlobsByImage: expected some blobs, got 0")
	}
	// All returned digests must start with "sha256:"
	for _, d := range digests {
		if !strings.HasPrefix(d, "sha256:") {
			t.Errorf("digest %q does not start with sha256:", d)
		}
	}
}

// TestLocalDirRemote_ListImagesFromFs_Seeded verifies that
// ociimagecopy.ListImagesFromFs finds the image refs placed via seedDump.
// (Replaces the deleted TestLocalDirRemote_ListImages_Seeded — Remote.ListImages
// was removed in D4, but ListImagesFromFs is kept as a package-level helper.)
func TestLocalDirRemote_ListImagesFromFs_Seeded(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	tagDir := filepath.Join(base, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(base, "share")
	seedDump(t, tagDir, shareDir)

	fsys, err := ociimagecopy.NewOsFs(base)
	if err != nil {
		t.Fatalf("NewOsFs: %v", err)
	}

	var images []imageref.ImageRef
	for img, err := range ociimagecopy.ListImagesFromFs(context.Background(), fsys) {
		if err != nil {
			t.Fatalf("ListImagesFromFs: %v", err)
		}
		images = append(images, img)
	}
	if len(images) == 0 {
		t.Fatal("ListImagesFromFs: expected at least 1 image, got 0")
	}
	found := false
	for _, img := range images {
		if img.Host == "ghcr.io" && img.Tag == "v1" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected ghcr.io/a/b:v1 in ListImagesFromFs, got %v", images)
	}
}

// TestLocalDirRemote_InspectImage verifies that InspectImage reads the exact
// raw manifest bytes from the local mirror (so sha256 is preserved).
func TestLocalDirRemote_InspectImage(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	tagDir := filepath.Join(base, "ghcr.io", "org", "repo", "_tags", "v1")
	shareDir := filepath.Join(base, "share")
	seedDump(t, tagDir, shareDir)

	r := buildLocalDirRemote(t, base)
	ref, err := imageref.Parse("ghcr.io/org/repo:v1")
	if err != nil {
		t.Fatal(err)
	}

	got, err := r.InspectImage(context.Background(), ref)
	if err != nil {
		t.Fatalf("InspectImage: %v", err)
	}
	if string(got) != realManifestContent {
		t.Errorf("InspectImage returned wrong bytes:\ngot:  %q\nwant: %q",
			got, realManifestContent)
	}
}

// TestLocalDirRemote_InspectImage_Missing verifies InspectImage errors when
// the image is not present.
func TestLocalDirRemote_InspectImage_Missing(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	r := buildLocalDirRemote(t, base)
	ref, _ := imageref.Parse("missing.example.com/repo:v1")
	_, err := r.InspectImage(context.Background(), ref)
	if err == nil {
		t.Error("InspectImage on missing image: expected error, got nil")
	}
}

// TestLocalDirRemote_Tags_PutOciLayout verifies that Tags().PutOciLayout writes
// tag files correctly (creates the dump dir and writes the oci-layout file).
// (Replaces the deleted TestLocalDirRemote_Dir_PutTagFile — Dir() was removed
// in D12; Tags() returns the FsOciDirs which implements PutOciLayout.)
func TestLocalDirRemote_Tags_PutOciLayout(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	r := buildLocalDirRemote(t, base)

	ref, _ := imageref.Parse("example.com/test:latest")
	data := []byte(`{"imageLayoutVersion":"1.0.0"}`)
	if err := r.Tags().PutOciLayout(context.Background(), ref, data); err != nil {
		t.Fatalf("Tags().PutOciLayout: %v", err)
	}
	// Verify the file exists and has the right content.
	got, err := os.ReadFile(
		filepath.Join(base, "example.com", "test", "_tags", "latest", "oci-layout"),
	)
	if err != nil {
		t.Fatalf("ReadFile after Tags().PutOciLayout: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("Tags().PutOciLayout content = %q, want %q", got, data)
	}
}
