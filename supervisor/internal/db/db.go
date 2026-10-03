// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package db queries the Nextcloud database directly, exactly as notify_push
// does. No HTTP auth or app password is needed — the supervisor runs on the
// same host as Nextcloud and has filesystem access to config.php.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	// Register the three supported pure-Go drivers.
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

// task processing status constants (mirror OCP\TaskProcessing\Task).
const (
	statusUnknown    = 0
	statusScheduled  = 1
	statusRunning    = 2
	statusSuccessful = 3
	statusFailed     = 4
	statusCancelled  = 5
)

// DB wraps a sql.DB with helpers scoped to Nextcloud's schema.
type DB struct {
	db     *sql.DB
	prefix string
	// ordinal is true for drivers that want PostgreSQL-style $1, $2 markers
	// instead of the `?` used by MySQL and SQLite. Queries below are written
	// with `?` and translated by rebind.
	ordinal bool
}

// rebind converts the `?` placeholders used throughout this package into the
// driver's native marker syntax. lib/pq only accepts ordinal markers ($1, $2),
// so without this every Postgres query fails with a syntax error.
func (d *DB) rebind(query string) string {
	if !d.ordinal {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := range len(query) {
		if query[i] != '?' {
			b.WriteByte(query[i])
			continue
		}
		n++
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(n))
	}
	return b.String()
}

// Open connects to the Nextcloud database.
// driver is "mysql", "postgres", or "sqlite3".
// dsn is the driver-specific connection string.
// prefix is the NC table prefix, usually "oc_".
func Open(driver, dsn, prefix string) (*DB, error) {
	// modernc.org/sqlite registers under "sqlite", not "sqlite3".
	if driver == "sqlite3" {
		driver = "sqlite"
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s db: %w", driver, err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping %s db: %w", driver, err)
	}
	return &DB{db: db, prefix: prefix, ordinal: driver == "postgres"}, nil
}

// Close closes the underlying connection.
func (d *DB) Close() {
	_ = d.db.Close()
}

// QueueStats holds the pending and running task count for a task type.
type QueueStats struct {
	Scheduled int
	Running   int
}

// GetQueueStats returns scheduled and running task counts for the given task
// type ID. If taskTypeID is empty, counts all task types.
func (d *DB) GetQueueStats(ctx context.Context, taskTypeID string) (QueueStats, error) {
	var stats QueueStats
	var err error
	stats.Scheduled, err = d.countByStatus(ctx, statusScheduled, taskTypeID)
	if err != nil {
		return QueueStats{}, err
	}
	stats.Running, err = d.countByStatus(ctx, statusRunning, taskTypeID)
	if err != nil {
		return QueueStats{}, err
	}
	return stats, nil
}

func (d *DB) countByStatus(ctx context.Context, status int, taskTypeID string) (int, error) {
	table := d.prefix + "taskprocessing_tasks"
	var row *sql.Row
	if taskTypeID == "" {
		row = d.db.QueryRowContext(ctx,
			d.rebind(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE status = ?", table)), status)
	} else {
		row = d.db.QueryRowContext(ctx,
			d.rebind(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE status = ? AND type = ?", table)), status, taskTypeID)
	}
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count tasks (status=%d type=%q): %w", status, taskTypeID, err)
	}
	return n, nil
}

// RunningTaskIDs returns the IDs of tasks currently RUNNING for a task type.
//
// Snapshotted before a worker starts so that, if that worker later has to be
// killed, we can tell its own claimed task apart from tasks belonging to the
// other workers of the same type that are still alive and working.
func (d *DB) RunningTaskIDs(ctx context.Context, taskTypeID string) ([]int64, error) {
	table := d.prefix + "taskprocessing_tasks"
	rows, err := d.db.QueryContext(ctx,
		d.rebind(fmt.Sprintf("SELECT id FROM %s WHERE status = ? AND type = ?", table)),
		statusRunning, taskTypeID)
	if err != nil {
		return nil, fmt.Errorf("list running tasks (type=%q): %w", taskTypeID, err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FailRunningTasks marks the tasks a killed worker was holding as FAILED.
//
// Called after a worker had to be killed for overrunning its timeout: the PHP
// process never got to report a result, so the task it had claimed would
// otherwise sit in RUNNING forever. Mirrors what Manager::setTaskResult does for
// an error (status, ended_at, error_message), so the task surfaces to the user
// as failed rather than perpetually in progress.
//
// exclude lists task IDs that were already RUNNING when this worker started;
// they belong to other, still-live workers of the same type and MUST NOT be
// touched — failing them would corrupt tasks that are actively being processed.
//
// Returns the number of tasks failed.
func (d *DB) FailRunningTasks(ctx context.Context, taskTypeID, errMsg string, exclude []int64) (int, error) {
	table := d.prefix + "taskprocessing_tasks"
	now := time.Now().Unix()

	query := fmt.Sprintf(`UPDATE %s SET status = ?, ended_at = ?, last_updated = ?, error_message = ?
			WHERE status = ? AND type = ?`, table)
	args := []any{statusFailed, now, now, errMsg, statusRunning, taskTypeID}

	if len(exclude) > 0 {
		query += " AND id NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(exclude)), ",") + ")"
		for _, id := range exclude {
			args = append(args, id)
		}
	}

	res, err := d.db.ExecContext(ctx, d.rebind(query), args...)
	if err != nil {
		return 0, fmt.Errorf("fail running tasks (type=%q): %w", taskTypeID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("fail running tasks (type=%q): rows affected: %w", taskTypeID, err)
	}
	return int(n), nil
}

// ListTaskTypes returns the distinct task type IDs that have at least one
// scheduled or running task.
func (d *DB) ListTaskTypes(ctx context.Context) ([]string, error) {
	table := d.prefix + "taskprocessing_tasks"
	rows, err := d.db.QueryContext(ctx,
		d.rebind(fmt.Sprintf("SELECT DISTINCT type FROM %s WHERE status IN (?, ?)", table)),
		statusScheduled, statusRunning)
	if err != nil {
		return nil, fmt.Errorf("list task types: %w", err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		types = append(types, t)
	}
	return types, rows.Err()
}

// SupervisorConfig is the admin-tunable config stored in oc_appconfig.
type SupervisorConfig struct {
	MaxWorkers   int
	PollInterval int
	Enabled      bool
}

// GetConfig reads supervisor settings from oc_appconfig.
func (d *DB) GetConfig(ctx context.Context) (SupervisorConfig, error) {
	cfg := SupervisorConfig{
		MaxWorkers:   4,
		PollInterval: 10,
		Enabled:      true,
	}
	table := d.prefix + "appconfig"
	rows, err := d.db.QueryContext(ctx,
		d.rebind(fmt.Sprintf("SELECT configkey, configvalue FROM %s WHERE appid = ?", table)),
		"task_proc_util")
	if err != nil {
		return cfg, fmt.Errorf("read appconfig: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return cfg, err
		}
		switch k {
		case "max_workers":
			if n, err := strconv.Atoi(v); err == nil {
				cfg.MaxWorkers = n
			}
		case "poll_interval":
			if n, err := strconv.Atoi(v); err == nil {
				cfg.PollInterval = n
			}
		case "autoscale_enabled":
			// Deliberately not the "enabled" key: Nextcloud reserves that one
			// for the app's own enable state ("yes"/"no"/JSON group list).
			cfg.Enabled = v != "0" && v != "false"
		}
	}
	return cfg, rows.Err()
}
