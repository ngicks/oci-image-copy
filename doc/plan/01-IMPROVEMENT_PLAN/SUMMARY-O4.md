# SUMMARY — O4: Honest remote-blob presence — chunk-name algorithm + probe error taxonomy

## What changed

Three honesty gaps in the file-server remote: chunk names hardcoded
`blobs/sha256/` (discarding the digest algorithm, so addresses could collide
across algorithms); probe paths swallowed ALL errors as "absent", so a 401/5xx
read as "remote has nothing, send everything"; and the meta-less top-level
`Blob` returned only chunk 0 for multi-chunk blobs while claiming the full size
(a latent interface landmine).

- `pkg/imagecopy/fileserver/naming.go` (decision D13)
  - `DefaultNaming.BlobChunk` now emits `blobs/<algo>/<hex>/<index>` using the
    digest's actual algorithm instead of a hardcoded `sha256`. For sha256 blobs
    the key is byte-identical (so existing sha256 stores are unaffected); other
    algorithms now get their own namespace and cannot collide. Wire-format change
    accepted pre-1.0; documented in the package doc.

- `pkg/imagecopy/fileserver_remote.go` (decision D14 + landmine fix)
  - `PrimeRefs` now propagates any non-`fs.ErrNotExist` error (transport / auth /
    5xx) instead of swallowing it. Absent metas (`fs.ErrNotExist`, e.g. first
    push) are still skipped silently.
  - `Blob` probes chunk 1 first: if it exists, the blob is multi-chunk and the
    call fails with the new `ErrMultiChunkBlobUnsupported` (rather than truncating
    to chunk 0); a transport error probing chunk 1 is surfaced, not swallowed.
    Single-chunk blobs still serve normally.
  - (`ProbeBlob` already propagated non-ErrNotExist errors from
    `ChunkedSink.State`, which only maps `fs.ErrNotExist` to "absent"; covered by
    a new regression test.)

- `README.md` — added a "Push trusts remote chunk integrity by size" note under
  the file-server section: size-only presence check, no content re-hash, the new
  algorithm-in-key format, the propagated-probe-error guarantee, and that a
  content-verifying push flag is deferred (D14).

## Tests added

- `pkg/imagecopy/fileserver/naming_test.go`:
  `TestDefaultNaming_BlobChunk_AlgorithmSegment` (sha512 lands under
  `blobs/sha512/`).
- `pkg/imagecopy/fileserver_remote_test.go` (new `errClient` test double that
  fails Get/Stat with a non-ErrNotExist error):
  - `TestFileServerRemote_PrimeRefs_PropagatesTransportError` (5xx is propagated,
    not "absent").
  - `TestFileServerRemote_PrimeRefs_AbsentIsSilent` (fs.ErrNotExist is silent).
  - `TestFileServerRemote_ProbeBlob_PropagatesTransportError` (401 surfaced).
  - `TestFileServerRemote_Blob_MultiChunkUnsupported` (2-chunk blob ->
    `ErrMultiChunkBlobUnsupported`).
  - `TestFileServerRemote_Blob_SingleChunkOK` (single-chunk still works).

## Test evidence

- `go build ./...` — ok
- `go vet ./...` — ok
- `go test ./pkg/... ./cmd/...` — all ok
- New O4 tests — PASS
- `go test ./internal/integration/` — ok (36.25s; the fileserver E2E push/pull
  exercises the new chunk naming end-to-end)

## Deviations from plan

None. Note that for sha256 (the only algorithm in use today) the chunk-name
"wire-format change" is byte-identical, so the D13 store-invalidation only bites
non-sha256 algorithms; the algorithm segment is now correct for all algorithms.
The `--verify`-style push flag remains deferred per D14.
