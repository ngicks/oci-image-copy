package ociimagecopy

// spec.go parses the URI-style --local and --remote flag values introduced
// in the CLI flag refactor (Goal 3 / Plan 3).
//
// Grammar (canonical forms):
//
//	--local <local-spec>
//	  containers-storage:          (bare name also accepted: containers-storage)
//	  docker-daemon:               (bare: docker-daemon)
//	  oci:/path/to/dir
//	  docker:                      (push/dump source only; bare: docker)
//
//	--remote <remote-spec>
//	  ssh://[user@]host[:port][/<transport-spec>]
//	      transport-spec ::= <transport>[:<arg>]
//	      empty transport-spec defaults to containers-storage:
//	      Examples:
//	        ssh://host/containers-storage:
//	        ssh://user@host:2222/oci:/srv/oci
//	        ssh://host/docker-daemon:
//	  file-server:<url>            (fully implemented; see remote.NewFileServerFromSpec)
//	  oci:/path/to/base/dir        (local-directory remote)

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
	"github.com/ngicks/oci-image-copy/pkg/cli/ssh"
)

// LocalSpec holds the parsed components of a --local flag value.
type LocalSpec struct {
	// Transport is one of the supported local transports.
	Transport skopeo.Transport
	// Path is the OCI layout dir (only for TransportOci).
	Path string
}

// ParseLocalSpec parses a --local flag value into a [LocalSpec].
//
// Accepted forms (canonical form has the trailing colon; bare form also works):
//
//	containers-storage:    (or: containers-storage)
//	docker-daemon:         (or: docker-daemon)
//	oci:/path/to/dir
//	docker:                (or: docker) — push/dump source only; pull validation rejects it
func ParseLocalSpec(s string) (LocalSpec, error) {
	if s == "" {
		return LocalSpec{}, errors.New("--local: empty spec")
	}

	// oci: requires a path argument.
	if p, ok := strings.CutPrefix(s, "oci:"); ok {
		if p == "" {
			return LocalSpec{}, errors.New(
				"--local oci: requires a path (e.g. oci:/path/to/dir)",
			)
		}
		return LocalSpec{Transport: skopeo.TransportOci, Path: p}, nil
	}

	// docker: prefix (or bare "docker")
	if s == "docker" || s == "docker:" {
		return LocalSpec{Transport: skopeo.TransportDocker}, nil
	}

	// containers-storage: (with or without trailing colon)
	if s == "containers-storage" || s == "containers-storage:" {
		return LocalSpec{Transport: skopeo.TransportContainersStorage}, nil
	}

	// docker-daemon: (with or without trailing colon)
	if s == "docker-daemon" || s == "docker-daemon:" {
		return LocalSpec{Transport: skopeo.TransportDockerDaemon}, nil
	}

	// Unknown spec.
	return LocalSpec{}, fmt.Errorf(
		"--local: unrecognised spec %q "+
			"(accepted forms: containers-storage:, docker-daemon:, oci:/path, docker:)",
		s,
	)
}

// RemoteKind identifies which variant of --remote spec was parsed.
type RemoteKind int

const (
	RemoteKindSSH        RemoteKind = iota // ssh://[user@]host[:port][/transport-spec]
	RemoteKindFileServer                   // file-server:<url>  (fully implemented)
	RemoteKindLocalDir                     // oci:/path/to/base/dir
)

// SSHRemoteSpec holds the parsed SSH remote components.
type SSHRemoteSpec struct {
	// Target is the SSH connection target (host/user/port or Name).
	Target ssh.Target
	// Transport is the skopeo transport on the remote side.
	Transport skopeo.Transport
	// OCIPath is the remote oci: dir path (only for TransportOci).
	OCIPath string
}

// FileServerRemoteSpec holds the parsed file-server remote components.
// The factory remote.NewFileServerFromSpec constructs a fully functional
// file-server Remote from this spec, wiring the HTTP client, headers, naming
// convention, and chunk size.
type FileServerRemoteSpec struct {
	// URL is the base URL of the file server. Userinfo and query string
	// are redacted in all log/error output via RedactFileServerURL.
	URL *url.URL
	// Headers is the list of additional request headers supplied via
	// --remote-header flags (repeatable). Values are redacted in logs.
	Headers []string
	// ChunkSize is the upload chunk size in bytes (default DefaultChunkSize).
	ChunkSize int64
	// NamingPrefix is the DefaultNaming prefix (default "").
	NamingPrefix string
}

// DefaultChunkSize is the default file-server chunk size (100 MiB).
const DefaultChunkSize int64 = 100 * 1024 * 1024

// LocalDirRemoteSpec holds the parsed local-directory remote components.
type LocalDirRemoteSpec struct {
	// Path is the absolute path to the local OCI store base directory.
	Path string
}

// RemoteSpec is the sum type returned by [ParseRemoteSpec].
type RemoteSpec struct {
	Kind RemoteKind

	// SSH is set when Kind == RemoteKindSSH.
	SSH *SSHRemoteSpec
	// FileServer is set when Kind == RemoteKindFileServer.
	FileServer *FileServerRemoteSpec
	// LocalDir is set when Kind == RemoteKindLocalDir.
	LocalDir *LocalDirRemoteSpec
}

