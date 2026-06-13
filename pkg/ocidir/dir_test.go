package ocidir

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/go-fsys-helper/vroot/osfs"
	"github.com/opencontainers/go-digest"
)

// osfsWrap wraps *osfs.Fs to satisfy vroot.Fs[vroot.File].
type osfsWrap struct{ inner *osfs.Fs }

func newOsfsWrap(path string) (*osfsWrap, error) {
	f, err := osfs.NewFs(path)
	if err != nil {
		return nil, err
	}
	return &osfsWrap{inner: f}, nil
}

func (w *osfsWrap) Chmod(name string, mode fs.FileMode) error { return w.inner.Chmod(name, mode) }

func (w *osfsWrap) Chown(
	name string,
	uid int,
	gid int,
) error {
	return w.inner.Chown(name, uid, gid)
}
func (w *osfsWrap) Chtimes(name string, atime, mtime time.Time) error {
	return w.inner.Chtimes(name, atime, mtime)
}
func (w *osfsWrap) Close() error                           { return w.inner.Close() }
func (w *osfsWrap) Create(name string) (vroot.File, error) { return w.inner.Create(name) }

func (w *osfsWrap) Lchown(
	name string,
	uid int,
	gid int,
) error {
	return w.inner.Lchown(name, uid, gid)
}

func (w *osfsWrap) Link(
	oldname, newname string,
) error {
	return w.inner.Link(oldname, newname)
}
func (w *osfsWrap) Lstat(name string) (fs.FileInfo, error)    { return w.inner.Lstat(name) }
func (w *osfsWrap) Mkdir(name string, perm fs.FileMode) error { return w.inner.Mkdir(name, perm) }
func (w *osfsWrap) MkdirAll(name string, perm fs.FileMode) error {
	return w.inner.MkdirAll(name, perm)
}
func (w *osfsWrap) Name() string                         { return w.inner.Name() }
func (w *osfsWrap) Open(name string) (vroot.File, error) { return w.inner.Open(name) }
func (w *osfsWrap) OpenFile(name string, flag int, perm fs.FileMode) (vroot.File, error) {
	return w.inner.OpenFile(name, flag, perm)
}
func (w *osfsWrap) ReadLink(name string) (string, error) { return w.inner.ReadLink(name) }
func (w *osfsWrap) Remove(name string) error             { return w.inner.Remove(name) }
func (w *osfsWrap) RemoveAll(name string) error          { return w.inner.RemoveAll(name) }

func (w *osfsWrap) Rename(
	oldname, newname string,
) error {
	return w.inner.Rename(oldname, newname)
}
func (w *osfsWrap) Stat(name string) (fs.FileInfo, error) { return w.inner.Stat(name) }

func (w *osfsWrap) Symlink(
	oldname, newname string,
) error {
	return w.inner.Symlink(oldname, newname)
}
func (w *osfsWrap) ReadDir(name string) ([]fs.DirEntry, error) {
	f, err := w.inner.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}
func (w *osfsWrap) ReadFile(name string) ([]byte, error) { return w.inner.ReadFile(name) }

var _ vroot.Fs[vroot.File] = (*osfsWrap)(nil)

const indexJSONFixture = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.index.v1+json",
  "manifests": [
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
      "size": 4321
    }
  ]
}`

func mustFs(t *testing.T, root string) vroot.Fs[vroot.File] {
	t.Helper()
	v, err := newOsfsWrap(root)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestReadManifest_Local walks internal/testdata/ocidir/, finds every
// dump dir (any directory containing an `index.json`), and verifies it
// round-trips through [SharedFsDir] + [ReadManifest]. The `_Local`
// suffix marks it for skipping in CI; populate testdata locally with
// the recipe in the repo-root prep_testdata.go.
func TestReadManifest_Local(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "internal", "testdata", "ocidir")
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		t.Skip("no internal/testdata/ocidir/ dir")
	}

	dumps, err := findDumpDirs(root)
	if err != nil {
		t.Fatalf("findDumpDirs: %v", err)
	}
	if len(dumps) == 0 {
		t.Skip("no OCI dumps under internal/testdata/ocidir/; run `go generate .` to populate")
	}

	for _, dumpDir := range dumps {
		t.Run(dumpDir, func(t *testing.T) {
			dir := NewSharedFsDir(
				NewFsDir(mustFs(t, filepath.Join(root, filepath.FromSlash(dumpDir)))),
				mustFs(t, filepath.Join(root, "share")),
			)

			layout, err := dir.ImageLayout()
			if err != nil {
				t.Fatalf("ImageLayout: %v", err)
			}
			if layout.Version == "" {
				t.Errorf("ImageLayout.Version empty")
			}

			mDesc, man, err := ReadManifest(context.Background(), dir)
			if err != nil {
				t.Fatalf("ReadManifest: %v", err)
			}
			if mDesc.Digest == "" {
				t.Error("manifest descriptor has empty digest")
			}
			if man.Config.Digest == "" {
				t.Error("manifest config has empty digest")
			}
			if got := len(AllDescriptors(mDesc, man)); got < 2 {
				t.Errorf("AllDescriptors size = %d, want >= 2 (manifest+config)", got)
			}
		})
	}
}

// findDumpDirs walks root and returns the relative paths of every
// directory that contains an `index.json` file. The "share" subdir is
// skipped (it holds the blob pool, not dumps).
func findDumpDirs(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "share" || strings.HasPrefix(rel, "share"+string(filepath.Separator)) {
			return filepath.SkipDir
		}
		if _, err := os.Stat(filepath.Join(p, "index.json")); err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out, err
}

func TestReadManifest_MissingManifestBlob(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "index.json"),
		[]byte(indexJSONFixture),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "oci-layout"),
		[]byte(`{"imageLayoutVersion":"1.0.0"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	// (no manifest blob)
	_, _, err := ReadManifest(context.Background(), NewFsDir(mustFs(t, root)))
	if !errors.Is(err, ErrMissingManifestBlob) {
		t.Fatalf("expected ErrMissingManifestBlob, got %v", err)
	}
}

