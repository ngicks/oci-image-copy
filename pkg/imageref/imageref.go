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

	// Lowercase the host before the DefaultRegistry comparison so that
	// "DOCKER.IO/nginx" canonicalizes the same as "docker.io/nginx"
	// (registry hosts are case-insensitive DNS names).
	host = strings.ToLower(host)
	if err := validateHost(host); err != nil {
		return ImageRef{}, err
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

	if err := validatePath(path); err != nil {
		return ImageRef{}, err
	}

	if digest == "" && tag == "" {
		tag = "latest"
	}
	if tag != "" {
		if err := validateTag(tag); err != nil {
			return ImageRef{}, err
		}
	}

	return ImageRef{
		Host:     host,
		Path:     path,
		Tag:      tag,
		Digest:   digest,
		Original: original,
	}, nil
}

// ValidateHostPathTag validates a host / repository-path / tag triple against
// the same rules [Parse] applies to its components. tag may be empty (for a
// digest-pinned ref). It is the shared validation used by both the write side
// ([Parse]) and the read side (dump-dir reconstruction) so a maliciously-named
// directory on a peer cannot smuggle a `..` host/segment or a slash-bearing tag
// back into an ImageRef.
func ValidateHostPathTag(host, path, tag string) error {
	if err := validateHost(host); err != nil {
		return err
	}
	if path == "" {
		return errors.New("imageref: missing repository path")
	}
	if err := validatePath(path); err != nil {
		return err
	}
	if tag != "" {
		if err := validateTag(tag); err != nil {
			return err
		}
	}
	return nil
}

// validatePath validates a slash-separated repository path: every segment
// must be non-empty, must not be a reserved on-disk segment, must not be the
// path-traversal segments "." or "..", and must contain no separators or
// control characters. These checks are defense-in-depth for the on-disk path
// layout, where Host/Path/Tag flow into filepath.Join under the dump base.
func validatePath(path string) error {
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" {
			return fmt.Errorf("imageref: empty segment in path %q", path)
		}
		if _, bad := ReservedSegments[seg]; bad {
			return fmt.Errorf(
				"imageref: path %q contains reserved segment %q",
				path, seg,
			)
		}
		if err := validatePathSegment(seg); err != nil {
			return fmt.Errorf("imageref: path %q: %w", path, err)
		}
	}
	return nil
}

// validatePathSegment rejects the path-traversal segments and any segment
// containing a path separator, backslash, or control character.
func validatePathSegment(seg string) error {
	switch seg {
	case ".", "..":
		return fmt.Errorf("segment %q is not allowed (path traversal)", seg)
	}
	for _, r := range seg {
		switch {
		case r == '/' || r == '\\':
			return fmt.Errorf("segment %q contains a path separator", seg)
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("segment %q contains a control character", seg)
		}
	}
	return nil
}

// validateTag enforces the OCI/Docker tag grammar:
// [A-Za-z0-9_][A-Za-z0-9._-]{0,127}. This rejects slashes (which would split
// the tag across dump-dir levels), leading separators, control characters, and
// overlong tags.
func validateTag(tag string) error {
	if len(tag) == 0 {
		return errors.New("imageref: empty tag")
	}
	if len(tag) > 128 {
		return fmt.Errorf("imageref: tag %q too long (max 128 chars)", tag)
	}
	if c := tag[0]; !isAlphaNum(c) && c != '_' {
		return fmt.Errorf(
			"imageref: tag %q must start with [A-Za-z0-9_]", tag,
		)
	}
	for i := 1; i < len(tag); i++ {
		c := tag[i]
		if !isAlphaNum(c) && c != '_' && c != '.' && c != '-' {
			return fmt.Errorf(
				"imageref: tag %q contains invalid character %q", tag, c,
			)
		}
	}
	return nil
}

// validateHost validates the shape of a registry host. It must be non-empty,
// contain no path separators or control characters, and not be a traversal
// token. (Full RFC-1123 hostname validation is intentionally out of scope:
// the goal is to keep the host from escaping the dump-base path layout, e.g.
// rejecting ".." which looksLikeHost would otherwise accept because it
// contains '.'.)
func validateHost(host string) error {
	if host == "" {
		return errors.New("imageref: empty host")
	}
	switch host {
	case ".", "..":
		return fmt.Errorf("imageref: host %q is not allowed (path traversal)", host)
	}
	for _, r := range host {
		switch {
		case r == '/' || r == '\\':
			return fmt.Errorf("imageref: host %q contains a path separator", host)
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("imageref: host %q contains a control character", host)
		}
	}
	return nil
}

func isAlphaNum(c byte) bool {
	return ('0' <= c && c <= '9') ||
		('a' <= c && c <= 'z') ||
		('A' <= c && c <= 'Z')
}

// looksLikeHost matches the classical Docker rule: the first slash-separated
// segment is treated as a host iff it contains "." or ":", or is exactly
// "localhost".
func looksLikeHost(s string) bool {
	return s == "localhost" ||
		strings.ContainsAny(s, ".:")
}

// ValidateDigestHex checks that s is a [DigestHexLen]-char lowercase sha256
// hex string. It is the shared digest-hex check used by [Parse] (the @sha256:
// suffix) and by dump-dir reconstruction (the _digests/<hex> leaf).
func ValidateDigestHex(s string) error {
	return validateHex(s)
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
