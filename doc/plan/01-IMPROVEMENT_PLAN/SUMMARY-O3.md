# SUMMARY — O3: SSH command path — cancellation, BatchMode, single Output

## What changed

The one-shot remote command path (`sshCmd`) had three gaps: it used
`exec.CommandContext` whose default cancel SIGKILLs only the local ssh (leaving
the remote skopeo running and mutating the peer); it omitted `-n` and
`-o BatchMode=yes`, so a host that passed `ssh.Probe` could still hang the
command path on an interactive prompt; and `localCmd.Output` / `sshCmd.Output`
were near-identical copies.

- `pkg/cli/ssh/ssh.go`
  - Added `CommandArgs(Target) []string`: `BinaryArgs` + `-n`,
    `-o BatchMode=yes`, and `-o ServerAliveInterval=15 -o ServerAliveCountMax=3`
    keepalives. Exposed `ServerAliveInterval` / `ServerAliveCountMax` consts.
    These bound how long the local ssh waits on a silent channel; a dead
    peer/network tears the channel down and the remote sshd reaps the orphaned
    remote process (decision D16: no `-tt`, no remote wrapper scripts).

- `pkg/cli/invoker.go`
  - Collapsed the two `Output` copies into a shared `runCaptured(ctx, cmd,
    redactedArgv, tailBytes, stderrMsg, extraAttrs...)` capture-and-classify
    helper; both `localCmd.Output` and `sshCmd.Output` now call it.
  - `sshCmd.Output` builds args via `ssh.CommandArgs`, and sets
    `cmd.Cancel` (SIGTERM the local ssh — a clean channel teardown the remote
    sshd notices) + `cmd.WaitDelay = SshWaitDelay` (SIGKILL backstop). Added
    the `SshWaitDelay` var.
  - Corrected the `SshInvoker` / `sshCmd.Output` doc comments to describe the
    actual SIGTERM→WaitDelay + keepalive teardown behavior (was silent on
    cancellation before).

## Tests added

- `pkg/cli/ssh/ssh_test.go`: `TestCommandArgs_Flags` (asserts `-n`,
  `BatchMode=yes`, ServerAlive keepalives present), `TestCommandArgs_PreservesTarget`.
- `pkg/cli/runner_test.go` (no test exercised `sshCmd` before):
  - `TestSshCmd_OutputAndArgs` — fake `ssh` shim records argv; asserts the
    non-interactive flags + the shellQuote'd remote word after `--` reach ssh,
    and stdout round-trips.
  - `TestSshCmd_Cancel` — a sleeping shim is torn down on ctx cancellation and
    Output returns promptly with an error (not after the full sleep).

## Test evidence

- `go build ./...` — ok
- `go vet ./...` — ok
- `go test ./pkg/... ./cmd/...` — all ok
- New O3 tests — PASS (`TestSshCmd_Cancel` ~2.1s, the WaitDelay window)
- `go test ./internal/integration/` — ok (36.09s; in-process ssh server
  accepts the new flags — this path drives sshCmd against a real sshd)

## Cross-agent workspace note (not a regression)

Mid-entry, the workspace member `go-fsys-helper/fsutil` changed
`SafeWriteOption.Copy` from 6 args to 4, which transiently broke
`pkg/imagecopy/fsocidirs.go` (a file another agent owns and I must not touch).
The breakage existed independently of my O3 changes (confirmed by stashing) and
was fixed by that agent updating the `fsocidirs.go` call site. I did not stage
or modify `fsocidirs.go`. After the fix, `go build/vet/test ./...` is green.

## Deviations from plan

None. (The dedicated shellQuote table test is part of O6 per the plan; O3's
sshCmd tests exercise shellQuote indirectly via the recorded remote word.)
