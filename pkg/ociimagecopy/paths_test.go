package ociimagecopy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/imageref"
)

func TestStorePaths(t *testing.T) {
	t.Parallel()
	base := "/tmp/x"
	st := NewStore(base)

	tagged, err := imageref.Parse("ghcr.io/a/b/c:d")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := st.TagDir(
		tagged,
	), filepath.Join(
		base,
		"ghcr.io",
		"a",
		"b",
		"c",
		"_tags",
		"d",
	); got != want {
		t.Errorf("TagDir: got %q, want %q", got, want)
	}
	if got := st.DigestDir(tagged); got != "" {
		t.Errorf("DigestDir on tagged ref: got %q, want empty", got)
	}

	digested, err := imageref.Parse("ghcr.io/x/y@sha256:" + strings.Repeat("0", 64))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "ghcr.io", "x", "y", "_digests", strings.Repeat("0", 64))
	if got := st.DigestDir(digested); got != want {
		t.Errorf("DigestDir: got %q, want %q", got, want)
	}
	if got := st.TagDir(digested); got != "" {
		t.Errorf("TagDir on digested ref: got %q, want empty", got)
	}

	if got, want := st.ShareDir(), filepath.Join(base, "share"); got != want {
		t.Errorf("ShareDir: got %q, want %q", got, want)
	}
}

func TestStoreDumpDir(t *testing.T) {
	t.Parallel()
	st := NewStore("/b")

	tagged, _ := imageref.Parse("nginx:latest")
	if d, err := st.DumpDir(tagged); err != nil || !strings.Contains(d, "_tags") {
		t.Errorf("tagged DumpDir = %q, %v", d, err)
	}

	digested, _ := imageref.Parse("nginx@sha256:" + strings.Repeat("a", 64))
	if d, err := st.DumpDir(digested); err != nil || !strings.Contains(d, "_digests") {
		t.Errorf("digested DumpDir = %q, %v", d, err)
	}
}

func TestStoreEnsureLayoutIdempotent(t *testing.T) {
	t.Parallel()
	st := NewStore(t.TempDir())
	ctx := context.Background()

	for i := range 2 {
		if err := st.EnsureLayout(ctx); err != nil {
			t.Fatalf("EnsureLayout pass %d: %v", i, err)
		}
	}
	if fi, err := os.Stat(st.ShareDir()); err != nil || !fi.IsDir() {
		t.Errorf("dir %q: stat=%v, isDir=%v", st.ShareDir(), err, fi != nil && fi.IsDir())
	}
}

func TestDefaultBaseDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/custom/xdg")
	got, err := DefaultBaseDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/custom/xdg", AppDirName); got != want {
		t.Errorf("DefaultBaseDir w/ XDG: got %q, want %q", got, want)
	}

	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/h")
	got, err = DefaultBaseDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/h", ".local", "share", AppDirName); got != want {
		t.Errorf("DefaultBaseDir w/o XDG: got %q, want %q", got, want)
	}
}
