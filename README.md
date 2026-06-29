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
| Remote | `skopeo` v1.16+; same container runtime as the `--remote` transport spec; `sshd` with SFTP subsystem enabled (SSH remotes only) |
| SSH    | OpenSSH client on local; `sshd` on remote with the `sftp` subsystem (e.g. `Subsystem sftp /usr/lib/openssh/sftp-server` in `sshd_config`) |

The minimum `skopeo` version must support `--shared-blob-dir` /
`--src-shared-blob-dir` / `--dest-shared-blob-dir` and
`--preserve-digests`. v1.16+ is known good.

The peer's data dir defaults to
`${XDG_DATA_HOME:-$HOME/.local/share}/oci-image-copy/` (resolved on the
remote via SSH). Override with `--remote oci:/path/to/dir` for a pure OCI
directory remote, or `--remote ssh://host/oci:/srv/oci` to set a custom
remote path over SSH.

## CLI reference

### `--local <spec>` (push / pull / dump)

Specifies the local transport. Accepted forms:

| Spec | Transport | Notes |
| ---- | --------- | ----- |
| `containers-storage:` | containers-storage | Default. Bare name (`containers-storage`) also accepted. |
| `docker-daemon:` | docker-daemon | Bare name (`docker-daemon`) also accepted. |
| `oci:/path/to/dir` | oci (OCI layout dir) | Path is required. |
| `docker:` | docker (registry) | Push/dump source only; rejected for pull. Bare name (`docker`) also accepted. |

### `--remote <spec>` (push / pull)

Specifies the remote peer. Accepted forms:

| Spec | Kind | Notes |
| ---- | ---- | ----- |
| `ssh://[user@]host[:port][/<transport-spec>]` | SSH remote | `<transport-spec>` is `containers-storage:`, `docker-daemon:`, or `oci:/path`. Defaults to `containers-storage:`. |
| `file-server:<url>` | File-server remote | Fully implemented. Push/pull via chunked HTTP objects + per-image meta. `ListImages` is unsupported (no global index on the server). |
| `oci:/path/to/base/dir` | Local-directory remote | OCI store at a local path; no SSH. Useful for pre-staged directories. |

**SSH remote examples:**

```sh
# Default transport on remote (containers-storage):
--remote ssh://myhost

# Custom SSH user and port, oci: transport on remote:
--remote ssh://alice@myhost:2222/oci:/srv/oci-store

# docker-daemon: transport on remote:
--remote ssh://myhost/docker-daemon:
```

**Local-directory remote example** (no SSH, no daemon required):

```sh
--remote oci:/mnt/nfs/oci-pool
```

### Common flags (push / pull)

| Flag | Default | Description |
| ---- | ------- | ----------- |
| `--local` | `containers-storage:` | Local transport spec (see above). |
| `--remote` | _(required)_ | Remote peer spec (see above). |
| `--local-dumpdir` | `~/.local/share/oci-image-copy` | Base of the local on-disk OCI store layout. |
| `--dry-run` | `false` | No mutation; emit a plan instead. |
| `--keep-going` | `false` | Continue on per-image failure. |

### File-server companion flags

| Flag / Env var | Default | Description |
| -------------- | ------- | ----------- |
| `--remote-header 'Name: value'` | | Extra HTTP request header for file-server remote (repeatable). Values are redacted in logs. |
| `--remote-chunk-size` | `100MiB` | Upload chunk size (human-readable, e.g. `50MiB`). |
| `--remote-naming-prefix` | `""` | Naming convention prefix for file-server blobs. |
| `OCI_IMAGE_COPY_FILESERVER_AUTH` env var | | Sets the `Authorization` header value (e.g. `Bearer <token>`). An explicit `--remote-header 'Authorization: ...'` flag takes precedence. The value is never logged. |

**Push trusts remote chunk integrity by size.** On push, a blob is considered
already present on the file server when every expected chunk object exists with
the expected byte length (a per-chunk `HEAD` / `Content-Length` check); the
chunk *contents* are not re-hashed. A right-sized but wrong-content remote chunk
is therefore not re-uploaded. (Pull has a whole-blob sha256 backstop; push does
not.) Chunk object keys include the digest algorithm segment
(`blobs/<algo>/<hex>/<index>`), so addresses cannot collide across algorithms.
A failed presence probe — transport error, auth (401/403), or server error
(5xx) — is propagated, never read as "absent": it will not silently downgrade
into "the remote has nothing, send everything". A content-verifying push flag is
not yet implemented (a feature, deferred — see the plan's decision D14).

### `push IMAGE [IMAGE...]`

Push images from the local transport to the remote peer.

Additional flag: `--assume-remote-has <digest>` (repeatable) — skip remote
enumeration when the caller already knows the peer's blob inventory.

**Commit-last ordering:** blob data is fully transferred before tag-directory
metadata (manifests/configs) is written on the remote. This means a partial
push that is interrupted leaves no dangling references.

### `pull IMAGE [IMAGE...]`

Pull images from the remote peer into the local transport. Pull is
**one-shot**: for peers backed by a live container runtime
(`containers-storage`, `docker-daemon`), the peer first materializes the image
from its live storage into its own OCI mirror via a remote-side `skopeo copy`.
The orchestrator then diffs the digest closure and fetches only the missing
blobs.

For peers configured with an `oci:` transport (a pure OCI mirror — either via
`--remote ssh://host/oci:/path` or `--remote oci:/path`), the materialization
step is a no-op — the mirror already IS the store.

For read-only peers the materialization step is skipped with a warning; Pull
proceeds to read the peer's mirror directly. If the image is absent from the
mirror as well, Pull returns an error.

Additional flags:

- `--assume-local-has <digest>` (repeatable) — skip local enumeration.
- `--verify-reused-blobs` (default `false`) — sha256-verify locally
  pre-existing blobs before reusing them. By default a local blob file that
  already exists at the expected size is reused without re-reading it (a
  standard content-addressed-storage tradeoff: the path encodes the digest,
  and freshly downloaded data is always verified). Enable this to detect
  on-disk corruption; a mismatching blob is discarded and re-downloaded.

**Note:** `--local docker:` is rejected for pull (a Docker registry cannot be
enumerated or loaded into directly).

**Commit-last ordering:** blob data is fully written locally before local
tag-directory metadata is committed.

### `dump IMAGE [IMAGE...]`

Dump local images into the on-disk OCI store layout only (no SSH). Useful for
pre-populating the store before a `push`, or for inspecting the layout.

Flags: `--local` (default `containers-storage:`), `--local-dumpdir`.

## Transport validation

Unsupported `--local` specs are rejected at CLI startup with a clear error
message. `docker:` is accepted as a source for `push`/`dump` but rejected for
`pull`. SSH remote specs that specify `docker:` transport are also rejected.
Unknown spec prefixes produce an error listing the accepted forms.

## Known limitations

- No concurrent invocations against the same `<base>/` (no locking on
  `share/`, `.part`, or tag dirs).
- No `--all-platforms` fan-out from `oci:` index sources.
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
| `TestE2E_Pull_DumpImage_OciNoOp` | DumpImage is a no-op for oci-transport peers; pull succeeds and the remote store is not mutated. |
| `TestE2E_LocalDirRemote_PushPull` | Push to a local-directory remote (`--remote oci:/path`) then pull from it; no SSH daemon required. |
| `TestCLIBinary_Help` | CLI binary builds and `--help` lists `push`/`pull`/`dump`; invalid `--local` spec yields a clear error; file-server remote parses the URL and proceeds to the real implementation (no stub error). |

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
