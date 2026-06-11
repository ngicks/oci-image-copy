# oci-image-copy

Share OCI images between two hosts efficiently — without standing up a registry —
by driving `skopeo` + `podman`/`docker` over an SSH connection and shipping
only the blob digests the peer doesn't already have.

Module: `github.com/ngicks/oci-image-copy`
Binary: `oci-image-copy` (subcommands: `push`, `pull`, `dump`)

## How it works

At a high level, this tool uses an OCI image layout as the exchange format.
The source and destination can still be normal local transports like
`containers-storage:` or `docker-daemon:`; the OCI directory is just the
synchronized staging format between the two hosts.

```text
LOCAL

    .-----------------------.     skopeo copy      +-----------------------------+
   / containers-storage:   /|  -----------------> | <base>/                     |
  / docker-daemon:        / |                     |   <host>/<repo-path>/       |
 / oci:                 /  |                     |     _tags/<tag>/            |
+-----------------------+   |                     |     _digests/<digest>/      |
| local image store     |  /                      |   share/sha256/<blob>       |
|                       | /                       +-----------------------------+
+-----------------------+/

                    sync missing manifests/configs/layers
    -------------------------------------------------------------------->

REMOTE

    +-----------------------------+      skopeo copy     .-----------------------.
    | <base>/                     |  -----------------> / containers-storage:   /|
    |   <host>/<repo-path>/       |                    / docker-daemon:        / |
    |     _tags/<tag>/            |                   / oci:                 /  |
    |     _digests/<digest>/      |                  +-----------------------+   |
    |   share/sha256/<blob>       |                  | remote image store    |  /
    +-----------------------------+                  |                       | /
                                                     +-----------------------+/
```

The remote side gets the same OCI directory structure as the local staging
side: per-image tag directories under `<base>/<host>/<repo-path>/_tags/` and
shared content-addressed blobs under `<base>/share/sha256/`. After sync, the
remote loads from its local OCI directory into the requested remote transport.

Each invocation:

1. Connects to a peer over SSH (using the system `ssh` binary — auth,
   host-key verification, `ProxyCommand`, and `Include` all flow through the
   user's `~/.ssh/config`).
2. Enumerates the peer's blob inventory by walking `share/sha256/` on the
   remote over SFTP.
3. Locally dumps each requested image from the configured local transport to
   an `oci:` layout with a shared blob pool (`<base>/share/`) via
   `skopeo copy --preserve-digests` using the form
   `oci:<tagDir>:<imageRef>`.
4. Diffs the digest closure against the peer's inventory; ships
   `manifest + config` blobs unconditionally and any layer blobs the
   peer is missing. Each blob transfer uses **resumable primitives from
   `github.com/ngicks/go-fsys-helper/fsutil`**:
   - A `.part` work-in-progress file is created next to the destination.
   - A `.part.etag` sidecar holds the blob's content-identity token (the
     digest string) so a resumed transfer can detect if the content changed.
   - On pull, a `sha256` pre-commit hook verifies the downloaded blob against
     the OCI digest before the atomic rename; on mismatch the `.part` and
     `.part.etag` sidecars are removed so the next attempt restarts clean.
   - On push, the remote sink reports its current offset via `State()` so
     the local side resumes from that position.
   - `ErrContentChanged` from `fsutil` is non-retryable.
5. Tells the peer to load from the synchronized OCI tag directory into its
   target transport via `skopeo copy oci:<tagDir>:<ref> <transport>:<ref>`
   over the same SSH session. No-op when the remote transport is `oci`.

`--dry-run` replaces every mutating step (local dump, network transfer,
peer load) with read-only equivalents and emits a plan instead of touching
state.

## Requirements

| Side   | Required                                                                        |
| ------ | ------------------------------------------------------------------------------- |
| Local  | `skopeo` v1.16+; `podman` if using `containers-storage`, `docker` for `docker-daemon` |
| Remote | `skopeo` v1.16+; same container runtime as `--remote-transport`; `sshd` with SFTP subsystem enabled |
| SSH    | OpenSSH client on local; `sshd` on remote with the `sftp` subsystem (e.g. `Subsystem sftp /usr/lib/openssh/sftp-server` in `sshd_config`) |

The minimum `skopeo` version must support `--shared-blob-dir` /
`--src-shared-blob-dir` / `--dest-shared-blob-dir` and
`--preserve-digests`. v1.16+ is known good.

The peer's data dir defaults to
`${XDG_DATA_HOME:-$HOME/.local/share}/oci-image-copy/` (resolved on the
remote via SSH). Override with `--remote-transport=oci --remote-path=<dir>`.

## CLI reference

### Common flags (push / pull)

