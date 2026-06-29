# SUMMARY — O2: Bounds-check index manifests; single ParseIndex choke point

## What changed

`idx.Manifests[0]` was indexed unguarded in several places, and index
read+parse+validate was copy-pasted across three packages with the copies
disagreeing (the fileserver meta path omitted validation entirely, so a peer
serving `{"manifests":[]}` could panic `ReadManifest` — a remotely-triggerable
crash). The `ocidir` package doc also referenced a `[ParseIndex]` that did not
exist.

- `pkg/ocidir/manifest.go`
  - Added `ParseIndex([]byte) (v1.Index, error)` and
    `ParseImageLayout([]byte) (v1.ImageLayout, error)` — unmarshal + validate
    choke points mirroring `ParseManifest`. This also makes the package doc's
    `[ParseIndex]` reference resolve (stale-doc fix).

- `pkg/ocidir/dir.go`
  - `FsDir.Index` / `FsDir.ImageLayout` now route through `ParseIndex` /
    `ParseImageLayout` (dropped the inlined unmarshal; removed the now-unused
    `encoding/json` import).
  - Extracted `ReadRawManifest(ctx, dir) (v1.Descriptor, []byte, error)` as the
    single index-walk + manifest-read + verify choke point. It enforces the
    single-manifest contract explicitly (decision D12) with three new sentinel
    errors: `ErrEmptyIndex`, `ErrMultiManifestIndex`, and `ErrNestedIndex`
    (single entry whose mediaType is an image-index / Docker manifest-list).
  - `ReadManifest` now delegates to `ReadRawManifest` and only adds the parse.

- `pkg/imagecopy/enumerate.go` — `sharedDir.Index` / `ImageLayout` route through
  `ocidir.ParseIndex` / `ParseImageLayout` (removed the inlined unmarshal and the
  now-unused `encoding/json` import).

- `pkg/imagecopy/fileserver/meta.go` — `ImageMeta.ParsedIndex` /
  `ParsedImageLayout` route through the ocidir choke points, so the fileserver
  meta path now validates the index (closing the `{"manifests":[]}` crash). Added
  the `ocidir` import (no cycle: ocidir imports no internal packages).

- `pkg/imagecopy/fileserver_remote.go` — `commitImageMeta` parses index.json via
  `ocidir.ParseIndex` instead of a local unmarshal + manual length check.

- `pkg/imagecopy/remote.go` (InspectImage oci branch) and
  `pkg/imagecopy/localdir_remote.go` (InspectImage) — replaced their duplicate
  index-walk + blob-read with `ocidir.ReadRawManifest`, which returns the
  verified raw bytes (preserving the `sha256(returned bytes) == manifest digest`
  contract) and enforces the single-manifest contract. Removed the now-unused
  `io` imports; added `ocidir`.

## D12 note

Multi-arch fan-out stays unsupported (matches the README limitation). A
multi-entry or nested-index index.json now errors explicitly instead of silently
taking entry [0].

## Tests added

- `pkg/ocidir/manifest_test.go`: `TestParseIndex_OK`, `TestParseIndex_Errors`
  (empty input / malformed json / empty manifests), `TestParseImageLayout_OK`,
  `TestParseImageLayout_Errors`.
- `pkg/ocidir/dir_test.go`: `TestReadManifest_EmptyIndex` (error, not panic),
  `TestReadManifest_MultiManifest` (`ErrMultiManifestIndex`),
  `TestReadManifest_NestedIndex` (`ErrNestedIndex` for OCI index + Docker list
  mediaTypes).
- `pkg/imagecopy/fileserver/meta_test.go`: `TestParsedIndex_EmptyManifests`.

## Test evidence

- `go build ./...` — ok
- `go vet ./...` — ok
- `go test ./pkg/... ./cmd/...` — all ok
- New O2 tests — PASS
- `go test ./internal/integration/` — ok (35.85s)

## Deviations from plan

None.
