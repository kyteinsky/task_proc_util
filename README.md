<!--
  - SPDX-FileCopyrightText: 2026 Nextcloud contributors
  - SPDX-License-Identifier: AGPL-3.0-or-later
-->
# Task Processing Utility

> [!WARNING]
> Disclaimer: This project has been vibe coded.

This application changes the number of Nextcloud Task Processing workers automatically. It calculates the number of workers from the number of tasks in the queue. The number of workers stays between a minimum value and a maximum value. The administrator sets these two values.

The application gives workers to all the task types. Thus a large number of tasks of one type does not prevent the operation of the other types.

The Nextcloud administrator manual gives this method in [*Improve AI task pickup speed*](https://docs.nextcloud.com/server/latest/admin_manual/ai/overview.html#improve-ai-task-pickup-speed). This application uses the same method. But it calculates the number of workers from the queue. It does not use a constant number of workers.

## How the Application Operates

```
┌──────────────────────┐        the supervisor reads config.php   ┌─────────────────────┐
│   Go supervisor      │  ← the number of tasks for each type     │   Nextcloud server  │
│   (service)          │  ← the configuration from oc_appconfig   │                     │
│                      │                                          │   (PHP and database)│
│  read → calculate    │                                          │                     │
│       → adjust ─────▶│  starts: occ taskprocessing-util:run     │                     │
└──────────────────────┘    --taskType <id> --timeout <N>  ──────▶└─────────────────────┘
```

### The Go Supervisor

The `supervisor/` directory contains the Go source code. The build makes one binary file for each computer architecture: `amd64`, `arm64`, and `arm`. The build puts each binary file in `bin/<arch>/task_proc_util-supervisor`.

A shell script at `bin/task_proc_util-supervisor` reads the architecture with `uname -m`. Then the script starts the correct binary file. Thus the `ExecStart` path in the service file is the same for all architectures.

The supervisor does these steps at each poll interval:

1. It reads the configuration of the administrator again. The configuration contains the maximum number of workers, the poll interval, and the enable switch. A change becomes effective immediately. A restart is not necessary.
2. It finds the task types that have tasks in the scheduled state or the running state. Then it reads the number of tasks of each of these types from the database.
3. It calculates the number of workers for each task type:
   - Each task type that has work gets a minimum of one worker. Thus all the task types that have work get a worker.
   - The supervisor divides the remaining workers in proportion to the quantity of work. It uses the largest-remainder method.
   - The total number of workers is not more than `max_workers`. It is also not more than the quantity of work.
4. It starts workers or stops workers. It continues until their number agrees with the calculation.

The **work** of a task type is the sum of two values:

- the tasks in the scheduled state,
- the tasks in the running state that the workers of this supervisor process now.

The supervisor counts the tasks that the workers process now. Thus it does not stop a worker during the operation on a task.

The supervisor does not count a task in the running state that has no worker. A task stays in this condition after a failure of a worker. A task also stays in this condition when an asynchronous provider outside this application controls it. These tasks must not keep workers permanently.

Before the supervisor starts the poll loop, it does the command `occ taskprocessing-util:run --help` one time. If `occ` does not operate, the supervisor shows the cause and stops. These are possible causes: an incorrect user, an incorrect directory, or a disabled application. This test prevents the start of workers that can only stop immediately.

### The PHP Worker Command

The command `occ taskprocessing-util:run --taskType <id>` starts one worker. Each worker processes only one task type.

The worker starts Nextcloud one time. Then it processes the tasks of its task type continuously. At the end of the `--timeout` interval, the worker stops and the supervisor starts a new worker. Thus the new worker gets the new provider data and the new configuration.

The supervisor starts each worker from the Nextcloud root directory, because `occ` does not operate in a different directory.

The worker uses only synchronous providers (`ISynchronousProvider`). A task type can have no preferred provider. A task type can also have an asynchronous provider. An asynchronous provider is an external application, for example `context_chat`, that has its own worker. In these two conditions the worker stops with exit code `3`. Then the supervisor does not start more workers for this task type.

The log file shows the cause one time:

```
level=WARN msg="task type has no preferred synchronous provider; not starting further workers for it"
  taskType=core:text2text stderr="No preferred synchronous provider for task type core:text2text; ..."
```

The supervisor examines these task types again when the administrator changes the configuration. Thus a new provider becomes effective without a restart of the supervisor.

Two or more workers operate at the same time. Each worker takes a task with one atomic operation. Thus two workers do not take the same task.

- On **Nextcloud 35 and subsequent versions**, the worker uses `claimNextScheduledTask()`. This function does one `SELECT ... FOR UPDATE SKIP LOCKED` operation. The operation selects the task and sets the state of the task to `RUNNING`.
- On **Nextcloud 33 and 34**, the worker uses `getNextScheduledTask()` and then `lockTask()`. The worker ignores the tasks that a different worker locked before.

### Stop and Timeout

The supervisor sends `SIGTERM` to the process group of the worker in two conditions: when it decreases the number of workers, and when it stops. The worker completes its current task and then stops.

A worker can continue for more than its `--timeout` interval. Usually this occurs because a provider does not give an answer. The supervisor waits 30 minutes and then sends `SIGKILL`. Then the supervisor sets the state of the task of that worker to `FAILED`. Thus the task does not stay in the `RUNNING` state permanently.

The supervisor changes the state of only the task of the worker that it stopped. The other tasks in the running state belong to the other workers of the same task type. The supervisor does not change these tasks.

The administrator settings are in **Administration → Artificial Intelligence**.

## Requirements

- Nextcloud 33 to 36.
- A minimum of one task type with a **synchronous** provider. An external application with an asynchronous provider has its own worker. Thus this application does not control it.
- The supervisor binary file that operates as a continuous service, for example with systemd. The service must operate on the same computer as Nextcloud. The service must operate as the user that owns `config.php`, because `occ` does not operate as a different user.
- Read access to `config.php`. An application password is not necessary. HTTP credentials are not necessary.
- Write access to the Nextcloud database. The supervisor sets the state of the task of a stopped worker to `FAILED`.

## Configuration

All the settings are in the **AI** section of the administrator settings.

| Setting         | Default | Name in the database | Function                                        |
|-----------------|---------|----------------------|-------------------------------------------------|
| Enable          | on      | `autoscale_enabled`  | The main switch. When it is off, all the workers stop. |
| Maximum workers | 4       | `max_workers`        | The maximum number of workers for all the task types together. |
| Poll interval   | 10 s    | `poll_interval`      | The interval between two examinations of the queue. |

The supervisor starts workers only for the task types that have work. When there is no work, the number of workers becomes zero. A task type gets no more workers than the number of tasks of that type. Therefore `max_workers` is the setting that controls the performance.

The switch has the name `autoscale_enabled` in the database. It does not have the name `enabled`, because Nextcloud uses `<appid>/enabled` for the state of the application.

## How to Start the Supervisor

The supervisor is a continuous service. It reads the Nextcloud `config.php` file directly. An application password is not necessary. HTTP credentials are not necessary. The service operates on the same computer as Nextcloud, and as the same user (`www-data`). The `notify_push` application uses the same method.

### Example systemd Unit

```ini
[Unit]
Description=Nextcloud Task Processing Utility supervisor
After=network.target

[Service]
User=www-data
Environment=NC_OCC=php /var/www/html/occ
Environment=NC_WORKER_TIMEOUT=300
ExecStart=/var/www/html/apps/task_proc_util/bin/task_proc_util-supervisor /var/www/html/config/config.php
# The line above starts the shell script. The script selects bin/amd64,
# bin/arm64 or bin/arm automatically with `uname -m`.
Restart=always
RestartSec=5
# The workers get an interval to complete the current task when the service
# stops. The default value (90 s) stops the supervisor too soon. Then the
# supervisor cannot set the state of the interrupted tasks.
TimeoutStopSec=3600

[Install]
WantedBy=multi-user.target
```

### Flags and Environment Variables

| Flag or positional value  | Environment variable | Default                | Description |
|---------------------------|----------------------|------------------------|-------------|
| `config.php` (positional) | `NC_CONFIG_FILE`     | —                      | The path to the Nextcloud `config.php` file. It is necessary, if you do not give all the other values. |
| `--occ`                   | `NC_OCC`             | `php occ`              | The command that starts occ. Put a space between the parts. |
| `--occ-dir`               | `NC_OCC_DIR`         | from `config.php`      | The Nextcloud root directory for the occ command. `occ` does not operate in a different directory. |
| `--worker-timeout`        | `NC_WORKER_TIMEOUT`  | `300`                  | The interval in seconds before a worker stops and starts again (0 = never). If a worker continues for more than this interval, the supervisor waits 30 minutes and then stops the worker. |
| `--database-url`          | `DATABASE_URL`       | —                      | A different database connection, for example `mysql://user:pass@host/db`. |
| `--database-prefix`       | `DATABASE_PREFIX`    | —                      | A different prefix for the names of the tables. |
| `--nextcloud-url`         | `NEXTCLOUD_URL`      | —                      | A different Nextcloud URL. |

The application operates with these databases: **MySQL / MariaDB**, **PostgreSQL**, and **SQLite3**. The application does not operate with Oracle.

### The Worker Command

The supervisor starts the workers automatically. Use these options only for manual operation and for troubleshooting. The supervisor gives only `--taskType` and `--timeout` to the worker.

| Option             | Default | Description |
|--------------------|---------|-------------|
| `--taskType`, `-t` | —       | The identifier of the task type for this worker. It is necessary. |
| `--timeout`        | `0`     | Stop after this number of seconds (0 = no limit). |
| `--max-tasks`      | `0`     | Stop after this number of tasks (0 = no limit). |
| `--interval`, `-i` | `1`     | The number of seconds to wait when there is no task. |
| `--exit-when-idle` | off     | Stop immediately when the queue of this task type is empty. |

These are the exit codes:

- `0` — the worker stopped correctly, because of the timeout, the maximum number of tasks, an empty queue, or a signal.
- `1` — an error occurred.
- `3` — the task type has no preferred synchronous provider.

## How to Measure the Performance

Two scripts make tasks and show the results. The two scripts read `NC_URL`, `NC_USER`, and `NC_PASS` from the environment.

```sh
# Show the task types of this server
./scripts/schedule-tasks.sh --list

# Make tasks of more than one type. The script shows a batch identifier.
./scripts/schedule-tasks.sh core:text2text=50 core:text2text:chat=10

# Show the condition of this batch until the queue is empty
./scripts/task-stats.sh --batch <id> --watch
```

The `/schedule` endpoint permits a maximum of 20 requests in 120 seconds for each user. The script waits and then does the request again. The `--delay` option decreases the speed for large batches.

Use the **achieved concurrency** value and the **task/s** value to measure the performance. Do not use the `RUN` column. The `RUN` column shows the condition at one moment only, thus its value is frequently too low. A worker has no task in the `RUNNING` state in the interval between two tasks.

## Development

```sh
# The Go supervisor
make supervisor          # make the binary file for this computer → bin/<arch>/task_proc_util-supervisor
make supervisor-release  # make the binary files for amd64, arm64 and arm
make supervisor-test     # go test ./...
make supervisor-vet      # go vet ./...

# The user interface
make frontend            # npm ci && npm run build → js/

# The full release build: all of the above and composer
make build
```
