package fsdir_test

import (
	"path"
	"testing"

	"github.com/ngicks/go-fsys-helper/vroot/memfs"
	"github.com/ngicks/go-fsys-helper/vroot/osfs"

	"github.com/ngicks/oci-image-copy/internal/ocidirtest"
	"github.com/ngicks/oci-image-copy/ocidir"
	"github.com/ngicks/oci-image-copy/ocidir/fsdir"
)

func TestDir(t *testing.T) {
	f := ocidirtest.New()
	fsys := memfs.New("fixture")
	if err := ocidirtest.Write(fsys, f.Files); err != nil {
		t.Fatal(err)
	}
	ocidirtest.TestOciDir(t, fsdir.New(fsys, nil), f)
}

func TestDir_sharedBlob(t *testing.T) {
	f := ocidirtest.New()
	layout, shared := map[string][]byte{}, map[string][]byte{}
	for name, b := range f.Files {
		if dgst, ok := ocidirtest.BlobDigest(name); ok {
			shared[path.Join(dgst.Algorithm().String(), dgst.Encoded())] = b
		} else {
			layout[name] = b
		}
	}
	fsys, sharedFsys := memfs.New("layout"), memfs.New("shared")
	if err := ocidirtest.Write(fsys, layout); err != nil {
		t.Fatal(err)
	}
	if err := ocidirtest.Write(sharedFsys, shared); err != nil {
		t.Fatal(err)
	}
	ocidirtest.TestOciDir(t, fsdir.New(fsys, sharedFsys), f)
}

func TestUbuntu_Local(t *testing.T) {
	root, err := osfs.NewRoot(ocidirtest.UbuntuDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	dir := fsdir.New(root, nil)

	seq, err := ocidir.AllImages(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for name, img := range seq {
		names = append(names, name)
		if img.OS != "linux" || len(img.RootFS.DiffIDs) == 0 {
			t.Errorf("image %q: unexpected config %#v", name, img)
		}
	}
	if len(names) == 0 {
		t.Fatal("no images")
	}
	t.Logf("images: %v", names)

	ocidirtest.TestSameAs(t, dir, dir)
}
