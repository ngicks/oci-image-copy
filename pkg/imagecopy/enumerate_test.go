package imagecopy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
)

// fakeSkopeoInspector returns canned manifest bytes per (transport, ref).
type fakeSkopeoInspector struct {
	byRef map[string][]byte
	err   error
}

func (f *fakeSkopeoInspector) Inspect(
	ctx context.Context,
	src skopeo.TransportRef,
	raw bool,
	sharedBlobDir string,
	extraArgs ...string,
) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if data, ok := f.byRef[string(src.Transport)+":"+src.Arg1]; ok {
		return data, nil
	}
	return nil, errors.New("no fixture for ref")
}

type fakeLister struct{ refs []string }

func (f *fakeLister) ImageLs(ctx context.Context) ([]string, error) {
	return f.refs, nil
}

func TestEnumerate_ContainersStorage(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	shareBlob := strings.Repeat("9", 64)
	must(t, os.MkdirAll(filepath.Join(tmp, "share", "sha256"), 0o755))
	must(t, os.WriteFile(filepath.Join(tmp, "share", "sha256", shareBlob), []byte("loose"), 0o644))

	fs, err := NewOsFs(tmp)
	if err != nil {
		t.Fatal(err)
	}

	cfg := EnumerateConfig{
		Transport: skopeo.TransportContainersStorage,
		Skopeo: &fakeSkopeoInspector{
			byRef: map[string][]byte{
				"containers-storage:foo/bar:1": []byte(ociManifestFixture),
			},
		},
		Podman:  &fakeLister{refs: []string{"foo/bar:1"}},
		Fs:      fs,
		BaseDir: tmp,
	}
	got, err := cfg.Enumerate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	manDigest := ocidir.DigestBytes([]byte(ociManifestFixture))
	wants := []string{
		manDigest,
		"sha256:" + strings.Repeat("1", 64),
		"sha256:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("b", 64),
		"sha256:" + shareBlob,
	}
	for _, w := range wants {
		if _, ok := got[w]; !ok {
			t.Errorf("missing %s in result %v", w, sortedKeys(got))
		}
	}
}

func TestEnumerate_DockerDaemon_SkipsBadInspect(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	fs, err := NewOsFs(tmp)
	if err != nil {
		t.Fatal(err)
	}
	cfg := EnumerateConfig{
		Transport: skopeo.TransportDockerDaemon,
		Docker:    &fakeLister{refs: []string{"only:1"}},
		Skopeo:    &fakeSkopeoInspector{},
		Fs:        fs,
		BaseDir:   tmp,
	}
	got, err := cfg.Enumerate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty (all inspects failed), got %v", sortedKeys(got))
	}
}

func TestEnumerate_OCI_FilesystemWalk(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	// The manifest blob must live under the path named by its real digest:
	// ReadManifest now verifies the blob bytes against that digest, so the
	// fixture has to be internally consistent (as any real OCI dir is).
	manDigest := ocidir.DigestBytes([]byte(ociManifestFixture))
	manifestHex := strings.TrimPrefix(manDigest, "sha256:")
	looseHex := strings.Repeat("9", 64)

	// index.json points its single manifest at the real manifest digest.
	indexJSON := `{"schemaVersion":2,` +
		`"mediaType":"application/vnd.oci.image.index.v1+json",` +
		`"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"digest":"` + manDigest + `","size":` +
		strconv.Itoa(len(ociManifestFixture)) + `}]}`

	tagsDump := filepath.Join(tmp, "ghcr.io", "a", "b", "_tags", "v1")
	must(t, os.MkdirAll(tagsDump, 0o755))
	must(t, os.WriteFile(filepath.Join(tagsDump, "index.json"), []byte(indexJSON), 0o644))
	must(t, os.WriteFile(filepath.Join(tagsDump, "oci-layout"), []byte("{}"), 0o644))

	digestDump := filepath.Join(tmp, "ghcr.io", "x", "_digests", manifestHex)
	must(t, os.MkdirAll(digestDump, 0o755))
	must(t, os.WriteFile(filepath.Join(digestDump, "index.json"), []byte(indexJSON), 0o644))
	must(t, os.WriteFile(filepath.Join(digestDump, "oci-layout"), []byte("{}"), 0o644))

	shareSha := filepath.Join(tmp, "share", "sha256")
	must(t, os.MkdirAll(shareSha, 0o755))
	must(t, os.WriteFile(filepath.Join(shareSha, manifestHex), []byte(ociManifestFixture), 0o644))
	must(t, os.WriteFile(filepath.Join(shareSha, looseHex), []byte("loose"), 0o644))

	fs, err := NewOsFs(tmp)
	if err != nil {
		t.Fatal(err)
	}
	cfg := EnumerateConfig{
		Transport: skopeo.TransportOci,
		Fs:        fs,
		BaseDir:   tmp,
	}
	got, err := cfg.Enumerate(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	wants := []string{
		manDigest,
		"sha256:" + strings.Repeat("1", 64),
		"sha256:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("b", 64),
		"sha256:" + looseHex,
	}
	for _, w := range wants {
		if _, ok := got[w]; !ok {
			t.Errorf("missing %s in result %v", w, sortedKeys(got))
		}
	}
}

func TestEnumerate_BadTransport(t *testing.T) {
	t.Parallel()
	if _, err := (EnumerateConfig{Transport: "bogus"}).Enumerate(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestEnumerate_OCI_MissingBaseDir_NoError(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	fs, err := NewOsFs(tmp)
	if err != nil {
		t.Fatal(err)
	}
	cfg := EnumerateConfig{Transport: skopeo.TransportOci, Fs: fs, BaseDir: tmp}
	got, err := cfg.Enumerate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", sortedKeys(got))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func sortedKeys(s map[string]struct{}) []string {
	out := make([]string, 0, len(s))
	for d := range s {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
