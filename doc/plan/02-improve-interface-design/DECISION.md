# Plan 02 — Decision Log & Implementation Spec

This file records every decision that would otherwise have needed a user
interview, with the choice, the justification, and the alternatives weighed.
It doubles as the precise implementation spec handed to the implementation
subagents. Decisions are grounded in the actual code read on 2026-06-17
(`pkg/ociimagecopy`, `pkg/ocidir`, the `fsutil`/`stream` deps).

Authoritative source of intent: `PLAN.md` in this directory. Where PLAN.md
leaves something open or ambiguous, the decision is recorded here.

---

## D1. `TagStoreV1.GetOciLayout` — KEEP (PLAN open decision)

PLAN default is "keep". Decision: **keep** `GetOciLayout`/`PutOciLayout`.
- Justification: symmetry with index, and pull's verbatim mirror path needs to
  read `oci-layout` bytes from the peer to write them to local. Even though the
  file is a near-constant (`{"imageLayoutVersion":"1.0.0"}`), the mirror copies
  it byte-for-byte today (`mirrorTagFilesFromPeer`), and the fileserver meta
  stores it verbatim. Dropping `GetOciLayout` would force pull to synthesize the
  bytes, breaking byte-preservation.
- Alternative weighed: drop `GetOciLayout`, hardcode the layout bytes on read.
  Rejected — breaks the "byte-preserving, no re-marshal" guarantee in PLAN §3.

## D2. `BlobInfo.Size` semantics — caller-supplied, backend echoes

`Stat(ctx, dgst, size)` takes the expected total `size`. `BlobInfo.Size` is
that value echoed back. `BlobInfo.CurrentSize` is bytes actually present.
- complete ⟺ `CurrentSize == Size`.
- A backend that cannot independently confirm the total simply echoes `size`.
- Justification: matches PLAN §"Open decisions"; the caller always has the
  descriptor size, and the chunked fileserver backend needs it to judge
  completeness anyway.

## D3. Drop `RefPrimer` / `PrimeRefs` entirely

PLAN says "likely droppable". Decision: **drop** `RefPrimer` and `PrimeRefs`.
- Justification: its only job was to populate the fileserver's `blobsFromMeta`
  (chunkSize cache) before `ListBlobs`/`BlobSource`/`ProbeBlob`. After the
  refactor, the meta read that answers `ListBlobsByImage` **and** `GetIndex`
  (via `getImageMeta`) primes `blobsFromMeta` as a side effect, so a separate
  prime pass is redundant. No other caller exists (verified: only `push.go`
  type-asserted it).
- Error taxonomy preserved (was D14): `ListBlobsByImage` swallows
  `fs.ErrNotExist` (absent image = empty has-set) but propagates transport/auth/
  5xx errors, so a failed probe is never silently read as "remote has nothing".

## D4. Remove `Remote.ListImages` + 3 impls + their tests

- Remove the `ListImages` method from the `Remote` interface and its three
  implementations (ssh walk, localdir walk, fileserver error-return).
- Remove the tests that exercise `Remote.ListImages` specifically.
- **Keep** `ListImagesFromFs` and `parseDumpDirRel` (fs helpers, still tested
  and useful). Keep their tests.
- Justification: global, impossible on fileserver, zero orchestrator consumers
  (PLAN §1).

## D5. Replace global `Remote.ListBlobs` with `ListBlobsByImage(ctx, ref)`

- New: `Remote.ListBlobsByImage(ctx, ref) iter.Seq2[digest.Digest, error]`.
  - fileserver: one `getImageMeta` read; yield each `meta.Descriptors` digest.
    (Also primes `blobsFromMeta`.) Absent meta (`fs.ErrNotExist`) ⇒ yield
    nothing.
  - fs (ssh/localdir): read the image's manifest closure from the mirror
    (`index.json` → manifest blob → `AllDescriptors`); yield each digest.
    Absent image (`fs.ErrNotExist` on index/manifest) ⇒ yield nothing.
- **Delete** the `ListBlobsFromFs` helper (the global `share/` walk) — no longer
  referenced once `resolveRemoteHas` is per-image.
- Justification: PLAN §1/§2.6 — dedup is per-image + per-blob, never whole-store.

## D6. Naming / type shapes (locked by PLAN)

