// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	raw, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "nc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec(`CREATE TABLE oc_taskprocessing_tasks (
		id INTEGER PRIMARY KEY, type TEXT, status INTEGER,
		ended_at INTEGER, last_updated INTEGER, error_message TEXT)`); err != nil {
		t.Fatal(err)
	}
	return &DB{db: raw, prefix: "oc_"}
}

func status(t *testing.T, d *DB, id int) int {
	t.Helper()
	var s int
	if err := d.db.QueryRow(`SELECT status FROM oc_taskprocessing_tasks WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Killing one overrunning worker must fail only the task that worker claimed.
// Other workers of the same task type are still processing their own tasks;
// failing those corrupts work that is actively in flight.
func TestFailRunningTasksSparesOtherWorkersTasks(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	// Worker A is already processing task 1.
	if _, err := d.db.Exec(`INSERT INTO oc_taskprocessing_tasks (id, type, status) VALUES
		(1,'speech2text',?), (2,'speech2text',?), (3,'text2text',?)`,
		statusRunning, statusScheduled, statusRunning); err != nil {
		t.Fatal(err)
	}

	// Worker B starts and snapshots what is already in flight.
	preexisting, err := d.RunningTaskIDs(ctx, "speech2text")
	if err != nil {
		t.Fatal(err)
	}

	// Worker B claims task 2, then wedges and is killed.
	if _, err := d.db.Exec(`UPDATE oc_taskprocessing_tasks SET status = ? WHERE id = 2`, statusRunning); err != nil {
		t.Fatal(err)
	}
	n, err := d.FailRunningTasks(ctx, "speech2text", "killed", preexisting)
	if err != nil {
		t.Fatal(err)
	}

	if n != 1 {
		t.Errorf("failed %d tasks, want exactly 1", n)
	}
	if got := status(t, d, 1); got != statusRunning {
		t.Errorf("task 1 belongs to live worker A: status %d, want %d", got, statusRunning)
	}
	if got := status(t, d, 2); got != statusFailed {
		t.Errorf("task 2 was the killed worker's: status %d, want %d", got, statusFailed)
	}
	if got := status(t, d, 3); got != statusRunning {
		t.Errorf("task 3 is another task type: status %d, want %d", got, statusRunning)
	}
}

// With no other workers running, the claimed task must still be failed;
// otherwise it sits in RUNNING forever and the user never sees an outcome.
func TestFailRunningTasksFailsOrphanWhenSoleWorker(t *testing.T) {
	d := newTestDB(t)
	if _, err := d.db.Exec(`INSERT INTO oc_taskprocessing_tasks (id, type, status) VALUES (1,'speech2text',?)`,
		statusRunning); err != nil {
		t.Fatal(err)
	}
	n, err := d.FailRunningTasks(context.Background(), "speech2text", "killed", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || status(t, d, 1) != statusFailed {
		t.Errorf("orphan not failed: n=%d status=%d", n, status(t, d, 1))
	}
}

// lib/pq rejects `?` markers, so Postgres queries must be rewritten to $N.
// MySQL and SQLite must be left untouched.
func TestRebindPlaceholders(t *testing.T) {
	const query = "SELECT id FROM t WHERE status = ? AND type = ?"

	pg := &DB{ordinal: true}
	if got, want := pg.rebind(query), "SELECT id FROM t WHERE status = $1 AND type = $2"; got != want {
		t.Errorf("postgres rebind = %q, want %q", got, want)
	}

	my := &DB{}
	if got := my.rebind(query); got != query {
		t.Errorf("non-postgres rebind = %q, want unchanged", got)
	}
}
