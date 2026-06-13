# oci-image-copy — Improvement Plan 01

Scope: module `github.com/ngicks/oci-image-copy` (workspace member `oci-image-copy`).
Origin: design review (multi-agent, supervisor-verified against source on 2026-06-13).
Baseline: all tests green before work started.

## Entry O1 — Verify manifest bytes against the descriptor digest (trust root)

Evidence (verified):
- `pkg/ocidir/dir.go:236-269`: `ReadManifest` reads the manifest blob by digest,
  parses, and returns WITHOUT recomputing the digest. Every downstream digest
  (config, layers) comes out of this unverified manifest. Blob transfers on pull
  ARE verified (`transfer.go` sha256 PreCommitHook) — the manifest is the one
  blindly-trusted content-addressed read. Same pattern in
  `fileserver_remote.go` manifest reads.

Change: hash-while-reading in `ReadManifest` via `mDesc.Digest.Verifier()`;
error on mismatch. Shared verify helper for the fileserver manifest-read paths.
Verification: unit test corrupting one manifest byte (expect digest-mismatch
error); ocidir + imagecopy tests green.
Size: S-M.

## Entry O2 — Bounds-check index manifests; single ParseIndex choke point

Evidence (verified):
- `pkg/ocidir/dir.go:229` and `pkg/imagecopy/remote.go:422` index
  `idx.Manifests[0]` unguarded. `pkg/imagecopy/fileserver/meta.go` unmarshals
  index.json WITHOUT `ValidateIndex`, so a peer serving `{"manifests":[]}`
  panics `ReadManifest` — remotely-triggerable crash. Index read+parse+validate
  is copy-pasted in 3 packages and the copies disagree (the meta.go copy omits
  validation). `ocidir/manifest.go` doc references a nonexistent `[ParseIndex]`.

Change: add `ocidir.ParseIndex`/`ParseImageLayout` (unmarshal+validate,
mirroring `ParseManifest`); route `FsDir.Index`, `sharedDir.Index`, and
fileserver meta through them; in `ReadManifest` explicitly error on empty or
multi-manifest index and nested-index mediaType (documented single-manifest
contract; README already lists no-multi-arch as a limitation); reuse
`ReadManifest` from `remote.go`'s InspectImage instead of its duplicate walk.
Verification: empty-index test (error, not panic), 2-entry index test; ocidir,
imagecopy, fileserver tests green.
Size: S-M.

## Entry O3 — SSH command path: cancellation, BatchMode, single Output implementation

Evidence (verified):
- `pkg/cli/invoker.go:173-179`: `exec.CommandContext(ctx, "ssh", ...)` kills
  only the local ssh on cancel; the remote skopeo keeps running and mutating
  the peer. No `cmd.Cancel`/`WaitDelay`, no ServerAlive options.
- Only `ssh.Probe` (`ssh/ssh.go:145`) uses `-o BatchMode=yes`; the command path
  omits it (and `-n`), so a host passing Probe can still hang on a prompt.
- `localCmd.Output` and `sshCmd.Output` are near-identical copies.

Change: shared ssh arg helper (`-n -o BatchMode=yes` + ServerAliveInterval/
CountMax); set `cmd.Cancel` (signal) + `cmd.WaitDelay`; collapse the two Output
copies into one captured-run helper; correct the stale cleanup claim in the
invoker doc comment.
Verification: argv-level tests asserting the flags; new shellQuote-adjacent
invoker tests; integration suite green.
Size: M.

## Entry O4 — Honest remote-blob presence: digest algorithm in chunk names; error taxonomy for probes

Evidence (verified):
- `pkg/imagecopy/transfer.go:263-274`: push skips upload when `sink.State`
  reports Complete — for fileserver that is size-only (per-chunk HEAD
  Content-Length), so a right-sized wrong-content remote blob is never
  re-uploaded; pull has a whole-blob sha256 backstop, push has none.
- `fileserver/naming.go` hardcodes `blobs/sha256/` discarding the digest
  algorithm.
- `fileserver_remote.go` probe paths (`ProbeBlob`/`PrimeRefs`) swallow ALL
  errors as "absent", so auth failures (401/5xx) report as "would send
  everything"; top-level `Blob` returns chunk-0 only for multi-chunk blobs
  (latent interface landmine).

Change: include the digest algorithm in chunk object names (wire-format change,
acceptable pre-1.0, documented); distinguish `fs.ErrNotExist` from transport/
auth errors in probe paths (propagate the latter); make multi-chunk top-level
`Blob` return an explicit unsupported error instead of truncating; document
that push trusts remote chunk integrity by size (verify flag deferred — see
DECISION.md).
Verification: naming tests for new key format; probe-error test (5xx ≠ absent);
fileserver + imagecopy tests green.
Size: M.

## Entry O5 — imageref: validate host/path/tag (defense-in-depth for path layout)

Evidence (verified):
- `pkg/imageref/imageref.go:136-146`: path segments checked only for empty +
  ReservedSegments (`_tags`,`_digests`); `.` and `..` pass. Tag accepted
  verbatim (`:99-105`). Host content never validated — and
  `looksLikeHost("..")` is TRUE (contains '.'), so `../../x:latest` parses with
  host `..`. These fields flow into `filepath.Join(Base, Host, ..., Tag)`
  (`paths.go`) with vroot as the only backstop. Host not lowercased, so
  `DOCKER.IO/nginx` skips library canonicalization.

Change: reject `.`/`..`/separator/control chars in path segments; tag grammar
`[A-Za-z0-9_][A-Za-z0-9._-]{0,127}`; validate host shape and lowercase it
before DefaultRegistry comparison; shared validation used by Parse (write side)
and `parseDumpDirRel` (read side, `remote.go:517-537`).
Verification: table-driven Parse tests (traversal refs, slash-in-tag, overlong
tag, uppercase host); existing imageref tests green.
Size: S.

## Entry O6 — CLI wrapper dedup; shellQuote test coverage; header redaction

Evidence (verified):
- `docker.go`/`podman.go`/`skopeo.go` triplicate `Exe`/`exe()`/`Version()`;
  exec errors returned bare while parse errors are wrapped.
- `shellQuote` (`invoker.go:212-223`) — the remote-argv escaper everything
  depends on — has zero direct tests; no test exercises `sshCmd` at all.
- Malformed `--remote-header` error echoes the raw header value
  (`fileserver_remote.go:124-129`) instead of using `RedactHeader`.

Change: small embeddable tool base (Invoker+Exe+Version) for the three
wrappers; wrap exec errors with operation context at the wrapper boundary;
shellQuote table test (quotes, `$()`, `;`, space, newline, `*`, empty) +
documented literal-argv guarantee; redact the malformed-header error.
Verification: new cli tests; full module tests green.
Size: S-M.

## Noted, not planned
- `Cmd.Run()` buffers stdout fully / no streaming `Stdout` seam, double-`Output`
  re-runs the process: real design wart, deferred — touches the Invoker
  interface used by all wrappers; revisit when a streaming need (e.g.
  `--password-stdin`) lands.
- `blobsFromMeta` last-writer-wins chunkSize conflict: detect/refuse deferred
  with the same fileserver-meta revisit.
