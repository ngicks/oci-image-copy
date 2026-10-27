package transfer_test

import (
	"errors"
	"io/fs"
	"slices"
	"sync"
	"testing"

	"github.com/ngicks/go-fsys-helper/vroot/memfs"
	"github.com/ngicks/go-fsys-helper/vroot/osfs"
	"github.com/opencontainers/go-digest"

	"github.com/ngicks/oci-image-copy/internal/ocidirtest"
	"github.com/ngicks/oci-image-copy/ocidir/fsdir"
	"github.com/ngicks/oci-image-copy/store"
	"github.com/ngicks/oci-image-copy/transfer"
	"github.com/ngicks/oci-image-copy/types"
)

var (
	amd64 = types.Platform{OS: "linux", Architecture: "amd64"}
	arm64 = types.Platform{OS: "linux", Architecture: "arm64"}
)

func memStore() *store.Store {
	return store.New(fsdir.NewStore(memfs.New("store")))
}

func fixtureSource(t *testing.T, f ocidirtest.Fixture) transfer.Source {
	t.Helper()
	fsys := memfs.New("fixture")
	if err := ocidirtest.Write(fsys, f.Files); err != nil {
		t.Fatal(err)
	}
	return transfer.FromOciDir(fsdir.New(fsys, nil))
}

func leaves(t *testing.T, s *store.Store) []string {
	t.Helper()
	var out []string
	for img, err := range s.Walk(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(
			out,
			img.Ref.String()+" "+img.Platform.String()+" "+img.Manifest.Digest.String(),
		)
	}
	slices.Sort(out)
	return out
}

func missingCount(p transfer.Plan) int {
	n := 0
	for _, e := range p.Entries {
		n += len(e.Missing)
	}
	return n
}

func TestPush(t *testing.T) {
	ctx := t.Context()
	f := ocidirtest.New()
	src := fixtureSource(t, f)
	dst := memStore()
	set := types.ImageSetManifest{Images: []string{
		ocidirtest.SingleName,
		ocidirtest.MultiName,
		"example.com/multi@" + f.Multi.Digest.String(),
	}}

	plan, err := transfer.PlanPush(ctx, src, dst, set, []types.Platform{amd64, arm64})
	// The single image exists only for amd64.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("PlanPush: want the arm64 single entry to fail with fs.ErrNotExist, got %v", err)
	}
	entryErr, ok := errors.AsType[*transfer.EntryError](err)
	if !ok || entryErr.Ref.String() != ocidirtest.SingleName || entryErr.Platform != arm64 {
		t.Errorf("PlanPush: want an EntryError naming %s for %s, got %v",
			ocidirtest.SingleName, arm64, err)
	}
	if len(plan.Entries) != 5 {
		t.Fatalf("plan has %d entries, want 5", len(plan.Entries))
	}
	var wantBytes int64
	seen := map[digest.Digest]bool{}
	for _, img := range []ocidirtest.Image{f.Single, f.MultiAmd64, f.MultiArm64} {
		for _, d := range append(append(slices.Clone(img.Layers), img.Config), img.Manifest) {
			if !seen[d.Digest] {
				seen[d.Digest] = true
				wantBytes += d.Size
			}
		}
	}
	if plan.Bytes() != wantBytes {
		t.Errorf("Bytes = %d, want %d (the shared layer counted once)", plan.Bytes(), wantBytes)
	}

	var mu sync.Mutex
	final := map[digest.Digest]transfer.Event{}
	opt := &transfer.Options{Progress: func(e transfer.Event) {
		mu.Lock()
		final[e.Blob] = e
		mu.Unlock()
	}}
	if err := transfer.Push(ctx, plan, dst, opt); err != nil {
		t.Fatal(err)
	}
	if len(final) != len(seen) {
		t.Errorf("progress reported %d blobs, want %d", len(final), len(seen))
	}
	for d, e := range final {
		if e.Done != e.Total {
			t.Errorf("progress for %s ended at %d of %d", d, e.Done, e.Total)
		}
	}

	pin := "example.com/multi@" + f.Multi.Digest.String()
	want := []string{
		"example.com/multi:1 linux/amd64 " + f.MultiAmd64.Manifest.Digest.String(),
		"example.com/multi:1 linux/arm64 " + f.MultiArm64.Manifest.Digest.String(),
		pin + " linux/amd64 " + f.MultiAmd64.Manifest.Digest.String(),
		pin + " linux/arm64 " + f.MultiArm64.Manifest.Digest.String(),
		"example.com/single:1 linux/amd64 " + f.Single.Manifest.Digest.String(),
	}
	slices.Sort(want)
	if got := leaves(t, dst); !slices.Equal(got, want) {
		t.Errorf("leaves:\ngot  %v\nwant %v", got, want)
	}

	again, err := transfer.PlanPush(ctx, src, dst, set, []types.Platform{amd64, arm64})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("re-plan: %v", err)
	}
	if n := missingCount(again); n != 0 || again.Bytes() != 0 {
		t.Errorf("re-plan has %d missing blobs (%d bytes), want none", n, again.Bytes())
	}
	if err := transfer.Push(ctx, again, dst, nil); err != nil {
		t.Errorf("re-push: %v", err)
	}

	t.Run("mirror", func(t *testing.T) {
		mirror := memStore()
		mset := types.ImageSetManifest{Images: set.Images[1:]}
		p, err := transfer.PlanPush(ctx, dst, mirror, mset, []types.Platform{amd64, arm64})
		if err != nil {
			t.Fatal(err)
		}
		if err := transfer.Push(ctx, p, mirror, nil); err != nil {
			t.Fatal(err)
		}
		if got, want := leaves(t, mirror), want[:4]; !slices.Equal(got, want) {
			t.Errorf("mirror leaves:\ngot  %v\nwant %v", got, want)
		}
	})
}

func TestFromOciDir_ubuntu_Local(t *testing.T) {
	ctx := t.Context()
	root, err := osfs.NewRoot(ocidirtest.UbuntuDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	src := transfer.FromOciDir(fsdir.New(root, nil))
	dst := memStore()
	set := types.ImageSetManifest{Images: []string{"docker.io/library/ubuntu:resolute-20261006"}}

	plan, err := transfer.PlanPush(ctx, src, dst, set, []types.Platform{amd64})
	if err != nil {
		t.Fatal(err)
	}
	if n := missingCount(plan); n != 4 {
		t.Errorf("plan has %d missing blobs, want 4 (2 layers, config, manifest)", n)
	}
	if err := transfer.Push(ctx, plan, dst, nil); err != nil {
		t.Fatal(err)
	}
	again, err := transfer.PlanPush(ctx, src, dst, set, []types.Platform{amd64})
	if err != nil {
		t.Fatal(err)
	}
	if n := missingCount(again); n != 0 {
		t.Errorf("re-plan has %d missing blobs, want 0", n)
	}
}
