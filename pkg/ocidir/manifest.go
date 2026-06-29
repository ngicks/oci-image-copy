// Package ocidir parses on-disk OCI image-layout dumps. It exposes:
//
//   - [ParseManifest] / [ParseIndex] for raw JSON blobs;
//   - [ReadManifest] / [BlobReader] for walking a dump dir plus its
//     shared blob pool down to a manifest descriptor + body;
//   - [AllDescriptors] for flattening the resulting (manifest, config,
//     layers) into one slice;
//   - [DigestBytes] / [SplitDigest] as small digest helpers.
//
// The package only knows about the on-disk layout — the
// `<dumpDir>/index.json` plus the share-pool layout
// `<shareDir>/<algo>/<hex>`. It has no opinion on where those
// directories live.
package ocidir

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// MediaTypeDockerList is the Docker schema-2 manifest-list media
// type. image-spec only ships the OCI media types; we carry this one
// so [ParseManifest] can recognize and reject Docker manifest lists
// alongside [v1.MediaTypeImageIndex].
const MediaTypeDockerList = "application/vnd.docker.distribution.manifest.list.v2+json"

// ParseManifest decodes a single image manifest. It returns an error
// for top-level index/list documents — those should be parsed as
// [v1.Index] (via [ParseIndex]).
func ParseManifest(data []byte) (v1.Manifest, error) {
	if len(data) == 0 {
		return v1.Manifest{}, errors.New("manifest: empty input")
	}
	var probe struct {
		MediaType string          `json:"mediaType"`
		Manifests []v1.Descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(data, &probe); err == nil {
		switch probe.MediaType {
		case v1.MediaTypeImageIndex, MediaTypeDockerList:
			return v1.Manifest{}, fmt.Errorf(
				"manifest: got index/list mediaType %q, expected single manifest",
				probe.MediaType,
			)
		}
		if len(probe.Manifests) > 0 && probe.MediaType == "" {
			return v1.Manifest{}, errors.New(
				"manifest: input looks like an index (has .manifests[]) but no mediaType",
			)
		}
	}

	var m v1.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return v1.Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	if m.Config.Digest == "" {
		return v1.Manifest{}, errors.New("manifest: missing config.digest")
	}
	return m, nil
}

// ParseIndex decodes an `index.json` document into a [v1.Index] and
// validates it via [ValidateIndex]. It is the single unmarshal+validate
// choke point for index reads — every [DirV1.Index] implementation routes
// through it so the structural contract is enforced in exactly one place.
//
// It does NOT enforce the single-manifest contract: a well-formed index
// with several manifests is still a valid index. The single-manifest
// restriction is a property of how *this tool* consumes an index and is
// enforced at consumption time by [ReadManifest], not here.
func ParseIndex(data []byte) (v1.Index, error) {
	if len(data) == 0 {
		return v1.Index{}, errors.New("index: empty input")
	}
	var idx v1.Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return v1.Index{}, fmt.Errorf("index: %w", err)
	}
	if err := ValidateIndex(idx); err != nil {
		return v1.Index{}, err
	}
	return idx, nil
}

// ParseImageLayout decodes an `oci-layout` document into a
// [v1.ImageLayout] and validates it via [ValidateImageLayout]. Mirrors
// [ParseIndex] as the single unmarshal+validate choke point for
// oci-layout reads.
func ParseImageLayout(data []byte) (v1.ImageLayout, error) {
	if len(data) == 0 {
		return v1.ImageLayout{}, errors.New("layout: empty input")
	}
	var l v1.ImageLayout
	if err := json.Unmarshal(data, &l); err != nil {
		return v1.ImageLayout{}, fmt.Errorf("layout: %w", err)
	}
	if err := ValidateImageLayout(l); err != nil {
		return v1.ImageLayout{}, err
	}
	return l, nil
}

// ValidateIndex enforces the structural invariants we rely on: a
// non-empty `manifests` array. JSON decoding is the caller's job.
func ValidateIndex(idx v1.Index) error {
	if len(idx.Manifests) == 0 {
		return errors.New("index: empty .manifests[]")
	}
	return nil
}

// ValidateImageLayout enforces the structural invariants we rely on:
// a non-empty `imageLayoutVersion`. JSON decoding is the caller's job.
func ValidateImageLayout(l v1.ImageLayout) error {
	if l.Version == "" {
		return errors.New("layout: missing imageLayoutVersion")
	}
	return nil
}

// DigestBytes returns the sha256 digest of b in `sha256:<hex>` form.
func DigestBytes(b []byte) string {
	return digest.SHA256.FromBytes(b).String()
}

// SplitDigest splits "<algorithm>:<hex>" into its parts. It returns an
// error for malformed digests. Thin wrapper over [digest.Parse] kept
// for the helper signature callers rely on.
func SplitDigest(d string) (algo, hex string, err error) {
	parsed, err := digest.Parse(d)
	if err != nil {
		return "", "", fmt.Errorf("malformed digest %q (want <algo>:<hex>): %w", d, err)
	}
	return string(parsed.Algorithm()), parsed.Encoded(), nil
}
