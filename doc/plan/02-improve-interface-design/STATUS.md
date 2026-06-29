# Plan 02 — Working Status

Last updated: 2026-06-17 (orchestrator: main context)

## Goal
Implement `PLAN.md` (split `OciDirs` into `BlobStore` + `TagStoreV1` + `StoreV1`,
drop dead/leaky methods, rename transfer primitives, replace `Image()` with
`NewImageView`, scope dedup per-image, move the remote spec unit to `remote`).

Decisions / impl spec: see `DECISION.md`.

## Orchestration model
- Main context = orchestrator: writes decisions, dispatches opus subagents,
  reviews their output **empirically** (build + test + diff read), iterates.
- Subagents do the edits; opus agents may spawn sonnet helpers.

## Baseline
- `go build ./...` ✅ (clean, captured 2026-06-17)
- `go test -skip '_Local$' ./...` ✅ all green (incl. `internal/integration` 38s)

## Plan of record (rounds)
- **R1 — Production refactor + spec move → `go build ./...` green.** (1 opus agent)
  Steps 1–8 (production side only): new interfaces, `NewImageView`, both impls,
  consumer rewrite (transfer/pull/push), remove old surface, move spec to remote,
  fix cmd. Tests left red (expected; R2 fixes them).
- **R2 — Test migration → `go test -skip '_Local$' ./...` green.** (1 opus agent,
  fans out to sonnet per test package)
- **R3 — Empirical review + verification gates** (orchestrator): full test run,
  vet, local suite, integration round-trips, high-risk diff read.

## Progress
- [x] Read all relevant source + deps (fsutil, stream/fileserver)
- [x] DECISION.md written (D1–D17 + gates)
- [x] STATUS.md written
- [x] R1 production refactor (1 opus agent) — done
- [x] R1 gate: `go build ./...` ✅ green (independently verified by orchestrator)
- [x] R1 empirical review: read ocidirs.go, imageview.go, fsocidirs.go,
      transfer.go, remote/fileserver.go, localdir.go, pull/push consumers,
      spec split — all match DECISION.md; symbol sweep clean (no lingering
      OciDirs/removed symbols in production).
- [x] R2 test migration (1 opus lead + per-package sonnet fan-out) — done.
      Migrated fakes/assertions to the split interfaces; moved remote-spec
      tests into `remote` package; deleted tests of genuinely-removed surface
      (ListImages, global ListBlobs, ProbeBlob, PrimeRefs, MkdirBlobParent,
      meta-less Blob/ErrMultiChunkBlobUnsupported, RawAccessor pull fallback)
      and re-expressed their intent as Stat / ListBlobsByImage tests. No
      assertions weakened; no expected values changed.
- [x] **Production bug found by R2 and fixed by orchestrator** — `Stat` reported
      a full-size uncommitted `.part` as complete (silent corrupt-blob reuse on
      pull). Fixed in `FsOciDirs.Stat` + `fileServerRemote.Stat` (guard
      `Offset > 0 && Offset < size`). See DECISION.md §D8a-fix. Verified by
      reproducing then re-running `TestE2E_DigestMismatchOnPullResume`.
- [x] Orchestrator cleanup: removed a dead `iter` import + dummy var the test
      agent left in `commit_order_test.go`.
- [x] R2 empirical review: read `commit_order_test.go` (both commit-last
      invariants still genuinely asserted), the re-expressed fileserver
      `Stat`/`ListBlobsByImage` tests (substantive, not vacuous), confirmed
      deleted tests target removed functionality only.
- [x] R3 verification gates — ALL GREEN (orchestrator-run, not trusted from agents):
      - `go build ./...` ✅
      - `go vet ./...` ✅
      - `go test -skip '_Local$' ./...` ✅ (incl. `internal/integration` 37.8s)
      - `go test -run '_Local$' ./...` ✅ compiles; passes/skips cleanly, 0 fail
      - integration round-trips PASS on BOTH backends: TestE2E_Push/Pull,
        TestE2E_LocalDirRemote_PushPull, TestFileServer_PushMeta/MultiChunk/
        DryRunPush/InterruptResume/InterruptResumePull, TestE2E_ResumeFromPartialPush,
        TestE2E_DigestMismatchOnPullResume, TestE2E_DryRun — these exercise every
        high-risk path (NewImageView, Stat skip/partial, PutIndex/PutOciLayout
        commit split, ListBlobsByImage, dry-run probe, resume).
- [x] Final sweeps: go-check-outdated-patterns (clean — modern idioms inherited);
      go-review-checklist (ctx-first ✓, Oci/Index naming ✓; file-size: fileserver.go
      shrank 728→570, remaining length is cohesive single-type/verbatim-move —
      further splitting is out of scope for this refactor).

## STATUS: COMPLETE — plan implemented, all gates green.

## Notes / risks being watched
- High-risk semantic spots (review closely): fileserver `Stat` (chunk-state),
  fileserver commit-marker split, `NewImageView` manifest-size + chunkSize
  priming order, fs `PrepUpload` mkdir internalization, pull mkdir inline,
  `resolveRemoteHas` per-image rewrite.
- Watch for tests "weakened to pass" during R2 — verify removed/edited
  assertions are legitimate (ListImages/ListBlobs removal) not coverage loss.
