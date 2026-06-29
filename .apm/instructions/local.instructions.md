---
description: "Basic instructions for my env"
applyTo: "*"
---

# Local-only test convention

Some Go tests in this repo cannot run on CI — they need locally-dumped
testdata, a running daemon, network access, or a privileged binary
(skopeo / podman / docker / ssh). Mark every such test with the
`_Local` suffix on its function name so CI can filter it out
mechanically.

## Rule

A test or sub-test that should be skipped in CI **must** end its
function name (or the leaf name of a `t.Run` invocation) with `_Local`.

```go
// runs in CI
func TestParseManifest_OCI(t *testing.T) { ... }

// CI-skipped: needs internal/testdata/ocidir/ populated by `go generate`
func TestReadManifest_Local(t *testing.T) { ... }
```

When a single `Test*` function mixes CI-safe and local cases, split
them into separate functions rather than relying on `t.Run` name
filtering — the suffix lives at the top level so `go test -skip` can
match it without per-package configuration.

## CI invocation

CI runs:

```sh
go test -skip '_Local$' ./...
```

The `-skip` flag (Go ≥ 1.20) takes a regex matched against the
slash-joined test name; `_Local$` matches both `TestFoo_Local` and
`TestFoo/sub_Local`.

## Local invocation

To run only the local-skipped tests:

```sh
go test -run '_Local$' ./...
```

To run everything (default):

```sh
go test ./...
```

A `_Local` test should `t.Skip(...)` cleanly when its prerequisites
(testdata, daemon, etc.) are absent so a developer who hasn't set them
up still sees a green build.
