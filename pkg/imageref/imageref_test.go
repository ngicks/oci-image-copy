package imageref

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		in       string
		wantHost string
		wantPath string
		wantTag  string
		wantDig  string
		wantStr  string
	}{
		{
			name:     "docker hub library implicit",
			in:       "nginx:latest",
			wantHost: "docker.io",
			wantPath: "library/nginx",
			wantTag:  "latest",
			wantStr:  "docker.io/library/nginx:latest",
		},
		{
			name:     "docker hub default tag",
			in:       "nginx",
			wantHost: "docker.io",
			wantPath: "library/nginx",
			wantTag:  "latest",
			wantStr:  "docker.io/library/nginx:latest",
		},
		{
			name:     "docker hub explicit",
			in:       "docker.io/library/nginx:latest",
			wantHost: "docker.io",
			wantPath: "library/nginx",
			wantTag:  "latest",
			wantStr:  "docker.io/library/nginx:latest",
		},
		{
			name:     "docker hub explicit with single segment",
			in:       "docker.io/nginx:latest",
			wantHost: "docker.io",
			wantPath: "library/nginx",
			wantTag:  "latest",
			wantStr:  "docker.io/library/nginx:latest",
		},
		{
			name:     "ghcr 3 segments",
			in:       "ghcr.io/a/b/c:d",
			wantHost: "ghcr.io",
			wantPath: "a/b/c",
			wantTag:  "d",
			wantStr:  "ghcr.io/a/b/c:d",
		},
		{
			name:     "ghcr 4 segments overlapping prefix",
			in:       "ghcr.io/a/b/c/d:latest",
			wantHost: "ghcr.io",
			wantPath: "a/b/c/d",
			wantTag:  "latest",
			wantStr:  "ghcr.io/a/b/c/d:latest",
		},
		{
			name:     "registry with port",
			in:       "registry.example.com:5000/team/proj/sub/app:tag",
			wantHost: "registry.example.com:5000",
			wantPath: "team/proj/sub/app",
			wantTag:  "tag",
			wantStr:  "registry.example.com:5000/team/proj/sub/app:tag",
		},
		{
			name:     "localhost",
			in:       "localhost/devenv/devenv:0.0.61",
			wantHost: "localhost",
			wantPath: "devenv/devenv",
			wantTag:  "0.0.61",
			wantStr:  "localhost/devenv/devenv:0.0.61",
		},
		{
			name:     "digest pinned",
			in:       "ghcr.io/a/b@sha256:" + strings.Repeat("a", 64),
			wantHost: "ghcr.io",
			wantPath: "a/b",
			wantDig:  strings.Repeat("a", 64),
			wantStr:  "ghcr.io/a/b@sha256:" + strings.Repeat("a", 64),
		},
		{
			name:     "digest pinned with port",
			in:       "registry.example.com:5000/x@sha256:" + strings.Repeat("0", 64),
			wantHost: "registry.example.com:5000",
			wantPath: "x", // no library canonicalization on non-docker.io
			wantDig:  strings.Repeat("0", 64),
			wantStr:  "registry.example.com:5000/x@sha256:" + strings.Repeat("0", 64),
		},
		{
			name:     "uppercase host lowercased and canonicalized",
			in:       "DOCKER.IO/nginx:latest",
			wantHost: "docker.io",
			wantPath: "library/nginx",
			wantTag:  "latest",
			wantStr:  "docker.io/library/nginx:latest",
		},
		{
			name:     "mixed-case registry host lowercased",
			in:       "GHCR.io/Org/Image:Tag",
			wantHost: "ghcr.io",
			wantPath: "Org/Image", // path case preserved (only host is lowercased)
			wantTag:  "Tag",
			wantStr:  "ghcr.io/Org/Image:Tag",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tc.in, err)
			}
			if got.Host != tc.wantHost {
				t.Errorf("Host: got %q, want %q", got.Host, tc.wantHost)
			}
			if got.Path != tc.wantPath {
				t.Errorf("Path: got %q, want %q", got.Path, tc.wantPath)
			}
			if got.Tag != tc.wantTag {
				t.Errorf("Tag: got %q, want %q", got.Tag, tc.wantTag)
			}
			if got.Digest != tc.wantDig {
				t.Errorf("Digest: got %q, want %q", got.Digest, tc.wantDig)
			}
			if got.String() != tc.wantStr {
				t.Errorf("String: got %q, want %q", got.String(), tc.wantStr)
			}
			if got.Original != tc.in {
				t.Errorf("Original: got %q, want %q", got.Original, tc.in)
			}
		})
	}
}