```go
type BlobStore interface {
    Stat(ctx context.Context, dgst digest.Digest, size int64) (BlobInfo, error)
    PrepDownload(ctx context.Context, dgst digest.Digest, size int64) (fsutil.ResumableSource, error)
    PrepUpload(ctx context.Context, dgst digest.Digest, size int64) (fsutil.ResumableSink, error)
}
type TagStoreV1 interface {
    GetIndex(ctx context.Context, ref imageref.ImageRef) ([]byte, error)
    PutIndex(ctx context.Context, ref imageref.ImageRef, raw []byte) error
    GetOciLayout(ctx context.Context, ref imageref.ImageRef) ([]byte, error)
    PutOciLayout(ctx context.Context, ref imageref.ImageRef, raw []byte) error
}
type StoreV1 interface { BlobStore; TagStoreV1 }
type BlobInfo struct {
    CurrentSize int64
    Size        int64
}
```
- `BlobStore`/`BlobInfo` carry **no** `V1` suffix (version-agnostic blob pool).
  `TagStoreV1`/`StoreV1` do (OCI-image-layout-v1-specific tag files).
- `PutBlobsResult` is unchanged and kept.
- The old `OciDirs` interface is **deleted** (no transitional alias).

## D7. `NewImageView` — free function, captures ctx (justified exception)

```go
func NewImageView(ctx context.Context, tags TagStoreV1, blobs BlobStore, ref imageref.ImageRef) ocidir.DirV1
```
- Returns a `*imageView` (unexported) implementing `ocidir.DirV1` **and**
  `ocidir.RawAccessor`.
- `Index()`/`ImageLayout()`: parse the verbatim bytes from
  `tags.GetIndex`/`tags.GetOciLayout` via `ocidir.ParseIndex`/`ParseImageLayout`.
- `RawIndex()`/`RawImageLayout()`: return those verbatim bytes (no re-marshal).
- `Blob(ctx, dgst, offset)`: resolve the blob's size from the parsed index's
  manifest descriptor (search `idx.Manifests` for the matching digest), call
  `blobs.PrepDownload(ctx, dgst, size)`, then `src.Open(ctx, offset)`. Return the
  resolved size. For the fileserver, the `GetIndex` call inside `Blob` primes the
  meta (chunkSize) so `PrepDownload` uses the correct chunk size — order matters,
  do `GetIndex` first.
- **ctx in struct**: `DirV1.Index()`/`ImageLayout()` take no ctx, so the view
  captures the request ctx passed to `NewImageView`. This violates the repo's
  "don't stash ctx in a struct" guideline, but is a **deliberate, scoped
  exception**: the view is a short-lived per-call adapter created immediately
  before `ReadManifest`/`ReadRawManifest`, the only consumers. PLAN's own
  signature passes ctx to `NewImageView`. `Blob` still takes its own ctx param
  and uses it.
- Location: new file `pkg/ociimagecopy/imageview.go` (package ociimagecopy).

## D8. `Stat` error semantics (modeled on os.Stat)

- Nothing present → return an error satisfying `errors.Is(err, fs.ErrNotExist)`.
- Partial → `BlobInfo{CurrentSize, Size}` with `0 < CurrentSize < Size`, nil err.
- Complete → `BlobInfo{CurrentSize: Size, Size}`, nil err.
- No `Exists`/`Complete` booleans.

### D8a. FS `Stat` implementation
Reuse `fsutil.FsSink.State` (already tested) against the share path:
- `st.Complete` ⇒ `BlobInfo{CurrentSize: st.Offset, Size: size}` (final file
  present; `st.Offset` is its size).
- `!Complete && st.Offset > 0` ⇒ `BlobInfo{CurrentSize: st.Offset, Size: size}`
  (resume from `.part`).
- `!Complete && st.Offset == 0` ⇒ `fs.ErrNotExist`.
- Build the sink via the same path logic as `PrepUpload` (share/<algo>/<hex>).

### D8b. Fileserver `Stat` implementation
Reuse the old `ProbeBlob` logic via `ChunkedSink.State`:
- chunkSize = `blobsFromMeta[dgst].chunkSize` if primed (>0), else `r.chunkSize`.
- `sink.State(ctx)` → `(offset, complete, err)`:
  - err → propagate (do NOT map to not-exist).
  - complete → `BlobInfo{CurrentSize: size, Size: size}`.
  - `!complete && offset > 0` → `BlobInfo{CurrentSize: offset, Size: size}`.
  - `!complete && offset == 0` → `fs.ErrNotExist`.

### D8a-fix. CORRECTION (found during R2 verification) — full-size uncommitted `.part`

The initial D8a/D8b mapping (`st.Offset > 0 → partial`) was **wrong** for a
full-size-but-uncommitted `.part` file. `fsutil.FsSink.State` returns
`{Offset: partSize, Complete: false}` for a `.part` (Complete is true only once
the committed final file exists). A `.part` that holds all bytes but has not yet
passed the sha256 pre-commit hook + rename therefore reported
`CurrentSize == Size`, wrongly satisfying the "complete" skip test — so
`pullOneBlob` (with `VerifyReusedBlobs=false`, the default) skipped it as
"reused", silently accepting unverified/corrupt bytes and never running the
sha256 backstop. (`internal/integration` `TestE2E_DigestMismatchOnPullResume`,
an unmodified pre-existing regression test, caught this; the orchestrator
reproduced the failure before fixing.)

