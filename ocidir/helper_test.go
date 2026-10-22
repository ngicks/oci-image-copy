package ocidir_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"slices"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ngicks/oci-image-copy/internal/ocidirtest"
	"github.com/ngicks/oci-image-copy/ocidir"
)

type mapDir map[string][]byte

type bytesReader struct{ *bytes.Reader }

func (bytesReader) Close() error { return nil }

func (d mapDir) open(name string) ([]byte, error) {
	b, ok := d[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	return b, nil
}

func (d mapDir) Blobs(ctx context.Context, dgst digest.Digest) (ocidir.ReadAtCloser, error) {
	b, err := d.open(ocidirtest.BlobPath(dgst))
	if err != nil {
		return nil, err
	}
	return bytesReader{bytes.NewReader(b)}, nil
}

func (d mapDir) Index(ctx context.Context) (ocispec.Index, error) {
	var idx ocispec.Index
	b, err := d.IndexBin(ctx)
	if err != nil {
		return idx, err
	}
	return idx, json.Unmarshal(b, &idx)
}

func (d mapDir) OciLayout(ctx context.Context) (ocispec.ImageLayout, error) {
	var l ocispec.ImageLayout
	b, err := d.OciLayoutBin(ctx)
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(b, &l)
}

func (d mapDir) IndexBin(ctx context.Context) ([]byte, error) {
	return d.open(ocispec.ImageIndexFile)
}

func (d mapDir) OciLayoutBin(ctx context.Context) ([]byte, error) {
	return d.open(ocispec.ImageLayoutFile)
}

func blobsOf(img ocidirtest.Image) []digest.Digest {
	var out []digest.Digest
	for _, l := range img.Layers {
		out = append(out, l.Digest)
	}
	return append(out, img.Config.Digest, img.Manifest.Digest)
}

func dedup(groups ...[]digest.Digest) []digest.Digest {
	var out []digest.Digest
	for _, g := range groups {
		for _, d := range g {
			if !slices.Contains(out, d) {
				out = append(out, d)
			}
		}
	}
	return out
}

func collect(
	t *testing.T,
	f ocidirtest.Fixture,
	seq func(func(ocidir.Blob, error) bool),
) []digest.Digest {
	t.Helper()
	var got []digest.Digest
	for blob, err := range seq {
		if err != nil {
			t.Fatalf("yield %q: %v", blob.Name, err)
		}
		b, err := io.ReadAll(io.NewSectionReader(blob.Reader, 0, blob.Reader.Size()))
		_ = blob.Reader.Close()
		if err != nil {
			t.Fatalf("read %q: %v", blob.Name, err)
		}
		if !bytes.Equal(b, f.Files[ocidirtest.BlobPath(blob.Name)]) {
			t.Errorf("content of %q mismatch", blob.Name)
		}
		got = append(got, blob.Name)
	}
	return got
}

func TestAllImages(t *testing.T) {
	f := ocidirtest.New()
	seq, err := ocidir.AllImages(t.Context(), mapDir(f.Files))
	if err != nil {
		t.Fatal(err)
	}

	type entry struct {
		Name  string
		Image ocispec.Image
	}
	var got []entry
	for name, img := range seq {
		got = append(got, entry{name, img})
	}
	want := []entry{
		{ocidirtest.SingleName, f.Single.Image},
		{ocidirtest.MultiName, f.MultiAmd64.Image},
		{ocidirtest.MultiName, f.MultiArm64.Image},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("not equal:\ngot  = %#v\nwant = %#v", got, want)
	}
}

func TestAllBlobsFromNames(t *testing.T) {
	f := ocidirtest.New()
	multi := dedup(blobsOf(f.MultiAmd64), blobsOf(f.MultiArm64), []digest.Digest{f.Multi.Digest})

	for _, tc := range []struct {
		name  string
		names []string
		want  []digest.Digest
	}{
		{"single", []string{ocidirtest.SingleName}, blobsOf(f.Single)},
		{"multi", []string{ocidirtest.MultiName}, multi},
		{
			"both",
			[]string{ocidirtest.SingleName, ocidirtest.MultiName},
			dedup(blobsOf(f.Single), multi),
		},
		{"everything", nil, dedup(blobsOf(f.Single), multi, blobsOf(f.Unnamed))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seq, err := ocidir.AllBlobsFromNames(t.Context(), mapDir(f.Files), tc.names...)
			if err != nil {
				t.Fatal(err)
			}
			if got := collect(t, f, seq); !slices.Equal(got, tc.want) {
				t.Errorf("not equal:\ngot  = %v\nwant = %v", got, tc.want)
			}
		})
	}
}

func TestAllBlobsFromDigests(t *testing.T) {
	f := ocidirtest.New()

	for _, tc := range []struct {
		name    string
		digests []digest.Digest
		want    []digest.Digest
	}{
		{"index child", []digest.Digest{f.MultiArm64.Manifest.Digest}, blobsOf(f.MultiArm64)},
		{"unnamed", []digest.Digest{f.Unnamed.Manifest.Digest}, blobsOf(f.Unnamed)},
		{
			"index and child",
			[]digest.Digest{f.Multi.Digest, f.MultiAmd64.Manifest.Digest},
			dedup(blobsOf(f.MultiAmd64), blobsOf(f.MultiArm64), []digest.Digest{f.Multi.Digest}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seq, err := ocidir.AllBlobsFromDigests(t.Context(), mapDir(f.Files), tc.digests...)
			if err != nil {
				t.Fatal(err)
			}
			if got := collect(t, f, seq); !slices.Equal(got, tc.want) {
				t.Errorf("not equal:\ngot  = %v\nwant = %v", got, tc.want)
			}
		})
	}
}

func TestAllBlobs_unknown(t *testing.T) {
	f := ocidirtest.New()
	dir := mapDir(f.Files)

	if _, err := ocidir.AllBlobsFromNames(
		t.Context(),
		dir,
		"example.com/missing:1",
	); !errors.Is(
		err,
		fs.ErrNotExist,
	) {
		t.Errorf("unknown name: want fs.ErrNotExist, got %v", err)
	}
	// A config is a blob but does not select an image.
	if _, err := ocidir.AllBlobsFromDigests(
		t.Context(),
		dir,
		f.Single.Config.Digest,
	); !errors.Is(
		err,
		fs.ErrNotExist,
	) {
		t.Errorf("unknown digest: want fs.ErrNotExist, got %v", err)
	}
}

func TestAllBlobs_missingBlob(t *testing.T) {
	f := ocidirtest.New()
	delete(f.Files, ocidirtest.BlobPath(f.Single.Layers[1].Digest))

	seq, err := ocidir.AllBlobsFromNames(t.Context(), mapDir(f.Files), ocidirtest.SingleName)
	if err != nil {
		t.Fatal(err)
	}
	var failed []digest.Digest
	for blob, err := range seq {
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("want fs.ErrNotExist, got %v", err)
			}
			failed = append(failed, blob.Name)
			continue
		}
		_ = blob.Reader.Close()
	}
	if want := []digest.Digest{f.Single.Layers[1].Digest}; !slices.Equal(failed, want) {
		t.Errorf("failed = %v, want %v", failed, want)
	}
}
