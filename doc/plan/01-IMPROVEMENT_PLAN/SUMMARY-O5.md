# SUMMARY — O5: imageref host/path/tag validation (defense-in-depth)

## What changed

Path segments were only checked for empty + ReservedSegments, so `.` and `..`
passed; tags were accepted verbatim (slash/overlong/control chars); and host
content was never validated — and `looksLikeHost("..")` is TRUE (contains '.'),
so `../../x:latest` parsed with host `..`. These fields flow into
`filepath.Join(Base, Host, …, Tag)` with vroot as the only backstop. Hosts were
also not lowercased, so `DOCKER.IO/nginx` skipped library canonicalization.

- `pkg/imageref/imageref.go`
  - `Parse` now lowercases the host (registry hosts are case-insensitive DNS
    names) *before* the `DefaultRegistry` comparison, so `DOCKER.IO/nginx`
    canonicalizes to `docker.io/library/nginx`. Path case is preserved.
  - Added `validateHost` (rejects empty, `.`/`..`, path separators, control
    chars), `validatePath` / `validatePathSegment` (rejects `.`/`..`,
    separators, control chars; keeps the existing empty + reserved checks), and
    `validateTag` (grammar `[A-Za-z0-9_][A-Za-z0-9._-]{0,127}`). `Parse` calls
    all three.
  - Added the exported `ValidateHostPathTag(host, path, tag)` shared validator
    and exported `ValidateDigestHex` so the read side can apply the same rules.

- `pkg/imagecopy/remote.go`
  - `parseDumpDirRel` (read side — reconstructs a ref from a peer-controlled
    dump-dir name) now calls `imageref.ValidateHostPathTag` for both `_tags/`
    and `_digests/` paths, and `imageref.ValidateDigestHex` for the digest leaf,
    so a maliciously-named directory cannot smuggle a `..` host/segment or a
    slash-bearing tag back into an ImageRef.

## Tests added

- `pkg/imageref/imageref_test.go`:
  - Parse success: uppercase host lowercased + canonicalized
    (`DOCKER.IO/nginx`), mixed-case registry host lowercased (path case kept).
  - Parse errors: `..`/`.` path segments (path traversal), traversal host
    (`../../x:latest`), overlong tag, bad-leading-char tag, invalid-char tag.
  - `TestValidateHostPathTag` (read-side validator: traversal host, slash host,
    empty path, traversal/reserved path segment, slash-in-tag, overlong tag,
    control char in tag) and `TestValidateDigestHex`.
- `pkg/imagecopy/remote_test.go`:
  - `TestParseDumpDirRel_Valid` (tagged + digested reconstruct correctly).
  - `TestParseDumpDirRel_RejectsMalicious` (traversal segment, overlong/bad tag,
    short/non-hex digest).

## Test evidence

- `go build ./...` — ok
- `go vet ./...` — ok
- `go test ./pkg/... ./cmd/...` — all ok
- New O5 tests — PASS
- `go test ./internal/integration/` — ok (37.33s; ListImages walks dump dirs
  through parseDumpDirRel)

## Deviations from plan

The plan listed "slash-in-tag" as a Parse test case, but `Parse`'s own tokenizer
structurally cannot place a slash in a tag (the tag is only the colon-suffix of
the last path segment). The slash-in-tag guard is therefore meaningful on the
read side, where a literal directory leaf can contain a slash; it is tested via
`ValidateHostPathTag` and `parseDumpDirRel` instead. No functional deviation.
