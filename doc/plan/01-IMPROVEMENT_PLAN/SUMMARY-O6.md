# SUMMARY — O6: CLI wrapper dedup; shellQuote test coverage; header redaction

## What changed

`docker.go` / `podman.go` / `skopeo.go` triplicated the `Invoker` field, the
`Exe`/`exe()` accessor, and `Version`; exec errors were returned bare while parse
errors were wrapped. `shellQuote` — the remote-argv escaper everything depends on
— had zero direct tests, and no test exercised `sshCmd` (the latter added in O3).
A malformed `--remote-header` error echoed the raw header value (a possible
secret).

- `pkg/cli/tool.go` (new) — embeddable `Tool` base carrying `Invoker`, `Name`,
  `DefaultExe`, with shared `Exe()`, `Version()`, and `Run`/`Output` helpers that
  wrap a non-zero exit with operation context (`<exe> <args>: <err>`) at the
  wrapper boundary.

- `pkg/cli/docker/docker.go`, `pkg/cli/docker/podman.go`,
  `pkg/cli/skopeo/skopeo.go` — each wrapper now embeds `cli.Tool` (dropping its
  own Invoker/Exe/Version copies) and routes its exec calls through the embedded
  `Output`/`Run`, so exec errors are wrapped with context. Added `NewSkopeo`
  constructor (Docker/Podman already had constructors). Skopeo keeps its
  compression fields. Removed now-unused `strings` imports.

- `pkg/imagecopy/local.go` — `newSkopeoWithCompression` builds via `NewSkopeo`
  then sets the compression fields (the embedded `Invoker` is no longer a direct
  struct field).

- `pkg/cli/invoker.go` — documented `shellQuote`'s literal-argv guarantee (every
  token reaches the remote `sh -c` as one literal word; no split/expand/glob/
  command-termination; `'\''` for a literal quote).

- `pkg/imagecopy/fileserver_remote.go` — the malformed-`--remote-header` error in
  `NewFileServerRemoteFromSpec` now reports `RedactHeader(h)` instead of the raw
  value.

## Tests added

- `pkg/cli/runner_test.go`: `TestShellQuote` — table test covering quotes,
  `$()`, `;`, space, newline, `*`, `|`/`&`, empty token, empty argv, and a
  literal single quote (`'\''`).
- `pkg/imagecopy/fileserver_remote_test.go`:
  `TestNewFileServerRemoteFromSpec_MalformedHeaderRedacted` — a colon-less header
  (a bare-token secret) is rejected with the value redacted, not echoed.
- Existing wrapper tests updated: `skopeo_test.go::newSkopeo` now uses
  `NewSkopeo`. The `PropagatesRunnerError` tests still pass (the wrapping uses
  `%w`, so `errors.Is` holds).

## Test evidence

- `go build ./...` — ok
- `go vet ./...` — ok
- `go test ./pkg/... ./cmd/...` — all ok
- New O6 tests + all wrapper tests — PASS
- `go test ./internal/integration/` — ok (36.61s)

## Deviations from plan

None.
