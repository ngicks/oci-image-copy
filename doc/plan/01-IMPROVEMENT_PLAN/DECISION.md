# oci-image-copy — Improvement Plan 01 — Decisions

The user was unavailable during this work; decisions below were made by the
supervising agent on its own judgment, with multi-agent review evidence and
(where noted) codex consulted as an advisor.

(Shared decisions D0/D1 — workspace scope and file naming — recorded in
`go-fsys-helper/doc/stream/plan/01-IMPROVEMENT_PLAN/DECISION.md`.)

## D12 — Single-manifest index contract enforced with explicit errors
`ReadManifest` errors on empty, multi-entry, or nested-index index.json instead
of silently taking `[0]`. Multi-arch fan-out stays unsupported (matches the
README's stated limitation); erroring is honest, silent `[0]` is not.

## D13 — Fileserver chunk-name wire format change accepted
Chunk object keys gain the digest algorithm segment. This invalidates existing
fileserver stores; accepted because the project is pre-1.0 WIP and an
address that drops the algorithm can collide across algorithms.

## D14 — Push-side content verification flag deferred
Push continues to trust remote chunk presence by size, now documented loudly,
with probe-path error taxonomy fixed (transport/auth errors no longer read as
"absent"). A `--verify`-style push flag is a feature, not a refactor; deferred.

## D15 — Invoker streaming redesign deferred
`Cmd.Run()` buffering, double-Output re-run semantics, and a streaming
`Stdout`/stdin seam (needed for `--password-stdin`) are real design warts but
require an Invoker interface change across all wrappers; deferred to a
dedicated plan.

## D16 — SSH remote-kill mechanism
Chosen: `-n -o BatchMode=yes` + ServerAlive keepalives + `cmd.Cancel`
(SIGTERM→WaitDelay) on the local ssh. Full remote-process-group kill (e.g.
`ssh -tt` or remote wrapper scripts) rejected: -tt corrupts binary stdio and
wrapper scripts are invasive; channel-drop teardown via keepalives is the
standard OpenSSH-compatible approach.