// TestReadManifest_DigestMismatch writes a valid index.json pointing at a
// manifest digest, then stores a manifest blob whose bytes have one byte
// flipped (so they no longer hash to that digest). ReadManifest must reject
// it with ErrManifestDigestMismatch rather than silently trusting the body.
func TestReadManifest_DigestMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	manifest := []byte(ociManifestFixtureForVerify)
	dg := digest.SHA256.FromBytes(manifest)

	// Corrupt one byte so the stored blob no longer matches its digest.
	corrupt := append([]byte(nil), manifest...)
	corrupt[len(corrupt)/2] ^= 0xFF

	algo, hex, err := SplitDigest(dg.String())
	if err != nil {
		t.Fatal(err)
	}
	blobDir := filepath.Join(root, "blobs", algo)
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, hex), corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	idx := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json",` +
		`"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"digest":"` + dg.String() + `","size":` +
		itoa(len(manifest)) + `}]}`
	if err := os.WriteFile(filepath.Join(root, "index.json"), []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "oci-layout"),
		[]byte(`{"imageLayoutVersion":"1.0.0"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	_, _, err = ReadManifest(context.Background(), NewFsDir(mustFs(t, root)))
	if !errors.Is(err, ErrManifestDigestMismatch) {
		t.Fatalf("expected ErrManifestDigestMismatch, got %v", err)
	}
}

// TestReadManifest_DigestMatch is the positive control for the verify path:
// an uncorrupted manifest blob round-trips without error.
func TestReadManifest_DigestMatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	manifest := []byte(ociManifestFixtureForVerify)
	dg := digest.SHA256.FromBytes(manifest)

	algo, hex, err := SplitDigest(dg.String())
	if err != nil {
		t.Fatal(err)
	}
	blobDir := filepath.Join(root, "blobs", algo)
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, hex), manifest, 0o644); err != nil {
		t.Fatal(err)
	}

	idx := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json",` +
		`"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"digest":"` + dg.String() + `","size":` +
		itoa(len(manifest)) + `}]}`
	if err := os.WriteFile(filepath.Join(root, "index.json"), []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "oci-layout"),
		[]byte(`{"imageLayoutVersion":"1.0.0"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	mDesc, _, err := ReadManifest(context.Background(), NewFsDir(mustFs(t, root)))
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if mDesc.Digest != dg {
		t.Errorf("mDesc.Digest = %q, want %q", mDesc.Digest, dg)
	}
}

func TestVerifyBlobBytes(t *testing.T) {
	t.Parallel()
	data := []byte("hello blob")
	dg := digest.SHA256.FromBytes(data)

	if err := VerifyBlobBytes(dg, data); err != nil {
		t.Errorf("VerifyBlobBytes on matching data: %v", err)
	}
	if err := VerifyBlobBytes(dg, []byte("hello blob!")); !errors.Is(err, ErrManifestDigestMismatch) {
		t.Errorf("expected ErrManifestDigestMismatch on mismatch, got %v", err)
	}
	if err := VerifyBlobBytes(digest.Digest("not-a-digest"), data); err == nil {
		t.Error("expected error for malformed digest")
	}
}

// itoa is a tiny strconv.Itoa stand-in so this test file keeps its small
// import set.
func itoa(n int) string {
	return strconv.Itoa(n)
}

// ociManifestFixtureForVerify is a small but valid OCI image manifest used by
// the digest-verification tests; its exact bytes are hashed at test time.
const ociManifestFixtureForVerify = `{"schemaVersion":2,` +
	`"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
	`"config":{"mediaType":"application/vnd.oci.image.config.v1+json",` +
	`"digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","size":2},` +
	`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip",` +
	`"digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222","size":3}]}`

func TestSplitDigest(t *testing.T) {
	t.Parallel()
	algo, hex, err := SplitDigest("sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if algo != "sha256" || hex != strings.Repeat("a", 64) {
		t.Errorf("got %q,%q", algo, hex)
	}

	if _, _, err := SplitDigest("oops"); err == nil {
		t.Error("expected error for malformed digest")
	}
	if _, _, err := SplitDigest(":x"); err == nil {
		t.Error("expected error for empty algo")
	}
}
