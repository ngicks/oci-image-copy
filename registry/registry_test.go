package registry_test

import (
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ngicks/go-fsys-helper/vroot/memfs"
	"github.com/ngicks/go-fsys-helper/vroot/osfs"
	"github.com/opencontainers/go-digest"
	imagetypes "go.podman.io/image/v5/types"

	"github.com/ngicks/oci-image-copy/blobcache"
	"github.com/ngicks/oci-image-copy/internal/ocidirtest"
	"github.com/ngicks/oci-image-copy/ocidir/fsdir"
	"github.com/ngicks/oci-image-copy/registry"
	"github.com/ngicks/oci-image-copy/store"
	"github.com/ngicks/oci-image-copy/transfer"
	"github.com/ngicks/oci-image-copy/types"
)

// Pinned so test runs stay reproducible.
const registryImage = "docker.io/library/registry:2.8.3"

// startRegistry runs a throwaway registry and returns its host:port.
func startRegistry(t *testing.T) string {
	t.Helper()
	for _, bin := range []string{"podman", "skopeo"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	// Host networking with a pre-picked port, because containers.conf may force netns=host,
	// which silently drops -p port publishing.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	out, err := exec.Command(
		"podman", "run", "-d", "--rm", "--network", "host",
		"-e", "REGISTRY_HTTP_ADDR="+addr,
		registryImage,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("podman run: %v\n%s", err, out)
	}
	id := strings.TrimSpace(
		string(out[strings.LastIndexByte(strings.TrimSpace(string(out)), '\n')+1:]),
	)
	t.Cleanup(func() { _ = exec.Command("podman", "rm", "-f", id).Run() })

	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/v2/")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return addr
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("registry not ready: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func skopeoCopy(t *testing.T, src, dst string) {
	t.Helper()
	out, err := exec.Command(
		"skopeo", "copy", "--all", "--preserve-digests", "--dest-tls-verify=false", src, dst,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("skopeo copy %s %s: %v\n%s", src, dst, err, out)
	}
}

func TestSource_Local(t *testing.T) {
	addr := startRegistry(t)
	ctx := t.Context()
	f := ocidirtest.New()

	dir := t.TempDir()
	root, err := osfs.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if err := ocidirtest.Write(root, f.Files); err != nil {
		t.Fatal(err)
	}
	skopeoCopy(t, "oci:"+dir+":"+ocidirtest.MultiName, "docker://"+addr+"/multi:1")

	cache, err := blobcache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	sys := &imagetypes.SystemContext{DockerInsecureSkipTLSVerify: imagetypes.OptionalBoolTrue}
	amd64 := types.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := types.Platform{OS: "linux", Architecture: "arm64"}
	parse := func(s string) types.Reference {
		r, err := types.ParseReference(s)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	repo := addr + "/multi"
	direct := registry.New(cache, &registry.Options{SystemContext: sys})
	mirrored := registry.New(
		cache,
		&registry.Options{Registries: []string{addr}, SystemContext: sys},
	)

	for _, tc := range []struct {
		name string
		src  *registry.Source
		ref  types.Reference
		p    types.Platform
		want ocidirtest.Image
	}{
		{"tag", direct, parse(repo + ":1"), arm64, f.MultiArm64},
		{"index digest", direct, parse(repo + "@" + f.Multi.Digest.String()), amd64, f.MultiAmd64},
		{
			"manifest digest",
			direct,
			parse(repo + "@" + f.MultiArm64.Manifest.Digest.String()),
			arm64,
			f.MultiArm64,
		},
		{"mirror", mirrored, parse("unreachable.invalid/multi:1"), amd64, f.MultiAmd64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img, err := tc.src.Resolve(ctx, tc.ref, tc.p)
			if err != nil {
				t.Fatal(err)
			}
			if img.Manifest.Digest != tc.want.Manifest.Digest {
				t.Errorf(
					"manifest %s, want the original %s",
					img.Manifest.Digest,
					tc.want.Manifest.Digest,
				)
			}
			descs, err := transfer.ImageBlobs(ctx, img)
			if err != nil {
				t.Fatal(err)
			}
			var got []digest.Digest
			for _, d := range descs {
				got = append(got, d.Digest)
				if !cache.Has(d.Digest) {
					t.Errorf("blob %s missing from the cache", d.Digest)
				}
			}
			var want []digest.Digest
			for _, l := range tc.want.Layers {
				want = append(want, l.Digest)
			}
			want = append(want, tc.want.Config.Digest, tc.want.Manifest.Digest)
			if !slices.Equal(got, want) {
				t.Errorf("blobs %v, want %v", got, want)
			}

			dst := store.New(fsdir.NewStore(memfs.New("store")))
			e, err := transfer.Diff(ctx, img, dst)
			if err != nil {
				t.Fatal(err)
			}
			if err := transfer.Push(
				ctx,
				transfer.Plan{Entries: []transfer.Entry{e}},
				dst,
				nil,
			); err != nil {
				t.Fatal(err)
			}
			stored, err := dst.Resolve(ctx, tc.ref, tc.p)
			if err != nil || stored.Manifest.Digest != tc.want.Manifest.Digest {
				t.Errorf(
					"store holds %v, %v; want the original manifest",
					stored.Manifest.Digest,
					err,
				)
			}
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		_, err := direct.Resolve(ctx, parse("unreachable.invalid/multi:1"), amd64)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("want fs.ErrNotExist, got %v", err)
		}
	})
}
