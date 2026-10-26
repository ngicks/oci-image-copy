package fsdir_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ngicks/go-fsys-helper/vroot/memfs"
	"github.com/ngicks/go-fsys-helper/vroot/osfs"

	"github.com/ngicks/oci-image-copy/internal/ocidirtest"
	"github.com/ngicks/oci-image-copy/internal/storetest"
	"github.com/ngicks/oci-image-copy/ocidir/fsdir"
	"github.com/ngicks/oci-image-copy/store"
	"github.com/ngicks/oci-image-copy/types"
)

func TestStore(t *testing.T) {
	storetest.TestBackend(t, func(t *testing.T) *store.Store {
		return store.New(fsdir.NewStore(memfs.New("store")))
	}, true)
}

// TestStore_skopeo_Local checks that skopeo reads a leaf with --src-shared-blob-dir.
func TestStore_skopeo_Local(t *testing.T) {
	if _, err := exec.LookPath("skopeo"); err != nil {
		t.Skip("skopeo is not installed")
	}
	dir := t.TempDir()
	root, err := osfs.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	s := store.New(fsdir.NewStore(root))
	f := ocidirtest.New()
	storetest.PutImage(t, s, f, f.Single)
	ref, _ := types.ParseReference(ocidirtest.SingleName)
	p := types.Platform{OS: "linux", Architecture: "amd64"}
	if err := s.Commit(t.Context(), ref, p, f.Single.Manifest); err != nil {
		t.Fatal(err)
	}

	leaf := filepath.Join(dir, filepath.FromSlash(store.LeafPath(ref, p)))
	if info, err := os.Stat(filepath.Join(leaf, "blobs")); err != nil || !info.IsDir() {
		t.Errorf("leaf has no empty blobs/: %v", err)
	}
	out, err := exec.Command(
		"skopeo", "inspect", "--raw",
		"--shared-blob-dir", filepath.Join(dir, "shared", "blobs"),
		"oci:"+leaf+":"+ref.Tag,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("skopeo inspect: %v\n%s", err, out)
	}
	if string(out) != string(f.Files[ocidirtest.BlobPath(f.Single.Manifest.Digest)]) {
		t.Errorf("skopeo read a different manifest:\n%s", out)
	}
}
