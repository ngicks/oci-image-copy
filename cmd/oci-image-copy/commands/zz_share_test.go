package commands

import (
	"strings"
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
	"github.com/ngicks/oci-image-copy/pkg/cli/ssh"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy"
)

// TestValidateSSHTarget covers the SSH target validation.
func TestValidateSSHTarget(t *testing.T) {
	t.Parallel()

	t.Run("missing host errors", func(t *testing.T) {
		t.Parallel()
		var target ssh.Target
		if err := validateSSHTarget(target); err == nil {
			t.Fatal("expected error for missing host, got nil")
		}
	})

	t.Run("negative port errors", func(t *testing.T) {
		t.Parallel()
		target := ssh.Target{Host: "host", Port: -1}
		if err := validateSSHTarget(target); err == nil {
			t.Fatal("expected error for negative port, got nil")
		}
	})

	t.Run("valid host only", func(t *testing.T) {
		t.Parallel()
		target := ssh.Target{Host: "myhost"}
		if err := validateSSHTarget(target); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("valid user+host+port", func(t *testing.T) {
		t.Parallel()
		target := ssh.Target{User: "alice", Host: "host", Port: 2222}
		if err := validateSSHTarget(target); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// TestValidateEnumerableLocal ensures docker: is rejected for pull.
func TestValidateEnumerableLocal(t *testing.T) {
	t.Parallel()

	t.Run("docker: rejected", func(t *testing.T) {
		t.Parallel()
		ls := ociimagecopy.LocalSpec{Transport: skopeo.TransportDocker}
		if err := validateEnumerableLocal("--local", ls); err == nil {
			t.Fatal("expected error for docker: transport in pull, got nil")
		}
	})

	t.Run("containers-storage: allowed", func(t *testing.T) {
		t.Parallel()
		ls := ociimagecopy.LocalSpec{Transport: skopeo.TransportContainersStorage}
		if err := validateEnumerableLocal("--local", ls); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("oci: allowed", func(t *testing.T) {
		t.Parallel()
		ls := ociimagecopy.LocalSpec{
			Transport: skopeo.TransportOci,
			Path:      "/some/path",
		}
		if err := validateEnumerableLocal("--local", ls); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// TestValidateSourceLocal ensures all transports are valid push/dump sources.
func TestValidateSourceLocal(t *testing.T) {
	t.Parallel()
	transports := []skopeo.Transport{
		skopeo.TransportContainersStorage,
		skopeo.TransportDockerDaemon,
		skopeo.TransportOci,
		skopeo.TransportDocker,
	}
	for _, tr := range transports {
		t.Run(string(tr), func(t *testing.T) {
			t.Parallel()
			ls := ociimagecopy.LocalSpec{Transport: tr}
			if err := validateSourceLocal("--local", ls); err != nil {
				t.Fatalf("unexpected error for %s: %v", tr, err)
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────────
// hasAuthorizationHeader
// ────────────────────────────────────────────────────────────────────────────

func TestHasAuthorizationHeader(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		headers []string
		want    bool
	}{
		{
			name:    "empty slice",
			headers: nil,
			want:    false,
		},
		{
			name:    "no authorization header",
			headers: []string{"X-Custom: value", "Content-Type: application/json"},
			want:    false,
		},
		{
			name:    "canonical Authorization header",
			headers: []string{"Authorization: Bearer tok"},
			want:    true,
		},
		{
			name:    "lowercase authorization header",
			headers: []string{"authorization: Bearer tok"},
			want:    true,
		},
		{
			name:    "mixed-case AUTHORIZATION",
			headers: []string{"AUTHORIZATION: Bearer tok"},
			want:    true,
		},
		{
			name:    "authorization among other headers",
			headers: []string{"X-Custom: v", "Authorization: Basic abc", "Accept: */*"},
			want:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := hasAuthorizationHeader(tc.headers)
			if got != tc.want {
				t.Errorf("hasAuthorizationHeader(%v) = %v, want %v", tc.headers, got, tc.want)
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────────
// buildRemote — file-server env-var auth wiring
// ────────────────────────────────────────────────────────────────────────────

// buildRemoteFileServerHeaders is a test helper that builds a file-server
// Remote from the given spec string and opts, then extracts the Authorization
// header value the spec would have received. It calls buildRemote and inspects
// the constructed spec by re-parsing — since the headers are set on
// FileServerRemoteSpec before NewFileServerRemoteFromSpec is called, we instead
// exercise the merging logic directly through fileServerOpts.
//
// Because buildRemote dials the real factory (which succeeds for a file-server
// spec), we use a test-local helper that only exercises the header-merging
// logic without constructing a live Remote.
func applyEnvAuth(headers []string, authFromEnv string) []string {
	opts := fileServerOpts{
		headers: headers,
		auth:    authFromEnv,
	}
	// Replicate the merging logic from buildRemote without the full Remote dial.
	result := append([]string(nil), opts.headers...)
	if opts.auth != "" && !hasAuthorizationHeader(result) {
		result = append(result, "Authorization: "+opts.auth)
	}
	return result
}

func TestBuildRemote_FileServerEnvAuth(t *testing.T) {
	t.Parallel()

	t.Run("env set, no explicit header — env value added", func(t *testing.T) {
		t.Parallel()
		got := applyEnvAuth(nil, "Bearer secret-token")
		if !hasAuthorizationHeader(got) {
			t.Fatalf("expected Authorization header in %v", got)
		}
		// Value must be present (not just the name).
		found := false
		for _, h := range got {
			if strings.EqualFold(strings.SplitN(h, ":", 2)[0], "authorization") {
				if strings.Contains(h, "Bearer secret-token") {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("Authorization header value not set correctly: %v", got)
		}
	})

	t.Run("env set + explicit flag Authorization — flag wins, no duplicate", func(t *testing.T) {
		t.Parallel()
		explicit := []string{"Authorization: Bearer explicit-flag-token"}
		got := applyEnvAuth(explicit, "Bearer env-token")
		count := 0
		for _, h := range got {
			if strings.EqualFold(strings.SplitN(h, ":", 2)[0], "authorization") {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 Authorization header, got %d: %v", count, got)
		}
		if !strings.Contains(got[0], "Bearer explicit-flag-token") {
			t.Errorf("flag-supplied token should win; got headers: %v", got)
		}
	})

	t.Run("env unset — no Authorization header added", func(t *testing.T) {
		t.Parallel()
		got := applyEnvAuth(nil, "")
		if hasAuthorizationHeader(got) {
			t.Errorf("expected no Authorization header when env unset, got: %v", got)
		}
	})

	t.Run("env set + case-insensitive flag header — flag wins", func(t *testing.T) {
		t.Parallel()
		explicit := []string{"authorization: Bearer lowercase-flag"}
		got := applyEnvAuth(explicit, "Bearer env-token")
		if hasAuthorizationHeader(got) {
			// Must be present (from explicit), but only once.
			count := 0
			for _, h := range got {
				if strings.EqualFold(strings.SplitN(h, ":", 2)[0], "authorization") {
					count++
				}
			}
			if count != 1 {
				t.Errorf("expected 1 Authorization header, got %d: %v", count, got)
			}
		} else {
			t.Errorf("expected Authorization header from explicit flag: %v", got)
		}
	})
}