| Flag | Default | Description |
| ---- | ------- | ----------- |
| `--local-transport` | `containers-storage` | Source/destination transport on the local side. One of: `containers-storage`, `docker-daemon`, `oci` — plus `docker` (registry) for `push`/`dump` only, since a registry cannot be enumerated or loaded into. |
| `--local-path` | | Absolute path for `--local-transport=oci`. |
| `--local-dumpdir` | `~/.local/share/oci-image-copy` | Base of the local on-disk OCI store layout. |
| `--remote-transport` | `containers-storage` | Transport on the remote side. Same allowed values as `--local-transport`. |
| `--remote-path` | | Absolute path on the remote for `--remote-transport=oci`. |
| `--remote-name` | | SSH config destination name (mutually exclusive with `--remote-host/user/port`). |
| `--remote-host` | | Remote SSH hostname or address. |
| `--remote-user` | | Remote SSH user (optional). |
| `--remote-port` | `0` | Remote SSH port (0 = ssh default / config). |
| `--dry-run` | `false` | No mutation; emit a plan instead. |
| `--keep-going` | `false` | Continue on per-image failure. |

### `push IMAGE [IMAGE...]`

Push images from the local transport to the remote peer.

Additional flag: `--assume-remote-has <digest,...>` — skip remote enumeration
when the caller already knows the peer's blob inventory.

### `pull IMAGE [IMAGE...]`

Pull images from the remote peer into the local transport. The image must
already exist in the peer's OCI mirror (use `dump` or a prior `push` from the
other side to populate it).

Additional flag: `--assume-local-has <digest,...>` — skip local enumeration.

### `dump IMAGE [IMAGE...]`

Dump local images into the on-disk OCI store layout only (no SSH). Useful for
pre-populating the store before a `push`, or for inspecting the layout.

Flags: `--local-transport`, `--local-path`, `--local-dumpdir`.

## Transport validation

Unsupported `--local-transport` and `--remote-transport` values are rejected
at CLI startup with a clear error message listing the supported set:
`containers-storage`, `docker-daemon`, `oci` everywhere, plus `docker`
(registry) where the local side only acts as a source (`push`, `dump`).

## Known limitations

- No concurrent invocations against the same `<base>/` (no locking on
  `share/`, `.part`, or tag dirs).
- No `--all-platforms` fan-out from `oci:` index sources.
- `pull` assumes the image already exists in the peer's OCI mirror; it
  does not trigger a remote-side `skopeo copy` from live storage. Use `push`
  from the other host, or `dump` on the remote host first.
- No `~/.ssh/config` parsing in-process. The connectivity probe shells out to
  `ssh -G <host>` so config-derived `ProxyJump`/`Include` paths work for the
  probe, but the in-process SFTP dial delegates to the same system `ssh`
  binary — host-key verification, auth, and `ProxyCommand` all flow through
  the user's normal SSH config.

## Integration tests

A hermetic end-to-end integration suite lives at `internal/integration/`.
It requires `skopeo` and the system `ssh` binary; it is skipped automatically
when either is absent.

The suite spins up an **in-process SSH+SFTP server** (no system `sshd` or
daemon required) using `golang.org/x/crypto/ssh` + `github.com/pkg/sftp`.
Each test uses an isolated `ssh_config` (written to a temp dir) via
`ssh.Target.ConfigFile` (`-F`), so there is no dependency on the user's
`~/.ssh` directory.

### Scenarios covered

| Test | What it verifies |
| ---- | ---------------- |
| `TestE2E_Push` | First push sends all blobs; second push reuses them all. |
| `TestE2E_Pull` | Round-trip push-then-pull; `skopeo inspect` validates the result. |
| `TestE2E_DryRun` | Dry-run push leaves the remote filesystem completely unchanged. |
| `TestE2E_ResumeFromPartialPush` | Pre-seeded `.part` + `.part.etag` on remote is resumed, not restarted; sidecars removed on success. |
| `TestE2E_DigestMismatchOnPullResume` | Corrupt `.part` (same size, wrong bytes) on local triggers sha256 hook, cleans up sidecars, fails without committing. |
| `TestCLIBinary_Help` | CLI binary builds and `--help` lists `push`/`pull`/`dump`; invalid transport yields a clear error. |

### Running

```sh
# From the module root (requires skopeo and ssh on PATH):
go test ./internal/integration/

# Verbose:
go test -v ./internal/integration/
```

## Dev setup

The workspace root (`../`) contains a `go.work` that covers both this module
and the sibling `go-fsys-helper` checkout. Development-time `replace` directives
in `go.mod` point at `../go-fsys-helper/{fsutil,vroot,vroot-adapter/sftpfs}`:

```
replace (
    github.com/ngicks/go-fsys-helper/fsutil              => ../go-fsys-helper/fsutil
    github.com/ngicks/go-fsys-helper/vroot               => ../go-fsys-helper/vroot
    github.com/ngicks/go-fsys-helper/vroot-adapter/sftpfs => ../go-fsys-helper/vroot-adapter/sftpfs
)
```

These directives must be dropped before publishing tagged versions of
`go-fsys-helper` and this module.

```sh
# Build the binary locally:
go build ./cmd/oci-image-copy

# Run all unit tests (no skopeo/ssh required):
go test ./...

# Run integration tests (requires skopeo + ssh on PATH):
go test ./internal/integration/
```
