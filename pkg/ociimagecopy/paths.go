package ociimagecopy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
)

// AppDirName is the per-user data subdirectory under XDG_DATA_HOME.
const AppDirName = "oci-image-copy"

// Store wraps a base data directory and exposes the on-disk layout used
// by both the local CLI and (mirrored over SFTP) the remote peer:
//
//	<base>/
//	  <host>/<repo-path>/_tags/<tag>/        # per-image oci: dump
//	  <host>/<repo-path>/_digests/<hex>/     # digest-pinned variant
//	  share/                                 # shared blob pool
type Store struct {
	Base string
}

// NewStore returns a Store rooted at base. Use [DefaultBaseDir] to derive
// the local default.
func NewStore(base string) *Store { return &Store{Base: base} }

// DefaultBaseDir returns ${XDG_DATA_HOME:-$HOME/.local/share}/oci-image-copy.
func DefaultBaseDir() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, AppDirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("store: cannot resolve $HOME: %w", err)
	}
	return filepath.Join(home, ".local", "share", AppDirName), nil
}

// ShareDir is the shared blob pool directory.
func (s *Store) ShareDir() string { return filepath.Join(s.Base, "share") }

// TagDir returns the tag-pinned dump directory for r. If r is not
// tag-pinned, it returns "".
func (s *Store) TagDir(r imageref.ImageRef) string {
	if !r.IsTagged() {
		return ""
	}
	return filepath.Join(s.Base, r.Host, filepath.FromSlash(r.Path), "_tags", r.Tag)
}

// DigestDir returns the digest-pinned dump directory for r. If r is not
// digest-pinned, it returns "".
func (s *Store) DigestDir(r imageref.ImageRef) string {
	if !r.IsDigested() {
		return ""
	}
	return filepath.Join(s.Base, r.Host, filepath.FromSlash(r.Path), "_digests", r.Digest)
}

// DumpDir returns whichever of TagDir / DigestDir is non-empty for r.
func (s *Store) DumpDir(r imageref.ImageRef) (string, error) {
	switch {
	case r.IsTagged():
		return s.TagDir(r), nil
	case r.IsDigested():
		return s.DigestDir(r), nil
	default:
		return "", errors.New("store: ref has neither tag nor digest")
	}
}

// EnsureLayout creates ShareDir if missing. Idempotent.
func (s *Store) EnsureLayout(ctx context.Context) error {
	for _, d := range []string{s.Base, s.ShareDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("store: mkdir %s: %w", d, err)
		}
	}
	return nil
}

// RelTagPath is the slash-form tag path **relative** to the base
// dir, suitable as input to a [vroot.Fs[vroot.File]] rooted at the base.
func RelTagPath(host, repoPath, tag string) string {
	return path.Join(host, repoPath, "_tags", tag)
}

// RelDigestPath is the slash-form digest path relative to base.
func RelDigestPath(host, repoPath, digestHex string) string {
	return path.Join(host, repoPath, "_digests", digestHex)
}

// RelSharePath is the relative-form share path ("share").
func RelSharePath() string { return "share" }

// RelDumpDir returns the relative-form dump dir for r (TagDir or
// DigestDir without the base prefix).
func RelDumpDir(r imageref.ImageRef) (string, error) {
	switch {
	case r.IsTagged():
		return RelTagPath(r.Host, r.Path, r.Tag), nil
	case r.IsDigested():
		return RelDigestPath(r.Host, r.Path, r.Digest), nil
	default:
		return "", errors.New("store: ref has neither tag nor digest")
	}
}

// RelBlobPath returns the share-relative path for digest:
// "share/<algo>/<hex>".
func RelBlobPath(digest string) (string, error) {
	algo, hex, err := ocidir.SplitDigest(digest)
	if err != nil {
		return "", err
	}
	return path.Join("share", algo, hex), nil
}