// ParseRemoteSpec parses a --remote flag value into a [RemoteSpec].
//
// Accepted forms:
//
//	ssh://[user@]host[:port][/<transport-spec>]
//	    <transport-spec> ::= <transport>[:<arg>]
//	    Empty path defaults to containers-storage:.
//	    Examples:
//	      ssh://host/containers-storage:
//	      ssh://user@host:2222/oci:/srv/oci
//	      ssh://host/docker-daemon:
//	      ssh://host                          → containers-storage: default
//
//	file-server:<url>
//	    Fully implemented. remote.NewFileServerFromSpec constructs the Remote.
//	    Companion flags (merged into the spec by the CLI layer):
//	      --remote-header 'Name: value'  (repeatable; values redacted in logs)
//	      --remote-chunk-size            (default 100 MiB)
//	      --remote-naming-prefix         (default "")
//	    Auth env var: OCI_IMAGE_COPY_FILESERVER_AUTH — sets the Authorization
//	    header when no explicit --remote-header Authorization is supplied.
//
//	oci:/path/to/base/dir
//	    Local-directory remote (no SSH). Construct an OCI store over an
//	    OS-backed directory for uniform blob reprocessing without a network hop.
func ParseRemoteSpec(s string) (RemoteSpec, error) {
	if s == "" {
		return RemoteSpec{}, errors.New("--remote: empty spec")
	}

	// oci:/path — local-directory remote
	if p, ok := strings.CutPrefix(s, "oci:"); ok {
		if p == "" {
			return RemoteSpec{}, errors.New(
				"--remote oci: requires a path (e.g. oci:/path/to/dir)",
			)
		}
		return RemoteSpec{
			Kind:     RemoteKindLocalDir,
			LocalDir: &LocalDirRemoteSpec{Path: p},
		}, nil
	}

	// file-server:<url> — fully implemented
	if rawURL, ok := strings.CutPrefix(s, "file-server:"); ok {
		if rawURL == "" {
			return RemoteSpec{}, errors.New(
				"--remote file-server: requires a URL (e.g. file-server:https://host/bucket/prefix)",
			)
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			// Redact credentials/query before embedding URL in error.
			redacted := redactRawURL(rawURL)
			return RemoteSpec{}, fmt.Errorf(
				"--remote file-server: invalid URL %q: parse error", redacted,
			)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return RemoteSpec{}, fmt.Errorf(
				"--remote file-server: URL scheme must be http or https, got %q", u.Scheme,
			)
		}
		return RemoteSpec{
			Kind: RemoteKindFileServer,
			FileServer: &FileServerRemoteSpec{
				URL:       u,
				ChunkSize: DefaultChunkSize,
			},
		}, nil
	}

	// ssh://... — SSH remote
	if strings.HasPrefix(s, "ssh://") {
		return parseSSHRemoteSpec(s)
	}

	return RemoteSpec{}, fmt.Errorf(
		"--remote: unrecognised spec %q; accepted forms: "+
			"ssh://[user@]host[:port][/<transport-spec>], "+
			"file-server:<url>, oci:/path",
		s,
	)
}

