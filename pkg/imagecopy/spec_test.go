package imagecopy

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

// ────────────────────────────────────────────────────────────────────────────
// ParseRemoteSpec — SSH variant
// ────────────────────────────────────────────────────────────────────────────

func TestParseRemoteSpec_SSH(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in            string
		wantHost      string
		wantUser      string
		wantPort      int
		wantTransport skopeo.Transport
		wantOCIPath   string
	}{
		{
			in:            "ssh://host",
			wantHost:      "host",
			wantTransport: skopeo.TransportContainersStorage,
		},
		{
			in:            "ssh://host/",
			wantHost:      "host",
			wantTransport: skopeo.TransportContainersStorage,
		},
		{
			in:            "ssh://host/containers-storage:",
			wantHost:      "host",
			wantTransport: skopeo.TransportContainersStorage,
		},
		{
			in:            "ssh://host/containers-storage",
			wantHost:      "host",
			wantTransport: skopeo.TransportContainersStorage,
		},
		{
			in:            "ssh://user@host:2222/oci:/srv/oci",
			wantHost:      "host",
			wantUser:      "user",
			wantPort:      2222,
			wantTransport: skopeo.TransportOci,
			wantOCIPath:   "/srv/oci",
		},
		{
			in:            "ssh://host/docker-daemon:",
			wantHost:      "host",
			wantTransport: skopeo.TransportDockerDaemon,
		},
		{
			in:            "ssh://user@host/containers-storage:",
			wantHost:      "host",
			wantUser:      "user",
			wantTransport: skopeo.TransportContainersStorage,
		},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseRemoteSpec(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Kind != RemoteKindSSH {
				t.Fatalf("Kind = %v, want RemoteKindSSH", got.Kind)
			}
			spec := got.SSH
			if spec.Target.Host != tc.wantHost {
				t.Errorf("Host = %q, want %q", spec.Target.Host, tc.wantHost)
			}
			if spec.Target.User != tc.wantUser {
				t.Errorf("User = %q, want %q", spec.Target.User, tc.wantUser)
			}
			if spec.Target.Port != tc.wantPort {
				t.Errorf("Port = %d, want %d", spec.Target.Port, tc.wantPort)
			}
			if spec.Transport != tc.wantTransport {
				t.Errorf("Transport = %q, want %q", spec.Transport, tc.wantTransport)
			}
			if spec.OCIPath != tc.wantOCIPath {
				t.Errorf("OCIPath = %q, want %q", spec.OCIPath, tc.wantOCIPath)
			}
		})
	}
}

func TestParseRemoteSpec_SSH_Errors(t *testing.T) {
	t.Parallel()
	cases := []string{
		"ssh://",             // missing host
		"ssh:///",            // missing host
		"ssh://host/docker:", // docker transport not valid for remote
		"ssh://host/oci:",    // oci without path
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			_, err := ParseRemoteSpec(s)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", s)
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────────
// ParseRemoteSpec — file-server variant
// ────────────────────────────────────────────────────────────────────────────

func TestParseRemoteSpec_FileServer(t *testing.T) {
	t.Parallel()
	cases := []string{
		"file-server:https://host/bucket/prefix",
		"file-server:http://minio.local:9000/bucket",
		"file-server:https://user:secret@host/bucket?token=abc",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			got, err := ParseRemoteSpec(s)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Kind != RemoteKindFileServer {
				t.Fatalf("Kind = %v, want RemoteKindFileServer", got.Kind)
			}
			if got.FileServer == nil {
				t.Fatal("FileServer is nil")
			}
			if got.FileServer.URL == nil {
				t.Fatal("FileServer.URL is nil")
			}
			if got.FileServer.ChunkSize != DefaultChunkSize {
				t.Errorf("ChunkSize = %d, want %d", got.FileServer.ChunkSize, DefaultChunkSize)
			}
		})
	}
}

