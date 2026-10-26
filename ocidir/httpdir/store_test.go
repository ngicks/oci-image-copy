package httpdir_test

import (
	"errors"
	"io/fs"
	"slices"
	"testing"

	"github.com/ngicks/go-fsys-helper/vroot/osfs"
	"github.com/opencontainers/go-digest"

	"github.com/ngicks/oci-image-copy/internal/ocidirtest"
	"github.com/ngicks/oci-image-copy/internal/storetest"
	"github.com/ngicks/oci-image-copy/ocidir"
	"github.com/ngicks/oci-image-copy/ocidir/fsdir"
	"github.com/ngicks/oci-image-copy/ocidir/httpdir"
	"github.com/ngicks/oci-image-copy/store"
	"github.com/ngicks/oci-image-copy/types"
)

func TestStore(t *testing.T) {
	ctx := t.Context()
	f := ocidirtest.New()
	amd64 := types.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := types.Platform{OS: "linux", Architecture: "arm64"}
	single, _ := types.ParseReference(ocidirtest.SingleName)
	multiRepo := "example.com/multi"

	// Fill a filesystem store, then serve its root over HTTP.
	tmp := t.TempDir()
	root, err := osfs.NewRoot(tmp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	fsStore := store.New(fsdir.NewStore(root))
	storetest.PutImage(t, fsStore, f, f.Single)
	storetest.PutImage(t, fsStore, f, f.MultiAmd64)
	storetest.PutImage(t, fsStore, f, f.MultiArm64)
	leaves := []struct {
		ref      types.Reference
		p        types.Platform
		manifest ocidirtest.Image
	}{
		{single, amd64, f.Single},
		{
			types.Reference{Repository: single.Repository, Digest: f.Single.Manifest.Digest},
			amd64,
			f.Single,
		},
		{types.Reference{Repository: multiRepo, Digest: f.Multi.Digest}, amd64, f.MultiAmd64},
	}
	for _, l := range leaves {
		if err := fsStore.Commit(ctx, l.ref, l.p, l.manifest.Manifest); err != nil {
			t.Fatal(err)
		}
	}
	srv := serve(t, tmp)
	s := store.New(httpdir.NewStore(httpdir.BaseURLStore(srv.URL), nil))

	for _, l := range leaves {
		img, err := s.Resolve(ctx, l.ref, l.p)
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
		if got, want := blobs(t, img.Dir), digests(l.manifest); !slices.Equal(got, want) {
			t.Errorf("blobs of %s = %v, want %v", l.ref, got, want)
		}
	}

	// A manifest digest without a leaf resolves from shared/blobs over HTTP too.
	arm := types.Reference{Repository: multiRepo, Digest: f.MultiArm64.Manifest.Digest}
	if _, err := s.Resolve(ctx, arm, arm64); err != nil {
		t.Errorf("Resolve(%s) from shared/blobs: %v", arm, err)
	}

	for _, l := range f.Single.Layers {
		if ok, err := s.HasDiffIDBlob(ctx, l.Digest, l.Digest); err != nil || !ok {
			t.Errorf("diffID marker for %s over HTTP: %v, %v", l.Digest, ok, err)
		}
	}
	if _, err := s.DiffIDBlobs(ctx, f.SharedLayer.Digest); !errors.Is(err, store.ErrNotListable) {
		t.Errorf("DiffIDBlobs over HTTP: want ErrNotListable, got %v", err)
	}

	missing, _ := types.ParseReference("example.com/missing:1")
	if _, err := s.Resolve(ctx, missing, amd64); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Resolve of a missing tag: want fs.ErrNotExist, got %v", err)
	}
	for _, err := range s.Walk(ctx) {
		if !errors.Is(err, store.ErrNotListable) {
			t.Errorf("Walk: want ErrNotListable, got %v", err)
		}
	}
	if err := s.Commit(
		ctx,
		single,
		amd64,
		f.Single.Manifest,
	); !errors.Is(
		err,
		errors.ErrUnsupported,
	) {
		t.Errorf("Commit on a read-only store: want errors.ErrUnsupported, got %v", err)
	}
}

func blobs(t *testing.T, dir ocidir.OciDir) []digest.Digest {
	t.Helper()
	seq, err := ocidir.AllBlobsFromNames(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []digest.Digest
	for b, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		_ = b.Reader.Close()
		out = append(out, b.Name)
	}
	return out
}

func digests(img ocidirtest.Image) []digest.Digest {
	var out []digest.Digest
	for _, l := range img.Layers {
		out = append(out, l.Digest)
	}
	return append(out, img.Config.Digest, img.Manifest.Digest)
}