func TestParse_Errors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "empty"},
		{"empty tag", "nginx:", "empty tag"},
		{"reserved _tags", "ghcr.io/a/_tags/b:1", "reserved segment"},
		{"reserved _digests", "ghcr.io/a/_digests/b:1", "reserved segment"},
		{"digest missing prefix", "ghcr.io/a/b@deadbeef", `must start with "sha256:"`},
		{"digest short hex", "ghcr.io/a/b@sha256:abc", "expected 64 hex"},
		{"digest non-hex", "ghcr.io/a/b@sha256:" + strings.Repeat("z", 64), "non-hex"},
		// Path traversal in path segments (defense-in-depth for the on-disk layout).
		{"parent-dir segment", "ghcr.io/a/../b:1", "path traversal"},
		{"dot segment", "ghcr.io/a/./b:1", "path traversal"},
		// looksLikeHost("..") is true (contains '.'), so this parses host="..";
		// validateHost must reject it.
		{"traversal host", "../../x:latest", "path traversal"},
		// Tag grammar. (Parse's own tokenizer cannot put a slash in a tag —
		// the slash-in-tag guard is exercised on the read side via
		// ValidateHostPathTag; see TestValidateHostPathTag.)
		{
			"overlong tag",
			"ghcr.io/a/b:" + strings.Repeat("x", 129),
			"too long",
		},
		{"tag bad leading char", "ghcr.io/a/b:.bad", "must start with"},
		{"tag invalid char", "ghcr.io/a/b:good$tag", "invalid character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.in)
			if err == nil {
				t.Fatalf("Parse(%q) expected error containing %q, got nil", tc.in, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse(%q) error = %v, want substring %q", tc.in, err, tc.want)
			}
		})
	}
}

// TestValidateHostPathTag exercises the shared read-side validator that guards
// dump-dir reconstruction against maliciously-named directories on a peer.
func TestValidateHostPathTag(t *testing.T) {
	t.Parallel()

	ok := []struct{ host, path, tag string }{
		{"ghcr.io", "org/image", "v1"},
		{"docker.io", "library/nginx", ""}, // empty tag allowed (digest ref)
		{"registry:5000", "a/b/c", "Tag-1_2.3"},
	}
	for _, c := range ok {
		if err := ValidateHostPathTag(c.host, c.path, c.tag); err != nil {
			t.Errorf("ValidateHostPathTag(%q,%q,%q) = %v, want nil", c.host, c.path, c.tag, err)
		}
	}

	bad := []struct {
		name            string
		host, path, tag string
		wantSubstr      string
	}{
		{"traversal host", "..", "a/b", "v1", "path traversal"},
		{"slash host", "a/b", "x", "v1", "path separator"},
		{"empty path", "ghcr.io", "", "v1", "missing repository path"},
		{"traversal path segment", "ghcr.io", "a/../b", "v1", "path traversal"},
		{"reserved path segment", "ghcr.io", "a/_tags/b", "v1", "reserved segment"},
		{"slash in tag", "ghcr.io", "a/b", "foo/bar", "invalid character"},
		{"overlong tag", "ghcr.io", "a/b", strings.Repeat("x", 129), "too long"},
		{"control char in tag", "ghcr.io", "a/b", "v\x001", "invalid character"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateHostPathTag(c.host, c.path, c.tag)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantSubstr)
			}
			if !strings.Contains(err.Error(), c.wantSubstr) {
				t.Fatalf("error = %v, want substring %q", err, c.wantSubstr)
			}
		})
	}
}

func TestValidateDigestHex(t *testing.T) {
	t.Parallel()
	if err := ValidateDigestHex(strings.Repeat("a", 64)); err != nil {
		t.Errorf("valid hex rejected: %v", err)
	}
	if err := ValidateDigestHex("abc"); err == nil {
		t.Error("short hex accepted")
	}
	if err := ValidateDigestHex(strings.Repeat("z", 64)); err == nil {
		t.Error("non-hex accepted")
	}
}