func TestParseRemoteSpec_FileServer_Errors(t *testing.T) {
	t.Parallel()
	cases := []string{
		"file-server:",        // empty URL
		"file-server:ftp://x", // non-http scheme
		"file-server:s3://x",  // non-http scheme
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			_, err := ParseRemoteSpec(s)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", s)
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────────
// ParseRemoteSpec — local-dir variant
// ────────────────────────────────────────────────────────────────────────────

func TestParseRemoteSpec_LocalDir(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       string
		wantPath string
	}{
		{"oci:/path/to/dir", "/path/to/dir"},
		{"oci:/srv/oci-store", "/srv/oci-store"},
		{"oci:relative/path", "relative/path"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseRemoteSpec(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Kind != RemoteKindLocalDir {
				t.Fatalf("Kind = %v, want RemoteKindLocalDir", got.Kind)
			}
			if got.LocalDir == nil {
				t.Fatal("LocalDir is nil")
			}
			if got.LocalDir.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", got.LocalDir.Path, tc.wantPath)
			}
		})
	}
}

func TestParseRemoteSpec_LocalDir_Error(t *testing.T) {
	t.Parallel()
	_, err := ParseRemoteSpec("oci:")
	if err == nil {
		t.Fatal("expected error for oci: without path")
	}
}

// ────────────────────────────────────────────────────────────────────────────
// ParseRemoteSpec — generic errors
// ────────────────────────────────────────────────────────────────────────────

func TestParseRemoteSpec_Errors(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"bogus",
		"containers-storage:",
		"docker-daemon:",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			_, err := ParseRemoteSpec(s)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", s)
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Redaction helpers
// ────────────────────────────────────────────────────────────────────────────

func TestRedactFileServerURL(t *testing.T) {
	t.Parallel()

	spec, err := ParseRemoteSpec("file-server:https://user:secret@host/bucket?token=abc")
	if err != nil {
		t.Fatal(err)
	}
	redacted := RedactFileServerURL(spec.FileServer.URL)
	if redacted == "" {
		t.Fatal("redacted URL is empty")
	}
	// Must not contain credentials or query string.
	if containsAny(redacted, "secret", "token=abc", "user:") {
		t.Errorf("redacted URL still contains sensitive data: %q", redacted)
	}
	// Must still contain the host.
	if !containsAny(redacted, "host") {
		t.Errorf("redacted URL lost the host: %q", redacted)
	}
}

func TestRedactHeader(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"Authorization: Bearer secret", "Authorization: [redacted]"},
		{"X-Auth-Token: mysecret", "X-Auth-Token: [redacted]"},
		{"nocolon", "[redacted header]"},
	}
	for _, tc := range cases {
		got := RedactHeader(tc.in)
		if got != tc.want {
			t.Errorf("RedactHeader(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// ────────────────────────────────────────────────────────────────────────────
// ParseChunkSize
// ────────────────────────────────────────────────────────────────────────────

func TestParseChunkSize_Valid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want int64
	}{
		{"100MiB", 100 * 1024 * 1024},
		{"1GiB", 1 << 30},
		{"512KiB", 512 * 1024},
		{"100MB", 100_000_000},
		{"1GB", 1_000_000_000},
		{"1KB", 1_000},
		{"1024", 1024},
		{"1B", 1},
		{"1K", 1 << 10},
		{"1M", 1 << 20},
		{"1G", 1 << 30},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseChunkSize(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("ParseChunkSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseChunkSize_Errors(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"0",
		"-1",
		"100Gi",  // not a valid suffix (GiB is, but not Gi)
		"100Mib", // not a valid suffix (MiB is, but not Mib)
		"12x",    // unknown suffix
		"abc",
		"1.5MiB", // decimal not supported
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			t.Parallel()
			_, err := ParseChunkSize(tc)
			if err == nil {
				t.Fatalf("ParseChunkSize(%q): expected error, got nil", tc)
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────────
// ParseRemoteSpec — file-server URL error redaction
// ────────────────────────────────────────────────────────────────────────────

func TestParseRemoteSpec_FileServer_ErrorRedaction(t *testing.T) {
	t.Parallel()
	// A URL that url.Parse() can handle but has invalid scheme — should not
	// leak userinfo or query in error message.
	_, err := ParseRemoteSpec("file-server:ftp://user:secret@host/path?token=abc")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if containsAny(msg, "secret", "token=abc") {
		t.Errorf("error message leaks credentials: %q", msg)
	}
}
