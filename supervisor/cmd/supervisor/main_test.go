// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kyteinsky/task_proc_util/supervisor/internal/db"
	"github.com/kyteinsky/task_proc_util/supervisor/internal/pool"
	sqlite "modernc.org/sqlite"
)

// Query counters, split by the table each loop cadence touches.
var appconfigReads, taskReads, queueStatReads atomic.Int64

// countingDriver wraps sqlite to tally queries per table. run() takes a
// concrete *db.DB, so the only seam for observing its database traffic is the
// driver underneath it.
type countingDriver struct{ inner driver.Driver }

type sqliteConn interface {
	driver.Conn
	driver.QueryerContext
	driver.ExecerContext
}

type countingConn struct{ sqliteConn }

func (d countingDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return countingConn{c.(sqliteConn)}, nil
}

func (c countingConn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(q, "appconfig"):
		appconfigReads.Add(1)
	case strings.Contains(q, "taskprocessing_tasks"):
		taskReads.Add(1)
		// The queue stats query specifically, as opposed to the per-worker
		// RunningTaskIDs snapshot that also hits this table.
		if strings.Contains(q, "GROUP BY") {
			queueStatReads.Add(1)
		}
	}
	return c.sqliteConn.QueryContext(ctx, q, a)
}

func init() {
	sql.Register("sqlite-counting", countingDriver{inner: &sqlite.Driver{}})
}

// newLoopDB seeds a Nextcloud-shaped sqlite database with the given supervisor
// settings. It returns a handle that counts queries, plus a writer for
// simulating an admin changing settings while the loop runs.
//
// WAL and a busy timeout are required on both handles: without them a write
// racing the loop's reads fails with SQLITE_BUSY, the settings change is
// silently dropped, and a test asserting "the change took effect" passes for
// entirely the wrong reason.
func newLoopDB(t *testing.T, pollInterval, configInterval int) (*db.DB, *sql.DB) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "nc.db") +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"

	seed, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = seed.Close() })
	if _, err := seed.Exec(`
		CREATE TABLE oc_taskprocessing_tasks (
			id INTEGER PRIMARY KEY, type TEXT, status INTEGER,
			ended_at INTEGER, last_updated INTEGER, error_message TEXT);
		CREATE TABLE oc_appconfig (appid TEXT, configkey TEXT, configvalue TEXT);
		INSERT INTO oc_appconfig VALUES ('task_proc_util','max_workers','0');`); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]int{"poll_interval": pollInterval, "config_interval": configInterval} {
		if _, err := seed.Exec(
			`INSERT INTO oc_appconfig VALUES ('task_proc_util', ?, ?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}

	ncDB, err := db.Open("sqlite-counting", dsn, "oc_")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ncDB.Close)
	return ncDB, seed
}

// runFor drives the real control loop until the deadline elapses.
func runFor(t *testing.T, ncDB *db.DB, d time.Duration) {
	t.Helper()
	appconfigReads.Store(0)
	taskReads.Store(0)
	queueStatReads.Store(0)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// max_workers=0 keeps Reconcile from ever spawning a PHP process.
	wp := pool.New([]string{"/bin/true"}, t.TempDir(), 300, ncDB, logger)

	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	run(ctx, logger, ncDB, wp)
}

// The reason the two cadences exist: the queue must be polled frequently while
// admin settings are re-read rarely. With a config interval far longer than the
// run, config must be read exactly once (at startup) no matter how many times
// the queue is polled. A loop that re-reads config per poll fails this.
func TestConfigIsNotReReadOnEveryPoll(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-based; runs the real loop")
	}

	ncDB, _ := newLoopDB(t, 1, 3600)
	runFor(t, ncDB, 4*time.Second)

	cfgN, taskN := appconfigReads.Load(), taskReads.Load()
	t.Logf("poll=1s config=3600s over 4s: appconfig reads=%d, task reads=%d", cfgN, taskN)

	if cfgN != 1 {
		t.Errorf("appconfig read %d times, want exactly 1 (startup only)", cfgN)
	}
	// Loose lower bound: a slow machine polls fewer times, never more often
	// than the interval allows.
	if taskN < 2 {
		t.Errorf("task tables read %d times, want the queue polled repeatedly", taskN)
	}
}

// Each poll must cost exactly one query against the task table, whatever the
// number of queued task types. The earlier shape — list the types, then count
// scheduled and running per type — cost 1+2N round trips per poll and grew
// with the workload.
func TestPollCostIsOneQueryRegardlessOfTaskTypes(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-based; runs the real loop")
	}

	ncDB, writer := newLoopDB(t, 1, 3600)
	for i, taskType := range []string{"a", "b", "c", "d", "e"} {
		if _, err := writer.Exec(
			`INSERT INTO oc_taskprocessing_tasks (id, type, status) VALUES (?,?,?)`,
			i+1, taskType, 1 /* SCHEDULED */); err != nil {
			t.Fatal(err)
		}
	}

	runFor(t, ncDB, 4*time.Second)

	// Four 1s polls over a 4s window: t=0,1,2,3.
	const polls, taskTypes = 4, 5
	statsN := queueStatReads.Load()
	t.Logf("%d task types over %d polls: queue stats queries=%d (old shape: %d)",
		taskTypes, polls, statsN, polls*(1+2*taskTypes))

	// One grouped query per poll, independent of how many types are queued.
	if statsN > polls {
		t.Errorf("queue stats read %d times over %d polls, want at most %d", statsN, polls, polls)
	}
	if statsN < 2 {
		t.Errorf("queue stats read only %d times; the poll loop did not run", statsN)
	}
}

// A poll interval edited in the admin UI must take effect at the next config
// refresh, without restarting the supervisor. Widening the interval mid-run
// must stop the fast polling.
func TestPollIntervalChangeAppliesWithoutRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-based; runs the real loop")
	}

	ncDB, writer := newLoopDB(t, 1, 2)

	// Simulate an admin widening the poll interval while the loop is running.
	go func() {
		time.Sleep(3 * time.Second)
		if _, err := writer.Exec(
			`UPDATE oc_appconfig SET configvalue = '3600' WHERE configkey = 'poll_interval'`); err != nil {
			t.Errorf("failed to update poll_interval: %v", err)
		}
	}()

	runFor(t, ncDB, 7*time.Second)

	taskN := taskReads.Load()
	t.Logf("poll widened 1s -> 3600s at t=3s: task reads over 7s = %d", taskN)

	// ~4 polls before the change takes effect, then none. Without the live
	// reload this would keep polling every second for the full 7s.
	if taskN > 6 {
		t.Errorf("task tables read %d times; poll interval change did not take effect", taskN)
	}
	if taskN < 2 {
		t.Errorf("task tables read %d times; expected fast polling before the change", taskN)
	}
}