// parseSSHRemoteSpec parses the ssh:// variant of a remote spec.
//
// URL structure:  ssh://[user@]host[:port][/<transport>[:<arg>]]
//
// The path component (leading "/" stripped) is the nested transport spec:
//
//   - empty / "/" → containers-storage: (default)
//   - "containers-storage:" → containers-storage transport, no arg
//   - "docker-daemon:"     → docker-daemon transport, no arg
//   - "oci:/srv/oci"       → oci transport, arg = /srv/oci (absolute path)
//
// Because the arg for oci: is an absolute path that starts with "/", the
// raw URL path looks like "//srv/oci" (after the transport-spec slash).
// We handle this by splitting the stripped path on the FIRST ":" only.
func parseSSHRemoteSpec(s string) (RemoteSpec, error) {
	u, err := url.Parse(s)
	if err != nil {
		return RemoteSpec{}, fmt.Errorf("--remote ssh: invalid URL %q: %w", s, err)
	}
	if u.Scheme != "ssh" {
		return RemoteSpec{}, fmt.Errorf("--remote ssh: scheme must be ssh, got %q", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return RemoteSpec{}, errors.New("--remote ssh: host is required")
	}

	var user string
	if u.User != nil {
		user = u.User.Username()
	}

	portStr := u.Port()
	var port int
	if portStr != "" {
		if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil || port < 0 {
			return RemoteSpec{}, fmt.Errorf("--remote ssh: invalid port %q", portStr)
		}
	}

	// Build ssh.Target. Use Name when there is only a bare hostname with no
	// user/port, so that ssh-config Host aliases resolve naturally.
	target := ssh.Target{
		User: user,
		Host: host,
		Port: port,
	}

	// Parse the nested transport spec from the URL path.
	// u.Path is already unescaped; it starts with "/" when present.
	rawPath := strings.TrimPrefix(u.Path, "/")

	transport, ociPath, err := parseTransportSpec(rawPath)
	if err != nil {
		return RemoteSpec{}, fmt.Errorf("--remote ssh: transport spec: %w", err)
	}

	return RemoteSpec{
		Kind: RemoteKindSSH,
		SSH: &SSHRemoteSpec{
			Target:    target,
			Transport: transport,
			OCIPath:   ociPath,
		},
	}, nil
}

// parseTransportSpec splits "transport[:<arg>]" at the first ":" and returns
// the transport and arg. An empty spec defaults to containers-storage:.
//
// The arg for oci: is an absolute path; because the URL path component
// starts with "/" and oci: uses the form "oci:/srv/oci", after stripping
// the leading slash from the URL path we have "oci:/srv/oci" — split at the
// first ":" gives transport="oci", arg="/srv/oci".
func parseTransportSpec(spec string) (skopeo.Transport, string, error) {
	if spec == "" {
		return skopeo.TransportContainersStorage, "", nil
	}

	// Split at FIRST ":" only.
	transport, arg, hasSep := strings.Cut(spec, ":")
	if !hasSep {
		// Bare transport name without colon — accepted as well.
		transport = spec
		arg = ""
	}

	switch transport {
	case "containers-storage":
		if arg != "" {
			return "", "", fmt.Errorf(
				"transport %q takes no argument, got %q", transport, arg,
			)
		}
		return skopeo.TransportContainersStorage, "", nil

	case "docker-daemon":
		if arg != "" {
			return "", "", fmt.Errorf(
				"transport %q takes no argument, got %q", transport, arg,
			)
		}
		return skopeo.TransportDockerDaemon, "", nil

	case "oci":
		// The URL parser may have stripped a leading slash from the path
		// when handling "oci://…" — but we're working with the raw text here.
		// The canonical form is "oci:/srv/oci", so arg == "/srv/oci" after Cut.
		// However, if the original URL path was "/oci:/srv/oci" the url.Parse
		// result for Path is "/oci:/srv/oci" and after stripping the leading
		// slash we have "oci:/srv/oci". Splitting at ":" gives arg="/srv/oci".
		// Accept both absolute and relative paths.
		if arg == "" {
			return "", "", errors.New("transport oci: requires a path argument")
		}
		return skopeo.TransportOci, arg, nil

	default:
		return "", "", fmt.Errorf(
			"unsupported transport %q in ssh remote spec "+
				"(want one of: containers-storage:, docker-daemon:, oci:/path)",
			transport,
		)
	}
}

// ParseChunkSize parses a human-readable chunk size string (e.g. "100MiB",
// "50MB", "1GiB") into bytes. Accepts common binary (MiB, GiB, KiB) and
// decimal (MB, GB, KB) suffixes. Returns an error for zero or negative values.
func ParseChunkSize(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("chunk size is empty")
	}
	// Multipliers for binary (IEC) and decimal (SI) suffixes.
	suffixes := []struct {
		suffix string
		mult   int64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
		{"B", 1},
	}
	for _, sf := range suffixes {
		after, ok := strings.CutSuffix(s, sf.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(after, 10, 64)
		if err != nil {
			return 0, fmt.Errorf(
				"chunk size %q: invalid number before suffix %q: %w", s, sf.suffix, err,
			)
		}
		if n <= 0 {
			return 0, fmt.Errorf("chunk size %q: must be > 0", s)
		}
		return n * sf.mult, nil
	}
	// No recognized suffix: try raw integer (full string must parse cleanly).
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("chunk size %q: unrecognised format", s)
	}
	if n <= 0 {
		return 0, fmt.Errorf("chunk size %q: must be > 0", s)
	}
	return n, nil
}

// redactRawURL performs best-effort URL redaction for a raw string that
// may not parse cleanly. Strips userinfo (everything before last '@' in
// the authority) and query string (everything after '?').
func redactRawURL(rawURL string) string {
	// Try proper parse first.
	if u, err := url.Parse(rawURL); err == nil {
		return RedactFileServerURL(u)
	}
	// Fallback: strip query.
	if idx := strings.IndexByte(rawURL, '?'); idx >= 0 {
		rawURL = rawURL[:idx]
	}
	// Strip userinfo: find "://" then find "@" before next "/"
	if i := strings.Index(rawURL, "://"); i >= 0 {
		rest := rawURL[i+3:]
		if j := strings.IndexAny(rest, "/@"); j >= 0 && rest[j] == '@' {
			rawURL = rawURL[:i+3] + rest[j+1:]
		}
	}
	return rawURL
}

// RedactFileServerURL returns a copy of u with userinfo and query string
// removed, for use in log messages and error strings.
func RedactFileServerURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	redacted := *u
	redacted.User = nil
	redacted.RawQuery = ""
	redacted.Fragment = ""
	return redacted.String()
}

// RedactHeader returns the header name only, dropping the value.
// "Authorization: Bearer secret" → "Authorization: [redacted]"
func RedactHeader(header string) string {
	name, _, ok := strings.Cut(header, ":")
	if !ok {
		return "[redacted header]"
	}
	return strings.TrimSpace(name) + ": [redacted]"
}
