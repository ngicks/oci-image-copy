// Package imageref parses and canonicalizes OCI / Docker image
// references of the form `[host[:port]/]<repo-path>[:tag|@digest]`.
//
// The package is intentionally small and standalone — it has no
// dependency on the rest of oci-image-copy so it can be reused by
// other tooling.
package imageref

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// DefaultRegistry is the host inferred for refs that do not specify one
	// (docker-style "nginx:latest" or "library/nginx:latest").
	DefaultRegistry = "docker.io"
	// DockerLibraryNamespace is the namespace prepended to single-segment
	// Docker Hub refs ("nginx" -> "library/nginx").
	DockerLibraryNamespace = "library"

	// DigestAlgorithm is the only digest algorithm we support.
	DigestAlgorithm = "sha256"
	// DigestPrefix is the leading prefix on a fully-qualified digest string.
	DigestPrefix = DigestAlgorithm + ":"
	// DigestHexLen is the expected length of a sha256 hex digest.
	DigestHexLen = 64
)

// ReservedSegments is the set of path segments reserved by the on-disk
// layout. Refs whose repository path contains any of these segments are
// rejected at parse time.
var ReservedSegments = map[string]struct{}{
	"_tags":    {},
	"_digests": {},
}

// ImageRef is a parsed [host[:port]/]<repo-path>[:tag|@digest] image
// reference. Either Tag or Digest is set, never both. Digest is the
// hex portion only (no "sha256:" prefix).
type ImageRef struct {
	Host     string // e.g. "docker.io", "registry.example.com:5000"
	Path     string // slash-separated, no leading/trailing slash
	Tag      string // mutually exclusive with Digest
	Digest   string // hex-only, no "sha256:" prefix; mutually exclusive with Tag
	Original string // verbatim input, kept for diagnostics
}

// IsTagged reports whether the ref is pinned by tag.
func (r ImageRef) IsTagged() bool { return r.Tag != "" }

// IsDigested reports whether the ref is pinned by digest.
func (r ImageRef) IsDigested() bool { return r.Digest != "" }

// String returns the canonical reference (host/path[:tag|@sha256:digest]).
func (r ImageRef) String() string {
	if r.IsDigested() {
		return r.Host + "/" + r.Path + "@" + DigestPrefix + r.Digest
	}
	return r.Host + "/" + r.Path + ":" + r.Tag
}

// Parse parses s into an [ImageRef], applying Docker Hub
// canonicalization and rejecting refs whose path contains a reserved
// segment.
func Parse(s string) (ImageRef, error) {
	if s == "" {
		return ImageRef{}, errors.New("imageref: empty reference")
	}
	original := s

	var digest, tag string

	// Digest first: '@sha256:<hex>' is unambiguous since '@' cannot appear
	// in host or path.
	if at := strings.LastIndex(s, "@"); at >= 0 {
		dpart := s[at+1:]
		if !strings.HasPrefix(dpart, DigestPrefix) {
			return ImageRef{}, fmt.Errorf(
				"imageref: digest %q must start with %q",
				dpart,
				DigestPrefix,
			)
		}
		hex := dpart[len(DigestPrefix):]
		if err := validateHex(hex); err != nil {
			return ImageRef{}, fmt.Errorf("imageref: invalid digest hex: %w", err)
		}
		digest = hex
		s = s[:at]
	}

	// Tag is anything after the last ':' that comes after the last '/'.
	// (':' before the last '/' is a port on the host.)
	if digest == "" {
		lastSlash := strings.LastIndex(s, "/")
		colonInLastSeg := strings.IndexByte(s[lastSlash+1:], ':')
		if colonInLastSeg >= 0 {
			pos := lastSlash + 1 + colonInLastSeg
			tag = s[pos+1:]
			if tag == "" {
				return ImageRef{}, errors.New("imageref: empty tag after ':'")
			}
			s = s[:pos]
		}
	}

	if s == "" {
		return ImageRef{}, errors.New("imageref: missing repository path")
	}

	parts := strings.Split(s, "/")
	var host, path string
	if len(parts) > 1 && looksLikeHost(parts[0]) {
		host = parts[0]
		path = strings.Join(parts[1:], "/")
	} else {
		host = DefaultRegistry
		path = strings.Join(parts, "/")
	}

	if path == "" {
		return ImageRef{}, errors.New("imageref: missing repository path")
	}

	// Docker Hub canonicalization: a single-segment path under docker.io is
	// implicitly under the "library" namespace.
	if host == DefaultRegistry {
		segs := strings.Split(path, "/")
		if len(segs) == 1 {
			path = DockerLibraryNamespace + "/" + path
		}
	}

	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" {
			return ImageRef{}, fmt.Errorf("imageref: empty segment in path %q", path)
		}
		if _, bad := ReservedSegments[seg]; bad {
			return ImageRef{}, fmt.Errorf(
				"imageref: path %q contains reserved segment %q",
				path, seg,
			)
		}
	}

	if digest == "" && tag == "" {
		tag = "latest"
	}

	return ImageRef{
		Host:     host,
		Path:     path,
		Tag:      tag,
		Digest:   digest,
		Original: original,
	}, nil
}

// looksLikeHost matches the classical Docker rule: the first slash-separated
// segment is treated as a host iff it contains "." or ":", or is exactly
// "localhost".
func looksLikeHost(s string) bool {
	return s == "localhost" ||
		strings.ContainsAny(s, ".:")
}

func validateHex(s string) error {
	if len(s) != DigestHexLen {
		return fmt.Errorf("expected %d hex chars, got %d", DigestHexLen, len(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case '0' <= c && c <= '9':
		case 'a' <= c && c <= 'f':
		default:
			return fmt.Errorf("non-hex char %q at index %d", c, i)
		}
	}
	return nil
}
