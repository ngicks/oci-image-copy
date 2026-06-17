// Package fileserver implements the file-server Remote for oci-image-copy
// (Layer 2 of PLAN3). It wires the generic stream/fileserver client and
// chunked adapters into the OCI Remote / StoreV1 abstraction.
//
// Object naming is controlled by a [NamingConvention]; the default
// [DefaultNaming] produces:
//
//	meta:  <prefix>/images/<host>/<repo-path>/_tags/<tag>.json
//	chunk: <prefix>/blobs/<algo>/<hex>/<08d index>
//
// The chunk key carries the digest algorithm segment (<algo>, e.g. "sha256")
// so addresses cannot collide across algorithms (decision D13). This is a wire-
// format change accepted pre-1.0: it invalidates existing fileserver stores.
//
// The per-image metadata object is written LAST as the commit marker:
// a crash mid-push leaves at worst orphan chunks, never a metadata
// object referencing missing chunks.
package fileserver

import (
	"fmt"
	"strings"

	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/opencontainers/go-digest"
)

// NamingConvention translates OCI-level requests to object names on the
// file server. Implementations adapt existing bucket layouts.
type NamingConvention interface {
	// ImageMeta returns the object name of the per-image metadata for ref.
	ImageMeta(ref imageref.ImageRef) string

	// BlobChunk returns the object name of chunk index i of blob dgst.
	// Chunk 0 always exists even for single-chunk blobs so that existence
	// probing via Stat is uniform across all object sizes.
	BlobChunk(dgst digest.Digest, index int) string
}

// DefaultNaming is the default [NamingConvention].
//
// Object names:
//   - meta:  <Prefix>/images/<host>/<repo-path>/_tags/<tag>.json
//   - chunk: <Prefix>/blobs/<algo>/<hex>/<08d index>
//
// When Prefix is empty no leading slash or separator is added.
type DefaultNaming struct {
	// Prefix is an optional path prefix, typically the bucket name or a
	// sub-path within a bucket (e.g. "my-bucket" or "oci-images").
	// An empty Prefix produces names with no leading separator.
	Prefix string
}

// ImageMeta implements [NamingConvention].
//
// The tag is used verbatim; a digest-only ref returns a name under
// _digests/<hex>.json instead of _tags/<tag>.json.
// The host and repo-path segments are encoded as-is; no URL escaping is
// applied here — the file-server client escapes them at the transport layer.
func (n DefaultNaming) ImageMeta(ref imageref.ImageRef) string {
	var leafPart string
	if ref.IsTagged() {
		leafPart = "_tags/" + ref.Tag + ".json"
	} else {
		leafPart = "_digests/" + ref.Digest + ".json"
	}

	suffix := "images/" + ref.Host + "/" + ref.Path + "/" + leafPart
	return n.join(suffix)
}

// BlobChunk implements [NamingConvention].
//
// The name format is:
//
//	<prefix>/blobs/<algo>/<hex>/<08d index>
//
// The digest algorithm segment (e.g. "sha256") is included so that two blobs
// with the same hex under different algorithms cannot collide (decision D13).
func (n DefaultNaming) BlobChunk(dgst digest.Digest, index int) string {
	algo, hex := splitDigestParts(dgst)
	suffix := fmt.Sprintf("blobs/%s/%s/%08d", algo, hex, index)
	return n.join(suffix)
}

// join prepends n.Prefix with a "/" separator when Prefix is non-empty.
func (n DefaultNaming) join(suffix string) string {
	if n.Prefix == "" {
		return suffix
	}
	return strings.TrimRight(n.Prefix, "/") + "/" + suffix
}

// splitDigestParts returns the (algorithm, hex) parts of a digest.
// For unsupported algorithms it returns ("", dgst.Encoded()).
func splitDigestParts(dgst digest.Digest) (algo, hex string) {
	return string(dgst.Algorithm()), dgst.Encoded()
}