**Fix:** in both `FsOciDirs.Stat` and `fileServerRemote.Stat`, the partial branch
is guarded `if st.Offset > 0 && st.Offset < size`. A non-Complete state with
`Offset >= size` (a full/oversize uncommitted `.part`) reports `fs.ErrNotExist`,
routing the caller back through the transfer, whose sha256 pre-commit hook
validates and (on mismatch) discards the `.part`. This preserves the
data-integrity backstop the resume path depends on. The invariant
"`CurrentSize == Size` ⟺ genuinely complete/committed" now holds on both
backends. (Callers do not consume `CurrentSize` as a numeric resume offset — the
fsutil Pull/Push primitives do their own `.part` resume — so reporting a
full uncommitted `.part` as absent loses nothing functionally.)

### D8c. Edge case — committed blob of wrong size
A backend reporting a complete-but-wrong-size blob is a pre-existing corruption
scenario. Stat reports facts (`CurrentSize`, `Size`); the skip decision is
`CurrentSize == Size`, so a wrong-size blob is NOT skipped and falls through to
the transfer (fsutil `Push`/`Pull` then handle it as they do today). Minor
Sent/Reused mis-count in this corrupt edge is accepted; not worth extra surface.

## D9. `MkdirBlobParent` internalized, asymmetrically (PLAN §2.1)

- **Push (fs backend)**: fold the parent `MkdirAll(share/<algo>)` into
  `FsOciDirs.PrepUpload` before returning the sink. Fileserver `PrepUpload`:
  no mkdir (flat keyspace).
- **Pull**: `pullOneBlob` writes to raw `localFs` via `opt.Pull`, not via a
  sink. Inline `localFs.MkdirAll(path.Dir(blobPath), 0o755)` in `pullOneBlob`
  (it already holds `localFs`).
- `MkdirBlobParent` is removed from every interface and impl.

## D10. Consolidate presence checks through `Stat` (PLAN §2.2)

- push skip (`transfer.go` `pushOneBlob`): `Stat`; skip (count reused) when
  `CurrentSize == Size`. Not-exist/partial ⇒ `PrepUpload` + `Push`.
- pull reuse (`transfer.go` `pullOneBlob`): `Stat` on the **local** store; skip
  when `CurrentSize == Size`, then optional sha256 verify (preserve the
  `VerifyReusedBlobs` behavior: verify the existing file, remove + re-fetch on
  mismatch). Not-exist/partial ⇒ mkdir + `PrepDownload` + `Pull` (which resumes).
- dry-run probe (`push.go` `pushOne`): `Stat` (universal; delete the
  `BlobProber` type-assertion). For each planned digest, `Stat`; if
  `CurrentSize == Size` move from toSend to reused. Stat not-exist ⇒ keep in
  plan. Other Stat error ⇒ keep in plan (best-effort; dry-run must not fail the
  whole image on a probe error — preserve current `err == nil && complete`
  semantics).

## D11. Spec unit moves to package `remote` (PLAN §"Remote spec parsing")

Move from `ociimagecopy` (`spec.go`) to `remote`:
`SSHRemoteSpec`, `FileServerRemoteSpec`, `LocalDirRemoteSpec`, `RemoteSpec`,
`RemoteKind` (+ consts), `ParseRemoteSpec`, `parseSSHRemoteSpec`,
`parseTransportSpec`, `DefaultChunkSize`, and the redaction helpers used only by
remote-spec parsing (`redactRawURL`, `RedactFileServerURL`, `RedactHeader`),
plus `ParseChunkSize`.
- **Keep in `ociimagecopy`**: `LocalSpec` + `ParseLocalSpec` (local spec, no
  remote dependency, consumed by cmd as `ociimagecopy.ParseLocalSpec`).
- `NewFileServerFromSpec` parameter type changes
  `*ociimagecopy.FileServerRemoteSpec` → `*FileServerRemoteSpec` (same package).
- `cmd/oci-image-copy/commands/zz_share.go`: `ociimagecopy.ParseRemoteSpec` →
  `remote.ParseRemoteSpec`, `ociimagecopy.RemoteKind*` → `remote.RemoteKind*`,
  `*ociimagecopy.SSHRemoteSpec` → `*remote.SSHRemoteSpec`, etc.
