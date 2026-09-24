<!--
  - SPDX-FileCopyrightText: 2026 Nextcloud contributors
  - SPDX-License-Identifier: AGPL-3.0-or-later
-->
# Task Processing Utility

Auto-scales Nextcloud Task Processing workers based on the live task queue, between an admin-configurable **minimum** and **maximum** number of workers. Tasks of every type are scheduled fairly so a flood of one type cannot starve the others.

This implements the pattern described in the Nextcloud admin manual — [*Improve AI task pickup speed*](https://docs.nextcloud.com/server/latest/admin_manual/ai/overview.html#improve-ai-task-pickup-speed) — with dynamic, queue-driven scaling instead of a fixed worker count.

## How it works

```
┌──────────────────────┐        direct DB reads (config.php)      ┌─────────────────────┐
│   Go supervisor      │  ← task queue depth per task type        │   Nextcloud server  │
│   (persistent svc)   │  ← admin config from oc_appconfig        │                     │
│                      │                                          │   (PHP + DB)        │
│  poll → schedule     │                                          │                     │
│       → reconcile ──▶│  spawn: occ taskprocessing-util:run      │                     │
└──────────────────────┘    --taskType <id> --timeout <N>  ──────▶└─────────────────────┘
```

**Go supervisor** (`supervisor/`, ships as arch-specific binaries):  
Cross-compiled for `amd64`, `arm64`, and `arm`. The binaries land in `bin/<arch>/task_proc_util-supervisor`. A shell wrapper at `bin/task_proc_util-supervisor` reads `uname -m`, maps it to the matching subdirectory, and `exec`s the correct binary — so the service `ExecStart` path is always the same regardless of host architecture. On every poll tick it:

1. re-reads the admin config (min/max workers, poll interval, enabled flag) — changes take effect without a restart,
2. lists available task types and reads each type's queue depth directly from the database,
3. computes a fair per-type worker allocation:
   - every type with pending work gets at least one slot (starvation prevention),
   - remaining capacity is distributed proportionally to queue depth (largest-remainder rounding),
   - total workers are capped between min and max, and never exceed the number of pending tasks,
4. reconciles the pool of running worker subprocesses to match the plan.

**PHP worker command** (`occ taskprocessing-util:run --taskType <id>`):  
A long-lived worker pinned to one task type. It bootstraps Nextcloud once then drains tasks of its assigned type, and recycles on `--timeout` so provider and config changes are picked up regularly.

Each task is claimed atomically so that the many workers running in parallel never double-process the same task:

- on **Nextcloud 35+** via `claimNextScheduledTask()`, a single `SELECT ... FOR UPDATE SKIP LOCKED` that both selects the task and marks it `RUNNING`;
- on **Nextcloud 33/34** via `getNextScheduledTask()` + `lockTask()`, skipping tasks another worker locked first.

**Shutdown and timeouts.** The supervisor sends `SIGTERM` to the worker's process group on scale-down or shutdown; the worker finishes its current task, then exits. If a worker overruns its `--timeout` — normally because a provider call is not returning — the supervisor escalates to `SIGKILL` after a grace period and marks the orphaned task `FAILED`, so it never sits in `RUNNING` forever.

**Admin settings** appear under **Administration → Artificial Intelligence**.

## Requirements

- Nextcloud 33 or later
- The supervisor binary running as a persistent service (e.g. systemd), on the same host as Nextcloud and as the same user
- Read access to `config.php` (no app password or HTTP credentials required)

## Configuration

All settings are available in the **AI** admin section:

| Setting         | Default | Meaning                                                   |
|-----------------|---------|-----------------------------------------------------------|
| Enable          | on      | Master switch; when off, all workers are drained.         |
| Minimum workers | 1       | Workers kept alive while there is pending work.           |
| Maximum workers | 4       | Upper bound on concurrent workers across all task types.  |
| Poll interval   | 10 s    | How often the supervisor checks the queue.                |

## Running the supervisor

The supervisor is a long-lived daemon. It reads Nextcloud's `config.php` directly — no app password or HTTP credentials needed. It runs on the same host as Nextcloud, as the same user (`www-data`), exactly like `notify_push`.

### Systemd example

```ini
[Unit]
Description=Nextcloud Task Processing Utility supervisor
After=network.target

[Service]
User=www-data
Environment=NC_OCC=php /var/www/html/occ
Environment=NC_WORKER_TIMEOUT=300
ExecStart=/var/www/html/apps/task_proc_util/bin/task_proc_util-supervisor /var/www/html/config/config.php
# ^ shell wrapper; auto-selects bin/amd64|arm64|arm/task_proc_util-supervisor via `uname -m`
Restart=always
RestartSec=5
# Workers are given a grace period to finish the task in flight on shutdown.
# The default (90s) would SIGKILL the supervisor first, skipping the cleanup
# that marks interrupted tasks as failed.
TimeoutStopSec=3600

[Install]
WantedBy=multi-user.target
```

### Flags (alternative to env vars)

| Positional / Flag | Env var | Default | Description |
|-------------------|---------|---------|-------------|
| `config.php` (positional) | `NC_CONFIG_FILE` | — | Path to Nextcloud's `config.php` (required unless overriding all values) |
| `--occ` | `NC_OCC` | `php occ` | Command to invoke occ (space-separated) |
| `--occ-dir` | `NC_OCC_DIR` | derived from `config.php` | Nextcloud root to run occ from. `occ` refuses to start anywhere else. |
| `--worker-timeout` | `NC_WORKER_TIMEOUT` | `300` | Worker recycle interval in seconds (0 = never). A worker that overruns this is force-killed after a 30 minute grace period. |
| `--database-url` | `DATABASE_URL` | — | Override DB connection (e.g. `mysql://user:pass@host/db`) |
| `--database-prefix` | `DATABASE_PREFIX` | — | Override table prefix |
| `--nextcloud-url` | `NEXTCLOUD_URL` | — | Override Nextcloud URL |

Supported databases: **MySQL / MariaDB**, **PostgreSQL**, **SQLite3**. Oracle is not supported.

## Development

```sh
# Go supervisor
make supervisor          # build for host arch → bin/<arch>/task_proc_util-supervisor
make supervisor-release  # cross-compile for amd64, arm64, arm
make supervisor-test     # go vet + go test ./...

# Frontend
make frontend            # npm ci && npm run build → js/

# Full release build (all of the above + composer)
make build
```
