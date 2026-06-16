package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/ngicks/oci-image-copy/pkg/cli/ssh"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy/remote"
)

// fileServerOpts holds the companion flags for a --remote file-server:...
// spec. They supplement the URL parsed from the spec.
type fileServerOpts struct {
	headers      []string
	chunkSize    string // human-readable (parsed by ociimagecopy.ParseChunkSize)
	namingPrefix string
	// auth carries the resolved file-server Authorization header value (from the
	// service config, defaults < file < env). It is added as
	// "Authorization: <value>" only when no explicit Authorization header is
	// present in headers (case-insensitive).
	// The value must never appear in logs or error messages.
	auth string
}

// initShare parses --local and --remote specs, builds a [*ociimagecopy.Local]
// and a [ociimagecopy.Remote], and wraps them in a [*ociimagecopy.Share].
//
// For SSH remotes it validates the target and runs an ssh probe before
// dialing SFTP. For local-directory remotes no probe is needed.
// The caller is responsible for share.Close().
func initShare(
	ctx context.Context,
	localSpec string,
	remoteSpec string,
	localDumpDir string,
	fsOpts fileServerOpts,
) (*ociimagecopy.Share, error) {
	ls, err := ociimagecopy.ParseLocalSpec(localSpec)
	if err != nil {
		return nil, err
	}

	local, err := ociimagecopy.NewLocal(ctx, ociimagecopy.LocalConfig{
		BaseDir:   localDumpDir,
		Transport: ls.Transport,
		OCIPath:   ls.Path,
	})
	if err != nil {
		return nil, err
	}

	remote, err := buildRemote(ctx, remoteSpec, fsOpts)
	if err != nil {
		return nil, err
	}

	return ociimagecopy.NewShare(local, remote), nil
}

// buildRemote parses a --remote spec and constructs the appropriate Remote.
// fsOpts supplements a file-server spec with companion flag values.
func buildRemote(
	ctx context.Context,
	remoteSpec string,
	fsOpts fileServerOpts,
) (ociimagecopy.Remote, error) {
	rs, err := ociimagecopy.ParseRemoteSpec(remoteSpec)
	if err != nil {
		return nil, err
	}

	switch rs.Kind {
	case ociimagecopy.RemoteKindSSH:
		return buildSSHRemote(ctx, rs.SSH)
	case ociimagecopy.RemoteKindLocalDir:
		return remote.NewLocalDir(rs.LocalDir.Path)
	case ociimagecopy.RemoteKindFileServer:
		spec := rs.FileServer
		// Merge companion flag values into the spec.
		if len(fsOpts.headers) > 0 {
			spec.Headers = fsOpts.headers
		}
		if fsOpts.chunkSize != "" {
			cs, err := ociimagecopy.ParseChunkSize(fsOpts.chunkSize)
			if err != nil {
				return nil, fmt.Errorf("--remote-chunk-size: %w", err)
			}
			spec.ChunkSize = cs
		}
		if fsOpts.namingPrefix != "" {
			spec.NamingPrefix = fsOpts.namingPrefix
		}
		// Apply config-resolved auth only when no explicit Authorization header
		// was supplied via --remote-header (flag wins; comparison is
		// case-insensitive per HTTP semantics).
		if fsOpts.auth != "" && !hasAuthorizationHeader(spec.Headers) {
			spec.Headers = append(spec.Headers, "Authorization: "+fsOpts.auth)
		}
		return remote.NewFileServerFromSpec(spec)
	default:
		return nil, fmt.Errorf("internal: unknown remote kind %v", rs.Kind)
	}
}

// validateSourceLocal rejects --local specs that cannot act as a push/dump
// source. All transports including docker: are valid sources.
func validateSourceLocal(flag string, ls ociimagecopy.LocalSpec) error {
	// All supported local transports are valid as push/dump sources.
	// ParseLocalSpec already rejects unknown transports, so no extra work needed.
	_ = flag
	_ = ls
	return nil
}

// validateEnumerableLocal rejects --local specs that cannot be enumerated
// or loaded into. docker: transport is a push/dump source only.
func validateEnumerableLocal(flag string, ls ociimagecopy.LocalSpec) error {
	switch ls.Transport {
	case "docker":
		return fmt.Errorf(
			"%s: docker: transport cannot be used as a pull destination "+
				"(it cannot be enumerated or loaded into; use containers-storage:, "+
				"docker-daemon:, or oci:/path)",
			flag,
		)
	}
	return nil
}

// buildSSHRemote validates and dials the SSH-backed remote from a parsed spec.
func buildSSHRemote(ctx context.Context, spec *ociimagecopy.SSHRemoteSpec) (ociimagecopy.Remote, error) {
	if err := validateSSHTarget(spec.Target); err != nil {
		return nil, err
	}
	if err := ssh.Probe(ctx, spec.Target); err != nil {
		return nil, fmt.Errorf("ssh probe: %w", err)
	}
	return remote.NewSSH(ctx, remote.SSHConfig{
		Target:    spec.Target,
		Transport: spec.Transport,
		OCIPath:   spec.OCIPath,
	})
}

// validateSSHTarget returns a clear error when the parsed SSH target is
// ambiguous or missing required fields.
func validateSSHTarget(t ssh.Target) error {
	if t.Host == "" {
		return fmt.Errorf("--remote ssh: host is required")
	}
	if t.Port < 0 {
		return fmt.Errorf("--remote ssh: port must be non-negative")
	}
	return nil
}

// hasAuthorizationHeader reports whether any header in hs has the name
// "Authorization" (case-insensitive per HTTP semantics).
// hs entries have the form "Name: value".
func hasAuthorizationHeader(hs []string) bool {
	for _, h := range hs {
		name, _, ok := strings.Cut(h, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "authorization") {
			return true
		}
	}
	return false
}
