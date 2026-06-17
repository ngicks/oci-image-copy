# oci-image-copy — Improvement Plan 02: split `OciDirs` by concern

Scope: module `github.com/ngicks/oci-image-copy`, package `pkg/ociimagecopy`
(+ `remote/`, `fileserver/`, `ocidir/`).
Origin: interface-design review of `OciDirs` (2026-06-17).
Baseline: all tests green before work starts.

## Problem

`OciDirs` (`ocidirs.go:22`) is one interface doing two unrelated jobs and
carrying two methods that don't earn their place:

- `Blob(ctx, dgst, offset)` — **dead in production**. Every real read goes
  through `Image(ref)` → `ocidir.DirV1.Blob`; `OciDirs.Blob` is only called
  from `_test.go`. Redundant with `Image().Blob()`.
- `MkdirBlobParent(dgst)` — **abstraction leak**. It exists only because the
  fsutil `Pull`/`Push` primitives refuse to create parent dirs; the fileserver
  backend implements it as a literal no-op (`remote/fileserver.go:409`),
  proving it is an `FsOciDirs`-only detail forced onto every backend.
- `BlobSource`/`BlobSink` — fine semantics (they return resumable handles), but
  the names read as jargon and sit next to the tag-file methods.
- `Image(ref) DirV1` + `PutTagFile(...)` — a *different* concern (per-tag
  pointer files), bundled into the same interface as the content-addressed
  blob pool.

The fix: split into two small consumer-side interfaces, drop the two dead/leaky
methods, and rename the transfer primitives to say what they do. `ocidir.DirV1`
itself stays — it is a general OCI-dir read primitive (used for skopeo dumps,
not just the `Image()` bridge); only the `Image()` *method* leaves the store
interface, and a small adapter keeps the manifest-read choke point working.

### Locked naming convention

The `V1` suffix marks only the surface that is **OCI-image-layout-v1-specific**
— the tag files (`index.json`/`oci-layout`) — mirroring `ocidir.DirV1` and
leaving room for a `V2` layout. So `TagStoreV1` and the combined `StoreV1` (which
embeds it) carry it. `BlobStore` does **not**: it is a plain content-addressable
blob pool keyed by digest, version-agnostic by construction, with no planned V2.
Its result type is plain `BlobInfo` for the same reason.

---

## 1. Public API shape

### Before

```go
type OciDirs interface {
    Blob(ctx, dgst, offset) (io.ReadCloser, int64, error)
    Image(ref) ocidir.DirV1
    BlobSource(ctx, dgst, size) (fsutil.ResumableSource, error)
    BlobSink(ctx, dgst, size) (fsutil.ResumableSink, error)
    MkdirBlobParent(dgst) error
    PutTagFile(ctx, ref, name string, data []byte) error
}

// optional, type-asserted at use site (push.go)
type BlobProber interface { ProbeBlob(ctx, dgst, size) (bool, error) }
type RefPrimer  interface { PrimeRefs(ctx, refs) error }
```

### After

```go
// content-addressed blob pool — large, streamed, resumable
type BlobStore interface {
    Stat(ctx, dgst digest.Digest, size int64) (BlobInfo, error)
    PrepDownload(ctx, dgst digest.Digest, size int64) (fsutil.ResumableSource, error)
    PrepUpload(ctx, dgst digest.Digest, size int64) (fsutil.ResumableSink, error)
}

// per-tag pointer files — tiny, whole-file, verbatim bytes
type TagStoreV1 interface {
    GetIndex(ctx, ref imageref.ImageRef) ([]byte, error)
    PutIndex(ctx, ref imageref.ImageRef, raw []byte) error
    GetOciLayout(ctx, ref imageref.ImageRef) ([]byte, error)
    PutOciLayout(ctx, ref imageref.ImageRef, raw []byte) error
}

// full OCI image-layout v1 store = both halves
type StoreV1 interface {
    BlobStore
    TagStoreV1
}

type BlobInfo struct {
    CurrentSize int64 // bytes currently present at the store (the resume point)
    Size        int64 // total expected blob size; complete ⟺ CurrentSize == Size
}

// Remote composes the two halves (was: Dir() OciDirs)
type Remote interface {
    Blobs() BlobStore
    Tags()  TagStoreV1
    ListBlobsByImage(ctx, ref imageref.ImageRef) iter.Seq2[digest.Digest, error] // was: ListBlobs(ctx)
    InspectImage(...)   // unchanged
    // ListImages removed — see below
}
```

