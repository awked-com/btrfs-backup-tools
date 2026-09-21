# Btrfs backup tools

Linux helpers for receiving Btrfs backups through a restricted SSH command,
checking received replicas, applying retention, and pausing transfers outside an
allowed window. The `backup` Go package exposes replica metadata and retention
logic for monitoring clients.

## Build

Install Go 1.25.8 or newer and the libbtrfsutil development headers and library
(`libbtrfsutil-dev` on Debian/Ubuntu). Then:

```sh
go install github.com/awked-com/btrfs-backup-tools/cmd/...@latest
```

Build with CGO enabled for the Btrfs backend. The commands target Linux; portable
library tests also run on macOS. Runtime dependencies include Btrfs tools, systemd
with cgroup v2 device filtering, sudo, mbuffer, and util-linux (`prlimit`).

## Commands

- `backup-info --config receiver.json COMMAND` validates and executes a supported
  metadata query under the configured backup root.
- `backup-confined-receive --config receiver.json TARGET` receives stdin into a
  temporary directory under a systemd scope with device and resource limits,
  then publishes validated read-only replicas into an assigned target.
- `backup-ssh-filter --config receiver.json --info INFO --receive RECEIVE --buffer MBUFFER --limiter PRLIMIT [--window WINDOW] [--sudo SUDO]`
  validates `SSH_ORIGINAL_COMMAND` and dispatches supported metadata and receive
  operations. INFO and RECEIVE are administrator-owned wrappers supplying the
  fixed configuration.
- `backup-retention --config retention.json` prints the proposed retention actions.
  Add `--apply` to delete eligible replicas, with identity and read-only checks
  repeated before deletion.
- `backup-window run WINDOW COMMAND -- [ARGS...]` supervises a transfer command.
  `backup-window control WINDOW` refreshes registered transfers. WINDOW is an
  executable that exits zero when transfers are allowed and nonzero otherwise.

The window supervisor uses `/run/btrfs-backup-window`; create it for the service
account before running. Keep all helpers together in the same `bin` directory.

## Configuration

Example receiver configuration (adjust paths and limits for your machine):

```json
{
  "root": "/backups",
  "directories": ["/backups/example"],
  "staging": "/backups/.incoming",
  "command_lock": "/run/backup/example-command.lock",
  "receive_lock": "/run/backup/example-receive.lock",
  "directory_locks": {"/backups/example": "/run/backup/example-directory.lock"},
  "limits": {
    "memory_bytes": 1073741824,
    "tasks": 64,
    "wall_seconds": 86400,
    "active_seconds": 14400,
    "idle_seconds": 300
  },
  "transfer_window": null,
  "btrfs": "/usr/bin/btrfs",
  "systemd_run": "/usr/bin/systemd-run"
}
```

The backup root must be a mount point. Provision the target, staging, and lock
parent directories before use; staging and targets must share a Btrfs filesystem.
Use administrator-controlled configuration, wrappers, and directory ancestors.
Configure SSH forced commands and narrow passwordless sudo rules for the fixed
INFO and RECEIVE wrappers. Do not grant the sender arbitrary root commands or
allow it to choose a receiver configuration. `--sudo` defaults to PATH lookup;
pass an absolute path in service configuration.

Example retention configuration:

```json
{
  "root": "/backups",
  "directories": ["/backups/example"],
  "directory_locks": {"/backups/example": "/run/backup/example-directory.lock"},
  "retention": {"minimum_days": 2, "days": 14, "weeks": 8, "months": 12}
}
```

Retention preserves the newest replica, recent arrivals, and daily/weekly/monthly
buckets. Only validated received read-only subvolumes are eligible. Failed receive
staging is retained for inspection; the tools report its location. Inspect it and
confirm the receiver is inactive before manually removing failed state.

## Development

Run `go test ./...` and `go vet ./...`. Linux CI additionally runs the race detector
on x86_64 and aarch64, including transfer-window process control tests. Tests cover
command rejection, malformed configuration, confinement arguments, interrupted
receives, changed replicas, and retention. No infrastructure repository, hosts,
credentials, or NixOS configuration are needed. Version tags use `vX.Y.Z`.
