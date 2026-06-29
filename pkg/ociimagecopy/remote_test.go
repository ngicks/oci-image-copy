package ociimagecopy

// Tests for parseDumpDirRel — the read-side parser that reconstructs an
// ImageRef from an on-peer dump-dir path. It backs ListImagesFromFs, so it
// lives in this package alongside the FS-enumeration helpers (the concrete
// remote implementations that call ListImagesFromFs live in package remote).

import "testing"

// TestParseDumpDirRel_Valid checks that well-formed dump-dir paths reconstruct
// the expected ImageRef.
func TestParseDumpDirRel_Valid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		rel     string
		wantStr string
	}{
		{"tagged", "ghcr.io/a/b/_tags/v1", "ghcr.io/a/b:v1"},
		{
			"digested",
			"ghcr.io/a/b/_digests/" + repeat("a", 64),
			"ghcr.io/a/b@sha256:" + repeat("a", 64),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := parseDumpDirRel(tc.rel)
			if err != nil {
				t.Fatalf("parseDumpDirRel(%q): %v", tc.rel, err)
			}
			if ref.String() != tc.wantStr {
				t.Errorf("got %q, want %q", ref.String(), tc.wantStr)
			}
		})
	}
}

// TestParseDumpDirRel_RejectsMalicious checks the read-side validation guards
// against maliciously-named dump dirs on a peer (traversal host/segment,
// slash/overlong tag, bad digest hex).
func TestParseDumpDirRel_RejectsMalicious(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, rel string }{
		{"traversal segment", "ghcr.io/a/../b/_tags/v1"},
		{"overlong tag", "ghcr.io/a/b/_tags/" + repeat("x", 129)},
		{"bad tag leading char", "ghcr.io/a/b/_tags/.bad"},
		{"reserved segment", "ghcr.io/a/_tags/b/_tags/v1"},
		{"short digest hex", "ghcr.io/a/b/_digests/abc"},
		{"non-hex digest", "ghcr.io/a/b/_digests/" + repeat("z", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseDumpDirRel(tc.rel); err == nil {
				t.Fatalf("parseDumpDirRel(%q) = nil error, want rejection", tc.rel)
			}
		})
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}
