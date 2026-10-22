package httpdir_test

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/ngicks/go-fsys-helper/vroot/osfs"

	"github.com/ngicks/oci-image-copy/internal/ocidirtest"
	"github.com/ngicks/oci-image-copy/ocidir/fsdir"
	"github.com/ngicks/oci-image-copy/ocidir/httpdir"
)

func serve(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.FileServerFS(os.DirFS(dir)))
	t.Cleanup(srv.Close)
	return srv
}

func TestDir(t *testing.T) {
	f := ocidirtest.New()
	tmp := t.TempDir()
	root, err := osfs.NewRoot(tmp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if err := ocidirtest.Write(root, f.Files); err != nil {
		t.Fatal(err)
	}
	srv := serve(t, tmp)

	dir := httpdir.New(httpdir.BaseURL(srv.URL+"/"), nil)
	ocidirtest.TestOciDir(t, dir, f)
	ocidirtest.TestSameAs(t, fsdir.New(root), dir)

	missing := httpdir.New(httpdir.BaseURL(srv.URL+"/nowhere"), nil)
	if _, err := missing.IndexBin(t.Context()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("IndexBin under a missing base: want fs.ErrNotExist, got %v", err)
	}
}

func TestUbuntu_Local(t *testing.T) {
	ubuntu := ocidirtest.UbuntuDir(t)
	root, err := osfs.NewRoot(ubuntu)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	srv := serve(t, ubuntu)

	ocidirtest.TestSameAs(t, fsdir.New(root), httpdir.New(httpdir.BaseURL(srv.URL), nil))
}
