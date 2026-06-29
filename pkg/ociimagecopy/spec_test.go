package ociimagecopy

import (
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
)

// ────────────────────────────────────────────────────────────────────────────
// ParseLocalSpec
// ────────────────────────────────────────────────────────────────────────────

func TestParseLocalSpec_Valid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantTrans skopeo.Transport
		wantPath  string
	}{
		// canonical colon forms
		{"containers-storage:", skopeo.TransportContainersStorage, ""},
		{"docker-daemon:", skopeo.TransportDockerDaemon, ""},
		{"docker:", skopeo.TransportDocker, ""},
		{"oci:/path/to/dir", skopeo.TransportOci, "/path/to/dir"},
		{"oci:/srv/oci-store", skopeo.TransportOci, "/srv/oci-store"},
		// bare (no trailing colon) — also accepted
		{"containers-storage", skopeo.TransportContainersStorage, ""},
		{"docker-daemon", skopeo.TransportDockerDaemon, ""},
		{"docker", skopeo.TransportDocker, ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseLocalSpec(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Transport != tc.wantTrans {
				t.Errorf("Transport = %q, want %q", got.Transport, tc.wantTrans)
			}
			if got.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", got.Path, tc.wantPath)
			}
		})
	}
}

func TestParseLocalSpec_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		wantMsg string
	}{
		{"", "empty"},
		{"oci:", "requires a path"},
		{"s3://bucket", "unrecognised"},
		{"bogus", "unrecognised"},
		{"docker-daemon:something", "unrecognised"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			_, err := ParseLocalSpec(tc.in)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tc.wantMsg != "" {
				if msg := err.Error(); len(msg) == 0 {
					t.Errorf("error message is empty, want something containing %q", tc.wantMsg)
				}
			}
		})
	}
}
