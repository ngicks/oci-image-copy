// Package commands implements the cobra subcommands (push, pull, dump)
// for the oci-image-copy CLI binary.
package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/ngicks/oci-image-copy/pkg/cli/ssh"
	"github.com/ngicks/oci-image-copy/pkg/imagecopy"
	"github.com/spf13/cobra"
)

// FileServerAuthEnvVar is the environment variable that supplies a default
// Authorization header value for the file-server remote. The value is used
// verbatim as the header value (e.g. "Bearer <token>" or "Basic <base64>").
//
// An explicit --remote-header 'Authorization: ...' flag takes precedence.
// The value is never logged or included in error messages.
const FileServerAuthEnvVar = "OCI_IMAGE_COPY_FILESERVER_AUTH"

func Execute(ctx context.Context) error {
	return rootCmd.ExecuteContext(ctx)
}

var rootCmd = &cobra.Command{
	Use:           "oci-image-copy",
	Short:         "Share OCI images between two hosts efficiently over SSH using skopeo + sftp.",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.NoArgs,
	RunE:          runRoot,
}

func runRoot(cmd *cobra.Command, args []string) error {
	return cmd.Help()
}

// fileServerOpts holds the companion flags for a --remote file-server:...
// spec. They supplement the URL parsed from the spec.
type fileServerOpts struct {
	headers      []string
	chunkSize    string // human-readable (parsed by imagecopy.ParseChunkSize)
	namingPrefix string
	// authFromEnv is the value of OCI_IMAGE_COPY_FILESERVER_AUTH, supplied by
	// the caller rather than read here to keep the logic testable without
	// setenv. It is added as "Authorization: <value>" only when no explicit
	// Authorization header is present in headers (case-insensitive).
	// The value must never appear in logs or error messages.
	authFromEnv string
}

// initShare parses --local and --remote specs, builds a [*imagecopy.Local]
// and a [imagecopy.Remote], and wraps them in a [*imagecopy.Share].
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
) (*imagecopy.Share, error) {
	ls, err := imagecopy.ParseLocalSpec(localSpec)
	if err != nil {
		return nil, err
	}

	local, err := imagecopy.NewLocal(ctx, imagecopy.LocalConfig{
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

	return imagecopy.NewShare(local, remote), nil
}

// buildRemote parses a --remote spec and constructs the appropriate Remote.
// fsOpts supplements a file-server spec with companion flag values.
func buildRemote(
	ctx context.Context,
	remoteSpec string,
	fsOpts fileServerOpts,
) (imagecopy.Remote, error) {
	rs, err := imagecopy.ParseRemoteSpec(remoteSpec)
	if err != nil {
		return nil, err
	}

	switch rs.Kind {
	case imagecopy.RemoteKindSSH:
		return buildSSHRemote(ctx, rs.SSH)
	case imagecopy.RemoteKindLocalDir:
		return imagecopy.NewLocalDirRemote(rs.LocalDir.Path)
	case imagecopy.RemoteKindFileServer:
		spec := rs.FileServer
		// Merge companion flag values into the spec.
		if len(fsOpts.headers) > 0 {
			spec.Headers = fsOpts.headers
		}
		if fsOpts.chunkSize != "" {
			cs, err := imagecopy.ParseChunkSize(fsOpts.chunkSize)
			if err != nil {
				return nil, fmt.Errorf("--remote-chunk-size: %w", err)
			}
			spec.ChunkSize = cs
		}
		if fsOpts.namingPrefix != "" {
			spec.NamingPrefix = fsOpts.namingPrefix
		}
		// Apply env-var auth only when no explicit Authorization header was
		// supplied via --remote-header (flag wins; comparison is case-insensitive
		// per HTTP semantics).
		if fsOpts.authFromEnv != "" && !hasAuthorizationHeader(spec.Headers) {
			spec.Headers = append(spec.Headers, "Authorization: "+fsOpts.authFromEnv)
		}
		return imagecopy.NewFileServerRemoteFromSpec(spec)
	default:
		return nil, fmt.Errorf("internal: unknown remote kind %v", rs.Kind)
	}
}

// validateSourceLocal rejects --local specs that cannot act as a push/dump
// source. All transports including docker: are valid sources.
func validateSourceLocal(flag string, ls imagecopy.LocalSpec) error {
	// All supported local transports are valid as push/dump sources.
	// ParseLocalSpec already rejects unknown transports, so no extra work needed.
	_ = flag
	_ = ls
	return nil
}

// validateEnumerableLocal rejects --local specs that cannot be enumerated
// or loaded into. docker: transport is a push/dump source only.
func validateEnumerableLocal(flag string, ls imagecopy.LocalSpec) error {
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
func buildSSHRemote(ctx context.Context, spec *imagecopy.SSHRemoteSpec) (imagecopy.Remote, error) {
	if err := validateSSHTarget(spec.Target); err != nil {
		return nil, err
	}
	if err := ssh.Probe(ctx, spec.Target); err != nil {
		return nil, fmt.Errorf("ssh probe: %w", err)
	}
	return imagecopy.NewRemote(ctx, imagecopy.RemoteConfig{
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
