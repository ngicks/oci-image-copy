package fileserver_test

import (
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy/fileserver"
	"github.com/opencontainers/go-digest"
)

func TestDefaultNaming_ImageMeta(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		prefix string
		ref    string
		want   string
	}{
		{
			name:   "no_prefix_tagged",
			prefix: "",
			ref:    "example.com/repo/path:v1",
			want:   "images/example.com/repo/path/_tags/v1.json",
		},
		{
			name:   "with_prefix_tagged",
			prefix: "my-bucket",
			ref:    "ghcr.io/org/image:latest",
			want:   "my-bucket/images/ghcr.io/org/image/_tags/latest.json",
		},
		{
			name:   "prefix_with_trailing_slash",
			prefix: "bucket/prefix/",
			ref:    "example.com/repo:v2",
			want:   "bucket/prefix/images/example.com/repo/_tags/v2.json",
		},
		{
			name:   "docker_io_ref",
			prefix: "",
			ref:    "docker.io/library/nginx:stable",
			want:   "images/docker.io/library/nginx/_tags/stable.json",
		},
		{
			name:   "deep_repo_path",
			prefix: "pfx",
			ref:    "registry.example.com:5000/a/b/c/d:tag",
			want:   "pfx/images/registry.example.com:5000/a/b/c/d/_tags/tag.json",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := fileserver.DefaultNaming{Prefix: tc.prefix}
			ref, err := imageref.Parse(tc.ref)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.ref, err)
			}
			got := n.ImageMeta(ref)
			if got != tc.want {
				t.Errorf("ImageMeta(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

func TestDefaultNaming_BlobChunk(t *testing.T) {
	t.Parallel()

	hex := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	dgst := digest.Digest("sha256:" + hex)

	cases := []struct {
		name   string
		prefix string
		index  int
		want   string
	}{
		{
			name:   "chunk0_no_prefix",
			prefix: "",
			index:  0,
			want:   "blobs/sha256/" + hex + "/00000000",
		},
		{
			name:   "chunk5_with_prefix",
			prefix: "bucket",
			index:  5,
			want:   "bucket/blobs/sha256/" + hex + "/00000005",
		},
		{
			name:   "large_index",
			prefix: "",
			index:  1024,
			want:   "blobs/sha256/" + hex + "/00001024",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := fileserver.DefaultNaming{Prefix: tc.prefix}
			got := n.BlobChunk(dgst, tc.index)
			if got != tc.want {
				t.Errorf("BlobChunk(index=%d) = %q, want %q", tc.index, got, tc.want)
			}
		})
	}
}

// TestDefaultNaming_BlobChunk_AlgorithmSegment verifies the chunk key carries
// the digest's actual algorithm (decision D13), not a hardcoded "sha256". A
// sha512 digest must land under blobs/sha512/, so it cannot collide with a
// sha256 digest that happens to share the hex prefix.
func TestDefaultNaming_BlobChunk_AlgorithmSegment(t *testing.T) {
	t.Parallel()

	hex512 := "cf83e1357eefb8bdf1542850d66d8007d620e4050b5715dc83f4a921d36ce9ce" +
		"47d0d13c5d85f2b0ff8318d2877eec2f63b931bd47417a81a538327af927da3e"
	dgst := digest.Digest("sha512:" + hex512)

	n := fileserver.DefaultNaming{}
	got := n.BlobChunk(dgst, 0)
	want := "blobs/sha512/" + hex512 + "/00000000"
	if got != want {
		t.Errorf("BlobChunk(sha512) = %q, want %q", got, want)
	}
}
