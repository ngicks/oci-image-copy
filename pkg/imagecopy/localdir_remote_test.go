package imagecopy

// localdir_remote_test.go exercises the localDirRemote implementation:
//   - NewLocalDirRemote construction
//   - ListBlobs / ListImages — via shared fs-walk helpers
//   - InspectImage — raw manifest bytes from mirror
//   - LoadImage / DumpImage — no-op assertions
//   - ReadOnly false
//   - Close no-op

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/imageref"
)

// buildLocalDirRemote returns a [Remote] backed by a freshly initialised
// store at baseDir. The directory must already exist.
func buildLocalDirRemote(t *testing.T, baseDir string) Remote {
	t.Helper()
	r, err := NewLocalDirRemote(baseDir)
	if err != nil {
		t.Fatalf("NewLocalDirRemote: %v", err)
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

// TestLocalDirRemote_ListBlobs_Empty verifies ListBlobs on an empty store
// yields nothing (no error, no items).
func TestLocalDirRemote_ListBlobs_Empty(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	r := buildLocalDirRemote(t, base)
	count := 0
	for _, err := range r.ListBlobs(context.Background()) {
		if err != nil {
			t.Fatalf("ListBlobs: unexpected error: %v", err)
		}
		count++
	}
	if count != 0 {
		t.Errorf("ListBlobs on empty store: got %d blobs, want 0", count)
	}
}

// TestLocalDirRemote_ListBlobs_Seeded verifies ListBlobs returns the blobs
// placed into the share/ dir.
func TestLocalDirRemote_ListBlobs_Seeded(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	tagDir := filepath.Join(base, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(base, "share")
	seedDump(t, tagDir, shareDir)

	r := buildLocalDirRemote(t, base)

	var digests []string
	for d, err := range r.ListBlobs(context.Background()) {
		if err != nil {
			t.Fatalf("ListBlobs: %v", err)
		}
		digests = append(digests, string(d))
	}
	if len(digests) == 0 {
		t.Fatal("ListBlobs: expected some blobs, got 0")
	}
	// All returned digests must start with "sha256:"
	for _, d := range digests {
		if !strings.HasPrefix(d, "sha256:") {
			t.Errorf("digest %q does not start with sha256:", d)
		}
	}
}

// TestLocalDirRemote_ListImages_Seeded verifies ListImages returns the image
// refs placed via seedDump.
func TestLocalDirRemote_ListImages_Seeded(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	tagDir := filepath.Join(base, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(base, "share")
	seedDump(t, tagDir, shareDir)

	r := buildLocalDirRemote(t, base)

	var images []imageref.ImageRef
	for img, err := range r.ListImages(context.Background()) {
		if err != nil {
			t.Fatalf("ListImages: %v", err)
		}
		images = append(images, img)
	}
	if len(images) == 0 {
		t.Fatal("ListImages: expected at least 1 image, got 0")
	}
	found := false
	for _, img := range images {
		if img.Host == "ghcr.io" && img.Tag == "v1" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected ghcr.io/a/b:v1 in ListImages, got %v", images)
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

// TestLocalDirRemote_Dir_PutTagFile verifies the OciDirs surface writes
// tag files correctly (PutTagFile creates the dump dir and writes the file).
func TestLocalDirRemote_Dir_PutTagFile(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	r := buildLocalDirRemote(t, base)

	ref, _ := imageref.Parse("example.com/test:latest")
	data := []byte(`{"imageLayoutVersion":"1.0.0"}`)
	if err := r.Dir().PutTagFile(context.Background(), ref, "oci-layout", data); err != nil {
		t.Fatalf("PutTagFile: %v", err)
	}
	// Verify the file exists and has the right content.
	got, err := os.ReadFile(
		filepath.Join(base, "example.com", "test", "_tags", "latest", "oci-layout"),
	)
	if err != nil {
		t.Fatalf("ReadFile after PutTagFile: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("PutTagFile content = %q, want %q", got, data)
	}
}
