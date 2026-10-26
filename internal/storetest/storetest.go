// Package storetest checks a store.Backend through store.Store against the synthetic ocidirtest
// fixture.
package storetest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"slices"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ngicks/oci-image-copy/internal/ocidirtest"
	"github.com/ngicks/oci-image-copy/ocidir"
	"github.com/ngicks/oci-image-copy/store"
	"github.com/ngicks/oci-image-copy/types"
)

var (
	amd64 = types.Platform{OS: "linux", Architecture: "amd64"}
	arm64 = types.Platform{OS: "linux", Architecture: "arm64"}
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read of an existing blob") }

// PutImage stores every blob of img taken from f.
func PutImage(t *testing.T, s *store.Store, f ocidirtest.Fixture, img ocidirtest.Image) {
	t.Helper()
	descs := append(slices.Clone(img.Layers), img.Config, img.Manifest)
	for _, d := range descs {
		PutBlob(t, s, f.Files[ocidirtest.BlobPath(d.Digest)], d)
	}
}

func PutBlob(t *testing.T, s *store.Store, b []byte, d ocispec.Descriptor) {
	t.Helper()
	if err := s.PutBlob(t.Context(), d, bytes.NewReader(b)); err != nil {
		t.Fatalf("PutBlob(%s): %v", d.Digest, err)
	}
}

func ref(t *testing.T, s string) types.Reference {
	t.Helper()
	r, err := types.ParseReference(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func digestRef(repo string, d digest.Digest) types.Reference {
	return types.Reference{Repository: repo, Digest: d}
}

func blobsOf(t *testing.T, img store.Image) []digest.Digest {
	t.Helper()
	seq, err := ocidir.AllBlobsFromNames(t.Context(), img.Dir)
	if err != nil {
		t.Fatalf("AllBlobsFromNames on %s: %v", img.Ref, err)
	}
	var out []digest.Digest
	for b, err := range seq {
		if err != nil {
			t.Fatalf("blob %s of %s: %v", b.Name, img.Ref, err)
		}
		_ = b.Reader.Close()
		out = append(out, b.Name)
	}
	return out
}

func want(img ocidirtest.Image) []digest.Digest {
	var out []digest.Digest
	for _, l := range img.Layers {
		out = append(out, l.Digest)
	}
	return append(out, img.Config.Digest, img.Manifest.Digest)
}

// TestBackend runs the store cases against stores returned by newStore, each one empty.
// listable tells whether the backend implements store.Lister.
func TestBackend(t *testing.T, newStore func(t *testing.T) *store.Store, listable bool) {
	f := ocidirtest.New()
	single := ref(t, ocidirtest.SingleName)
	multiRepo := ref(t, ocidirtest.MultiName).Repository

	t.Run("commit and resolve", func(t *testing.T) {
		s := newStore(t)
		PutImage(t, s, f, f.Single)
		PutImage(t, s, f, f.MultiAmd64)
		ctx := t.Context()

		leaves := []struct {
			ref      types.Reference
			manifest ocidirtest.Image
		}{
			{single, f.Single},
			{digestRef(single.Repository, f.Single.Manifest.Digest), f.Single},
			// index pin: the index blob itself is never stored
			{digestRef(multiRepo, f.Multi.Digest), f.MultiAmd64},
		}
		for _, l := range leaves {
			if err := s.Commit(ctx, l.ref, amd64, l.manifest.Manifest); err != nil {
				t.Fatalf("Commit(%s): %v", l.ref, err)
			}
			img, err := s.Resolve(ctx, l.ref, amd64)
			if err != nil {
				t.Fatalf("Resolve(%s): %v", l.ref, err)
			}
			if img.Manifest.Digest != l.manifest.Manifest.Digest {
				t.Errorf(
					"Resolve(%s).Manifest = %s, want %s",
					l.ref,
					img.Manifest.Digest,
					l.manifest.Manifest.Digest,
				)
			}
			if got := blobsOf(t, img); !slices.Equal(got, want(l.manifest)) {
				t.Errorf("blobs of %s = %v, want %v", l.ref, got, want(l.manifest))
			}
			// the same manifest again is a no-op
			if err := s.Commit(ctx, l.ref, amd64, l.manifest.Manifest); err != nil {
				t.Errorf("re-Commit(%s): %v", l.ref, err)
			}
		}
		if ok, _ := s.HasBlob(ctx, f.Multi.Digest); ok {
			t.Error("the index blob was stored")
		}

		if err := s.Commit(
			ctx,
			single,
			amd64,
			f.MultiAmd64.Manifest,
		); !errors.Is(
			err,
			store.ErrConflict,
		) {
			t.Errorf("conflicting Commit: want ErrConflict, got %v", err)
		}

		var walked []string
		for img, err := range s.Walk(ctx) {
			if !listable {
				if !errors.Is(err, store.ErrNotListable) {
					t.Errorf("Walk on a non-listable backend: want ErrNotListable, got %v", err)
				}
				break
			}
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			walked = append(
				walked,
				img.Ref.String()+" "+img.Platform.String()+" "+img.Manifest.Digest.String(),
			)
		}
		if listable {
			var wantWalk []string
			for _, l := range leaves {
				wantWalk = append(
					wantWalk,
					l.ref.String()+" "+amd64.String()+" "+l.manifest.Manifest.Digest.String(),
				)
			}
			slices.Sort(walked)
			slices.Sort(wantWalk)
			if !slices.Equal(walked, wantWalk) {
				t.Errorf("Walk:\ngot  %v\nwant %v", walked, wantWalk)
			}
		}
	})

	t.Run("manifest digest without leaf", func(t *testing.T) {
		s := newStore(t)
		PutImage(t, s, f, f.MultiArm64)
		r := digestRef(multiRepo, f.MultiArm64.Manifest.Digest)
		img, err := s.Resolve(t.Context(), r, arm64)
		if err != nil {
			t.Fatal(err)
		}
		if got := blobsOf(t, img); !slices.Equal(got, want(f.MultiArm64)) {
			t.Errorf("blobs = %v, want %v", got, want(f.MultiArm64))
		}
		if _, err := s.Resolve(t.Context(), single, amd64); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Resolve of an absent tag: want fs.ErrNotExist, got %v", err)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		s := newStore(t)
		ctx := t.Context()

		// manifest and config only: layers missing
		for _, d := range []ocispec.Descriptor{f.Single.Config, f.Single.Manifest} {
			PutBlob(t, s, f.Files[ocidirtest.BlobPath(d.Digest)], d)
		}
		if err := s.Commit(ctx, single, amd64, f.Single.Manifest); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("missing layer: want fs.ErrNotExist, got %v", err)
		}
		if _, err := s.Resolve(ctx, single, amd64); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a refused Commit left a leaf: %v", err)
		}

		PutImage(t, s, f, f.Single)
		if err := s.Commit(
			ctx,
			single,
			arm64,
			f.Single.Manifest,
		); !errors.Is(
			err,
			store.ErrPlatform,
		) {
			t.Errorf("wrong platform: want ErrPlatform, got %v", err)
		}
		if err := s.Commit(ctx, single, amd64, f.Multi); err == nil {
			t.Error("committing an image index: want error")
		}

		foreign := f.Single.Layers[0]
		foreign.MediaType = "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"
		m, _ := json.Marshal(ocispec.Manifest{
			SchemaVersion: 2,
			MediaType:     ocispec.MediaTypeImageManifest,
			Config:        f.Single.Config,
			Layers:        []ocispec.Descriptor{foreign},
		})
		md := ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    digest.FromBytes(m),
			Size:      int64(len(m)),
		}
		PutBlob(t, s, m, md)
		if err := s.Commit(ctx, single, amd64, md); !errors.Is(err, store.ErrForeignLayer) {
			t.Errorf("foreign layer: want ErrForeignLayer, got %v", err)
		}

		absent := []byte("absent blob")
		ad := ocispec.Descriptor{Digest: digest.FromBytes(absent), Size: int64(len(absent))}
		if err := s.PutBlob(ctx, ad, bytes.NewReader([]byte("other bytes"))); err == nil {
			t.Error("PutBlob with mismatching content: want error")
		}
		if ok, _ := s.HasBlob(ctx, ad.Digest); ok {
			t.Error("mismatching content was stored")
		}
		// an existing blob is not read again
		if err := s.PutBlob(ctx, f.Single.Layers[1], errReader{}); err != nil {
			t.Errorf("PutBlob of an existing blob: %v", err)
		}
	})
}
