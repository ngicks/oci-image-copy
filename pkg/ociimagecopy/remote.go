package ociimagecopy

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/opencontainers/go-digest"
)

// ErrReadOnly is returned by [Remote.LoadImage], [Remote.DumpImage], and the
// write side of [BlobStore] / [TagStoreV1] when the peer is read-only.
var ErrReadOnly = errors.New("remote: read-only")

// Remote is an OCI store the orchestrator can read from and (when not
// read-only) write to. The concrete implementations live in package remote
// (the SSH+SFTP-backed remote, the local-directory remote, and the HTTP
// file-server remote); custom transports (S3, an HTTP mirror, an in-memory
// test double) plug in by implementing this interface.
//
// Read-only implementations return true from [Remote.ReadOnly].
// Mutating operations on read-only peers return [ErrReadOnly].
type Remote interface {
	// Close releases any subsystem resources (e.g. the ssh+sftp
	// subprocess for the SSH-backed remote). Safe to call multiple times.
	Close() error

	// ReadOnly reports whether mutating operations targeting this peer
	// should be rejected.
	ReadOnly() bool

	// Blobs returns the content-addressed blob pool ([BlobStore]) this
	// Remote backs.
	Blobs() BlobStore

	// Tags returns the per-tag pointer-file store ([TagStoreV1]) this
	// Remote backs.
	Tags() TagStoreV1

	// ListBlobsByImage enumerates the content-addressed blobs in ref's image
	// closure as the peer knows them (the fileserver: meta.Descriptors, one
	// read; fs remotes: the manifest closure). Cost is closure-sized, not
	// store-sized. An absent image yields nothing (no error); only real
	// transport/auth/server errors are yielded. Order is unspecified.
	ListBlobsByImage(ctx context.Context, ref imageref.ImageRef) iter.Seq2[digest.Digest, error]

	// LoadImage tells the peer to load ref's content from its OCI
	// mirror into its live storage (containers-storage / docker-
	// daemon / etc.). Returns [ErrReadOnly] when the peer is read-
	// only; returns nil (no-op) when the peer has no live storage to
	// load into (e.g., a pure OCI mirror).
	LoadImage(ctx context.Context, ref imageref.ImageRef) error

	// DumpImage materializes ref from the peer's live storage into the
	// peer's content-addressable store (the mirror), so that the per-ref
	// view via [NewImageView] over Tags()/Blobs() and the blob set behind it
	// become readable. It is the inverse of LoadImage.
	//
	//   - Implementations backed by a live storage (containers-storage /
	//     docker-daemon over SSH) run the equivalent of
	//     `skopeo copy <transport>:<ref> oci:<tagDir>` with the shared
	//     blob pool on the peer.
	//   - Implementations whose store IS the live storage (pure oci:
	//     mirrors, S3-like stores) return nil without doing anything.
	//   - Read-only peers return [ErrReadOnly] (the pull orchestrator
	//     treats this as advisory: it logs and proceeds to the mirror
	//     read, which gives the definitive error if the content is
	//     genuinely absent).
	//
	// DumpImage is idempotent per (ref, content): re-dumping an
	// unchanged tag is cheap because skopeo skips blobs already present
	// in the shared pool. It is called for every pulled ref even when
	// the mirror already has the tag, because tags move; the live
	// storage is the source of truth on the peer.
	DumpImage(ctx context.Context, ref imageref.ImageRef) error

	// InspectImage returns the raw manifest bytes for ref as known to
	// the peer's source of truth, without mutating anything. Used by
	// pull --dry-run to compute the transfer plan without dumping on
	// the peer.
	//
	//   - Live-storage implementations run
	//     `skopeo inspect --raw <transport>:<ref>` on the peer.
	//   - Mirror-only implementations read the manifest from the store
	//     directly.
	InspectImage(ctx context.Context, ref imageref.ImageRef) ([]byte, error)
}

// ListImagesFromFs walks fs for <host>/<repo>/_tags/<tag> and
// _digests/<hex> dump dirs and yields the parsed [imageref.ImageRef]. It is the
// shared image-enumeration helper for the FS-backed [Remote] implementations
// (the SSH and local-directory remotes in package remote).
func ListImagesFromFs(
	ctx context.Context,
	fsys vroot.Fs[vroot.File],
) iter.Seq2[imageref.ImageRef, error] {
	return func(yield func(imageref.ImageRef, error) bool) {
		dumps, err := walkDumpDirs(fsys, ".")
		if err != nil {
			yield(imageref.ImageRef{}, err)
			return
		}
		for _, d := range dumps {
			if err := ctx.Err(); err != nil {
				yield(imageref.ImageRef{}, err)
				return
			}
			ref, err := parseDumpDirRel(d)
			if err != nil {
				if !yield(imageref.ImageRef{}, fmt.Errorf("parse %q: %w", d, err)) {
					return
				}
				continue
			}
			if !yield(ref, nil) {
				return
			}
		}
	}
}

// parseDumpDirRel parses an FS-relative dump-dir path
// `<host>/<repo>/_tags/<tag>` or `<host>/<repo>/_digests/<hex>` into
// the corresponding [imageref.ImageRef].
func parseDumpDirRel(rel string) (imageref.ImageRef, error) {
	if marker, leaf, ok := splitOn(rel, "/_tags/"); ok {
		host, repoPath, ok := strings.Cut(marker, "/")
		if !ok || host == "" || repoPath == "" {
			return imageref.ImageRef{}, fmt.Errorf("missing host/path in %q", rel)
		}
		// Defense-in-depth: a peer-controlled dump-dir name must satisfy the
		// same host/path/tag rules Parse enforces (no `..`, no slash-in-tag).
		if err := imageref.ValidateHostPathTag(host, repoPath, leaf); err != nil {
			return imageref.ImageRef{}, fmt.Errorf("invalid dump dir %q: %w", rel, err)
		}
		ref := imageref.ImageRef{Host: host, Path: repoPath, Tag: leaf}
		ref.Original = ref.String()
		return ref, nil
	}
	if marker, leaf, ok := splitOn(rel, "/_digests/"); ok {
		host, repoPath, ok := strings.Cut(marker, "/")
		if !ok || host == "" || repoPath == "" {
			return imageref.ImageRef{}, fmt.Errorf("missing host/path in %q", rel)
		}
		// Validate host + path (tag empty for a digest ref); the digest hex
		// itself is validated below.
		if err := imageref.ValidateHostPathTag(host, repoPath, ""); err != nil {
			return imageref.ImageRef{}, fmt.Errorf("invalid dump dir %q: %w", rel, err)
		}
		if err := imageref.ValidateDigestHex(leaf); err != nil {
			return imageref.ImageRef{}, fmt.Errorf("invalid dump dir %q: %w", rel, err)
		}
		ref := imageref.ImageRef{Host: host, Path: repoPath, Digest: leaf}
		ref.Original = ref.String()
		return ref, nil
	}
	return imageref.ImageRef{}, fmt.Errorf("path has no _tags/_digests marker")
}

// splitOn splits s at sep, returning the (before, after, ok) triple.
// Like [strings.Cut] but for an arbitrary separator.
func splitOn(s, sep string) (before, after string, ok bool) {
	before, after, ok = strings.Cut(s, sep)
	if !ok {
		return "", "", false
	}
	return before, after, true
}
