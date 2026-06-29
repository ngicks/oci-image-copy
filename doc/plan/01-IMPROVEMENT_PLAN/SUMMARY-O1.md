# SUMMARY — O1: Verify manifest bytes against the descriptor digest (trust root)

## What changed

The manifest blob is the one blindly-trusted content-addressed read in the
pipeline: `ReadManifest` read it by digest, parsed it, and returned it without
ever recomputing that digest. Every downstream config/layer digest is taken
from that manifest body, so a corrupt or tampered manifest poisoned the whole
closure undetected. (Blob transfers on pull already had a sha256 PreCommitHook;
the manifest read did not.)

- `pkg/ocidir/dir.go`
  - Added `VerifyBlobBytes(want digest.Digest, data []byte) error`, a shared
    trust-root check built on the vendored go-digest `digest.Digest.Verifier()`
    API. It validates `want` first (so an unavailable-but-well-formed algorithm
    is reported, not panicked on), writes the bytes to the verifier, and on
    mismatch returns `ErrManifestDigestMismatch` wrapped with both the expected
    digest and the actual digest of the bytes.
  - Added the `ErrManifestDigestMismatch` sentinel error.
  - `ReadManifest` now calls `VerifyBlobBytes(mDesc.Digest, mData)` after reading
    the manifest blob and before `ParseManifest`.

- `pkg/imagecopy/fileserver_remote.go` (the other two manifest-read paths):
  - `InspectImage` verifies the raw manifest bytes against the meta's manifest
    descriptor digest before returning them (callers rely on
    `sha256(returned bytes) == manifest digest`).
  - `commitImageMeta` verifies the manifest blob bytes before deriving the
    descriptor closure from them.

## Test fixture correction (not a plan defect)

`pkg/imagecopy/enumerate_test.go::TestEnumerate_OCI_FilesystemWalk` stored the
`ociManifestFixture` blob under a path named `sha256:dddd...` while declaring
that same placeholder digest in `index.json`. The fixture was internally
inconsistent (the stored bytes never hashed to `dddd...`) and only passed
because the manifest was previously unverified. The test now builds
`index.json` and the blob path from the manifest's real digest
(`ocidir.DigestBytes(ociManifestFixture)`), matching real on-disk layouts.
This is a fixture fix, not a behavior regression.

## Tests added

- `pkg/ocidir/dir_test.go`: `TestReadManifest_DigestMismatch`,
  `TestReadManifest_DigestMatch`, `TestVerifyBlobBytes`.
- `pkg/imagecopy/fileserver_remote_test.go`:
  `TestFileServerRemote_InspectImage_DigestMismatch`.

## Test evidence

- `go build ./...` — ok
- `go vet ./...` — ok
- `go test ./pkg/... ./cmd/...` — all ok
- New O1 tests — PASS
- `go test ./internal/integration/` — ok (35.96s; ssh+skopeo present in env)

## Deviations from plan

None. The fixture correction above was required to keep `./pkg/...` green and is
documented as a fixture defect that the (correct) verification exposed.