- `spec_test.go`: split — remote-spec tests move to `remote` package; local-spec
  tests stay in `ociimagecopy`.
- Justification: avoids the import cycle (PLAN). Verified: nothing in core
  consumes the remote-spec types except `spec.go` itself and `cmd/` (which
  already imports `remote`). `RedactFileServerURL`/`RedactHeader` are referenced
  by `fileserver.go` (same target package) — confirm with grep and move
  accordingly; if any stay referenced from core, keep a thin copy there.

## D12. `Remote` / `Local` expose `Blobs()` + `Tags()` (was `Dir()`)

- `Remote` interface: `Dir() OciDirs` → `Blobs() BlobStore` + `Tags() TagStoreV1`.
- `Local` (concrete): `Dir() OciDirs` → `Blobs() BlobStore` + `Tags() TagStoreV1`.
  Internal field becomes `dirs *FsOciDirs` (implements `StoreV1`); both accessors
  return it.
- fs-backed remotes (ssh, localdir): both accessors return their `*FsOciDirs`.
- fileserver: both accessors return `self` (`*fileServerRemote` implements
  `StoreV1`).

## D13. `resolveRemoteHas` rewrite (PLAN §2.6)

```
out := map[string]struct{}{}
for each ref in refs:
    for d, err := range peer.ListBlobsByImage(ctx, ref):
        if err != nil { return nil, err }   // ListBlobsByImage already swallows not-exist
        out[string(d)] = {}
return out
```
- Per-blob `Stat` in `pushOneBlob` is the cross-image correctness backstop.
- Honors `AssumeRemoteHas*` short-circuits exactly as before (unchanged).
- `resolveRemoteHas` keeps the `refs []imageref.ImageRef` parameter (now used to
  drive `ListBlobsByImage` instead of `PrimeRefs`).

## D14. `RawAccessor` use removed from pull

`mirrorTagFilesFromPeer` no longer type-asserts `ocidir.RawAccessor`. It reads
verbatim bytes via `peer.Tags().GetIndex`/`GetOciLayout` and writes via
`dst.PutOciLayout`/`PutIndex`. `ocidir.RawAccessor` the type stays (FsDir +
imageView implement it; still used by `ReadRawManifest` paths indirectly — keep).
Write order: `oci-layout` then `index.json` (unchanged, preserves fileserver
commit-marker "both present ⇒ commit" with index last for the manifest read).

## D15. Fileserver commit-marker split (PLAN §2.4)

`PutTagFile` → `PutIndex` + `PutOciLayout`. Each stashes its verbatim half in the
per-ref `accum` (still keyed by `ref.String()`, still mutex-guarded). Whichever
arrives second triggers the existing `commitImageMeta` (consume + single Put).
Invariant "both present ⇒ commit, meta written last" unchanged. Read-only guard
on both. The old single `PutTagFile` name-switch is removed.

## D16. Verbatim byte preservation

`TagStoreV1.GetIndex`/`GetOciLayout`/`PutIndex`/`PutOciLayout` carry raw `[]byte`
end to end (no parse/re-marshal). fs backend: write/read the files directly.
fileserver: stash/return `json.RawMessage` verbatim. This makes PLAN §3
byte-preservation intrinsic (no reliance on the `RawAccessor` opt-in).

## D17. Things explicitly NOT changed

- `ocidir.DirV1`, `ocidir.ReadManifest`, `ocidir.ReadRawManifest`,
  `ocidir.RawAccessor`, `ocidir.FsDir`, `ocidir.SharedFsDir` — all unchanged.
- `enumerate.go` `sharedDir` and the skopeo-dump walk — unchanged (does not use
  the store interface).
- On-disk / on-wire schema — unchanged (PLAN §3).
- `fileServerRemote.InspectImage` keeps reading the manifest via the meta; it may
  be re-expressed through `NewImageView` but is NOT required to — leave its
  internal read as-is unless it references a removed method. (It uses
  `getImageMeta` + `ChunkedSourceAdapter` directly; keep.)

## Verification gates (empirical, run by the orchestrator — not trusted from agents)

1. `go build ./...` green after production refactor.
2. `go test -skip '_Local$' ./...` green after test migration.
3. `go vet ./...` clean.
4. `go test -run '_Local$' ./...` — local suite passes or cleanly skips.
5. Round-trip: integration tests (`internal/integration`) exercise fs +
   fileserver push/pull; confirm they pass. Spot-check a manual round-trip if
   integration coverage is thin on the changed paths.
6. Diff review of the high-risk files: `imageview.go`, the new interface file,
   `fsocidirs.go`, `remote/fileserver.go`, `transfer.go`, `pull.go`, `push.go`.