**Dedup is scoped, not global.** The old `Remote.ListBlobs(ctx)` enumerated the
*entire* store — a scale landmine (fs remotes walk all of `share/sha256/*`; the
fileserver can only return the union of consulted metas and documents "no global
index"). Its only consumer is push's `resolveRemoteHas`, and dedup never needs
the whole store — only, per image being transferred, which of *that image's*
blobs the peer already holds. That decomposes into:

- `Remote.ListBlobsByImage(ctx, ref)` — the image's closure as the peer knows it
  (fileserver: `meta.Descriptors`, one read; fs: the manifest closure). Cost is
  closure-sized, not store-sized. Cheap pre-pass for re-pushes.
- `BlobStore.Stat` (probe) — per-blob, the correctness backstop for cross-image
  shared layers and dry-run accuracy. `pushOneBlob` Stats before `PrepUpload`
  regardless, so dedup is correct even when the pre-pass misses.

**`ListImages` is removed** for the same reasons, and then some: it is global
(same scale risk), the fileserver backend literally **can't** implement it
(returns a "no global index" error), and it has **no orchestrator consumer at
all** — the only callers are tests of itself. Remove the `Remote.ListImages`
method and its three implementations (ssh/localdir error-or-walk). The fs-only
helpers `ListImagesFromFs` / `parseDumpDirRel` stay (they work fine for fs
stores and are still useful/tested) — it is the *polymorphic interface method*
that is impossible, not the fs walk.

**Verb split is deliberate.** `BlobStore` streams (handle factories +
metadata-only `Stat`), so `Prep*`/`Stat`; `TagStoreV1` reads/writes tiny whole
files, so `Get`/`Put`. Different access pattern ⇒ different verbs.

**`Stat` is modeled on `os.Stat`.** It reports how much of the blob is present:

- nothing present → an error satisfying `errors.Is(err, fs.ErrNotExist)`;
- partial → `BlobInfo{CurrentSize, Size}` with `0 < CurrentSize < Size`;
- complete → `CurrentSize == Size`.

No `Exists`/`Complete` booleans: absence is an error, completeness is the
`CurrentSize == Size` comparison. `CurrentSize` doubles as the resume offset, so
`Stat` subsumes the resumable sink's `State()` (`Offset`+`Complete`) for the
skip/resume decision. `size` stays a parameter: the chunked fileserver backend
needs the expected total to judge completeness and to fill `Size` for a partial
blob.

**`TagStoreV1` is `[]byte`, not structs.** Tag files are handled verbatim — no
parse/re-marshal round-trip — so a mirror preserves the source's exact bytes
(this is the current behavior, via `ocidir.RawAccessor`, made intrinsic to the
interface). `index.json` and `oci-layout` are **separate** method pairs because
`index.json` is mutable (a repeated `podman image save -f oci-dir` to the same
ref appends manifests, so the destination may read-merge-write) while
`oci-layout` is a write-once version marker. Merge is the *caller's* policy:
parse via `ocidir.ParseIndex`, merge, marshal, `PutIndex`. The store stays a
byte sink.

**`Image()` is replaced by an adapter, not deleted capability.**
`ocidir.DirV1` and `ocidir.ReadManifest`/`ReadRawManifest` are unchanged. A new
bridge reconstructs a per-ref `DirV1` from the split store:

```go
func NewImageView(ctx, tags TagStoreV1, blobs BlobStore, ref) ocidir.DirV1
```

`Index`/`ImageLayout` serve verbatim bytes via `TagStoreV1`; `Blob` serves the
manifest blob via `BlobStore.PrepDownload`, resolving its size from the parsed
index (the only blob `ReadManifest` reads through the view). It also satisfies
`ocidir.RawAccessor`. Manifest-read call sites change by one line each:
`ReadManifest(ctx, peer.Dir().Image(ref))` → `ReadManifest(ctx, NewImageView(ctx, peer.Tags(), peer.Blobs(), ref))`.

### Removed / folded

| Old surface | Disposition |
|---|---|
| `OciDirs.Blob` | **deleted** — dead in prod; manifest reads go via `NewImageView` → `PrepDownload` |
| `OciDirs.MkdirBlobParent` | **deleted** — internalized (see §2) |
| `OciDirs.BlobSource` | renamed → `BlobStore.PrepDownload` |
| `OciDirs.BlobSink` | renamed → `BlobStore.PrepUpload` |
| `OciDirs.Image(ref) DirV1` | **deleted from the interface** — replaced by free `NewImageView`; `ocidir.DirV1` itself kept |
| `OciDirs.PutTagFile` | replaced by `TagStoreV1.PutIndex` / `PutOciLayout` (verbatim `[]byte`) |
| `BlobProber.ProbeBlob` | folded into `BlobStore.Stat` (optional iface removed) |
| `RefPrimer.PrimeRefs` | **likely droppable** — its meta-priming role folds into `ListBlobsByImage`/`GetIndex` (see §2.6); keep only if another caller needs it |
| `ocidir.RawAccessor` (in pull) | folds away — `TagStoreV1` already returns verbatim bytes |
| `Remote.ListBlobs(ctx)` (global) | replaced by `Remote.ListBlobsByImage(ctx, ref)` + `BlobStore.Stat` |
| `ListBlobsFromFs` helper | **deleted** — per-image closure read replaces the global `share/` walk |
| `Remote.ListImages(ctx)` | **deleted** — global, impossible on the fileserver, no orchestrator consumer (tests only); `ListImagesFromFs`/`parseDumpDirRel` kept |

### Remote spec parsing moves out of the core package

`spec.go`'s remote-spec types and parser move from `ociimagecopy` to the
`remote` package: `SSHRemoteSpec`, `FileServerRemoteSpec`, `LocalDirRemoteSpec`,
the `RemoteSpec` sum type, and `ParseRemoteSpec`.

Why the whole unit, not just `FileServerRemoteSpec`: `remote` already imports
`ociimagecopy`, and `RemoteSpec` embeds `*FileServerRemoteSpec`. Moving only the
fileserver spec while `RemoteSpec` stays in core would force `ociimagecopy` to
import `remote` → **import cycle**. Moving the cohesive unit (nothing in core
consumes it; only `spec.go` itself and `cmd/`, which already imports `remote`)
breaks no cycle and co-locates spec parsing with the factories it feeds
(`NewFileServerFromSpec`, the SSH/localdir builders) — matching the repo's
"factories live with implementations" rule. `cmd/` switches
`ociimagecopy.ParseRemoteSpec` → `remote.ParseRemoteSpec` (etc.).

---

## 2. Change of logic

1. **`MkdirBlobParent` internalized, asymmetrically.**
   - *Push:* the destination write goes through the sink, so the parent-dir
     `MkdirAll` folds into `FsOciDirs.PrepUpload`. Fileserver: was a no-op, now
     simply absent from the interface.
   - *Pull:* the destination write does **not** go through a sink — `pullOneBlob`
     writes to the raw `localFs` via `opt.Pull(...)` (`transfer.go:189`). So its
     mkdir is inlined in `pullOneBlob` using the `localFs` it already holds
     (`localFs.MkdirAll(path.Dir(blobPath), 0o755)`), not hidden behind a sink.

2. **`Stat` consolidates three ad-hoc presence checks** at the call sites:
   - push skip: `sink.State(ctx).Complete` (`transfer.go:264`) → `Stat`, skip when `CurrentSize == Size`
   - pull reuse: `localFs.Stat(blobPath)` size match (`transfer.go:141`) → `Stat`, then optional sha256 verify
   - dry-run probe: `BlobProber.ProbeBlob` (`push.go:254-257`) → `Stat` (universal; no type assertion)

   Partial/absent both fall through to the transfer, which resumes from
   `CurrentSize`. The fs backend answers from `fs.Stat` / the `.part` sidecar;
   the fileserver answers from chunk-state against the expected `size` (the old
   `ProbeBlob` logic).

3. **Manifest reads go through `NewImageView`; `DirV1` is preserved.**
   `ocidir.ReadManifest`/`ReadRawManifest` keep their `DirV1` signature; the four
   call sites (`pull.go:206`, `push.go:373`, `remote/ssh.go:356`,
   `remote/localdir.go:107`) wrap the split store in `NewImageView`. The
   fileserver's `fsMetaDirV1` and the fs `Image()` view are removed in favor of
   the one generic adapter. **This is the only non-mechanical part.** Subtlety:
   `NewImageView.Blob` needs the manifest size; it reads the parsed index for it,
   and for the fileserver the `GetIndex` call first primes the meta cache
   (chunkSize), so the subsequent `PrepDownload` uses the correct chunk size.

4. **Fileserver commit-marker preserved across two methods.** Today `PutTagFile`
   buffers `oci-layout`+`index.json` per ref and writes the `ImageMeta` commit
   marker once *both* arrive (`remote/fileserver.go:411-439`). After the split,
   `PutOciLayout` and `PutIndex` each stash their (verbatim) half; whichever
   lands second triggers the same meta assembly + single committing `Put`. The
   "both present ⇒ commit, meta written last" invariant is unchanged.

5. **Index-merge policy stays in the service layer.** `TagStoreV1` is mechanism:
   `GetIndex` returns the current index bytes (or `fs.ErrNotExist`), `PutIndex`
   writes the bytes given. Merging appended manifests on a repeated save is the
   push/pull orchestration's job (parse → merge → marshal → `PutIndex`),
   consistent with the repo's "stores are mechanism, services compute" rule.
   Current behavior is unchanged (no merge yet); the shape *enables* it.

6. **Dedup is per-image + per-blob, never whole-store.** `resolveRemoteHas`
   (`push.go`) stops iterating a global `peer.ListBlobs(ctx)`. Instead, for each
   ref being pushed it calls `peer.ListBlobsByImage(ctx, ref)` (one meta read for
   the fileserver; a manifest-closure read for fs remotes) to seed the has-set
   cheaply, and the per-blob `Stat` in `pushOneBlob` (and the dry-run probe loop)
   covers cross-image shared layers. `PrimeRefs` folds into this: the meta read
   that answers `ListBlobsByImage`/`GetIndex` also caches the chunkSize that
   `PrepDownload`/`Stat` need, so the separate prime pass is no longer required
   (the optional `RefPrimer` may be dropped if nothing else needs it).

---

## 3. Persistent data schema

The refactor is **schema-neutral and byte-preserving**. No object/path moves,
and because `TagStoreV1` carries verbatim `[]byte`, persisted tag files are
byte-identical to the source (no re-marshal).

### FS backend (`FsOciDirs`) — unchanged layout

```
<base>/
  <host>/<repo-path>/_tags/<tag>/        index.json, oci-layout   (TagStoreV1)
  <host>/<repo-path>/_digests/<hex>/     index.json, oci-layout   (TagStoreV1)
  share/<algo>/<hex>                     content-addressed blobs  (BlobStore)
```

### Fileserver backend — unchanged keys & meta

```
meta:  <prefix>/images/<host>/<repo-path>/_tags/<tag>.json   (commit marker)
chunk: <prefix>/blobs/<algo>/<hex>/<08d index>               (blob chunks)
```

`ImageMeta` JSON (`fileserver/meta.go:38`) is untouched on the wire:
`{version, chunkSize, ociLayout (raw), indexJSON (raw), descriptors[]}`. The
`json.RawMessage` `OciLayout`/`IndexJSON` fields stay raw; the fileserver
`TagStoreV1` returns those bytes verbatim from `GetIndex`/`GetOciLayout` and
feeds the accumulator verbatim from `PutIndex`/`PutOciLayout`. Because the
bytes round-trip untouched, the "digest math over the bytes holds" rationale in
`meta.go` is preserved by construction.

### Compatibility

No migration required — existing fs stores and fileserver stores remain
readable/writable, and tag-file bytes are unchanged. Pure API/logic refactor.

---

## Open decisions

- **`GetOciLayout`** — kept for symmetry, but low value (the file is a constant).
  Droppable if nothing reads it back. (default: keep)
- **`BlobInfo.Size` on the fileserver without primed meta** — `Stat` takes the
  expected `size` so the caller (which has the descriptor size) supplies the
  total; a backend that cannot independently confirm it echoes that value.

## Work breakdown (suggested order)

1. Define `BlobStore` / `TagStoreV1` / `StoreV1` / `BlobInfo` and
   `NewImageView`.
2. Implement both interfaces on `FsOciDirs`; internalize the push mkdir
   (`PrepUpload`) and inline the pull mkdir (`pullOneBlob`).
3. Implement both on `fileServerRemote`; split `PutTagFile` into
   `PutIndex`/`PutOciLayout` over the shared commit-marker buffer; add
   `GetIndex`/`GetOciLayout` (from meta) and `Stat` (from chunk-state); remove
   `Blob`/`Image`/`fsMetaDirV1`/`ProbeBlob`/`MkdirBlobParent`.
4. Route the four `ReadManifest`/`ReadRawManifest` call sites through
   `NewImageView`; drop `ocidir.RawAccessor` use in pull.
5. Route push skip / pull reuse / dry-run probe through `Stat`
   (`CurrentSize == Size`); delete `BlobProber`.
6. Replace `Remote.ListBlobs` with `ListBlobsByImage`; rewrite `resolveRemoteHas`
   to seed the has-set per pushed ref + lean on per-blob `Stat`; delete
   `ListBlobsFromFs`; fold `PrimeRefs` into the meta read (drop `RefPrimer` if
   unused). Also delete `Remote.ListImages` + its three impls and the three
   tests of it (`ListImagesFromFs`/`parseDumpDirRel` stay).
7. Move the spec unit (`*RemoteSpec`, `ParseRemoteSpec`) from `ociimagecopy` to
   `remote`; update `cmd/` call sites.
8. Replace `OciDirs` with the two interfaces everywhere; `Remote.Dir()` →
   `Blobs()`/`Tags()`; `Local.Dir()` → `Blobs()`/`Tags()`; retype `transfer.go`,
   `pull.go`, `push.go` params.
9. Verification: full `go test ./...`; `_Local` suite where testdata present;
   round-trip pull→push→pull on fs + fileserver backends.
