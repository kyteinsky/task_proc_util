// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pool manages the lifecycle of the PHP worker subprocesses. Each
// worker is pinned to a single task type and runs the
// `taskprocessing-util:run` occ command until it recycles (timeout) or is
// stopped. The pool reconciles the set of running workers against a desired
// per-task-type count produced by the scheduler.
package pool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// gracePeriod is how long a worker is given to finish its current task and
// exit after being asked to stop, before it is killed outright.
//
// The PHP worker only checks its own --timeout between tasks, so a worker
// blocked inside a provider call would otherwise never exit. Go enforces the
// hard bound; PHP handles the clean case. Generous because AI tasks are
// long-running and we would rather let one finish than kill it and fail a task
// that was about to succeed.
const gracePeriod = 30 * time.Minute

// stderrTailBytes caps how much of a failing worker's stderr is retained for
// the log line. Enough for a PHP fatal or an occ precondition message, bounded
// so a chatty worker cannot grow the supervisor's memory without limit.
const stderrTailBytes = 4096

// tailBuffer keeps only the last limit bytes written to it.
type tailBuffer struct {
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= t.limit {
		t.buf = append(t.buf[:0], p[n-t.limit:]...)
		return n, nil
	}
	if over := len(t.buf) + n - t.limit; over > 0 {
		t.buf = t.buf[over:]
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) String() string {
	return strings.TrimSpace(string(t.buf))
}

// TaskFailer marks orphaned RUNNING tasks as failed. Implemented by *db.DB.
type TaskFailer interface {
	// RunningTaskIDs snapshots the tasks already in flight for a type.
	RunningTaskIDs(ctx context.Context, taskTypeID string) ([]int64, error)
	// FailRunningTasks fails RUNNING tasks of a type except those in exclude.
	FailRunningTasks(ctx context.Context, taskTypeID, errMsg string, exclude []int64) (int, error)
}

// exitNoSyncProvider mirrors Run::EXIT_NO_SYNC_PROVIDER in the PHP worker: the
// task type has no preferred synchronous provider, so a worker for it can never
// do anything and must not be respawned every tick.
const exitNoSyncProvider = 3

// Pool supervises PHP worker subprocesses.
type Pool struct {
	occCommand    []string
	occDir        string
	workerTimeout int
	failer        TaskFailer
	logger        *slog.Logger

	mu      sync.Mutex
	workers map[string][]*worker // taskTypeID -> running workers
	nextID  int
	// unworkable task types that reported EXIT_NO_SYNC_PROVIDER. Cleared on
	// every config change so enabling a provider takes effect without a restart.
	unworkable map[string]bool
}

type worker struct {
	id         int
	taskTypeID string
	cancel     context.CancelFunc
	done       chan struct{}
}

// New creates a Pool.
//
// occDir is the Nextcloud root that workers are run from; occ exits immediately
// if started anywhere else. Empty means inherit the supervisor's directory.
func New(occCommand []string, occDir string, workerTimeout int, failer TaskFailer, logger *slog.Logger) *Pool {
	return &Pool{
		occCommand:    occCommand,
		occDir:        occDir,
		workerTimeout: workerTimeout,
		failer:        failer,
		logger:        logger,
		workers:       map[string][]*worker{},
		unworkable:    map[string]bool{},
	}
}

// ResetUnworkable clears the set of task types marked as having no synchronous
// provider, so a newly configured provider is picked up without a restart.
func (p *Pool) ResetUnworkable() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.unworkable)
}

// Reconcile starts or stops workers so the running count per task type matches
// the desired plan. Task types absent from the plan are scaled to zero.
func (p *Pool) Reconcile(ctx context.Context, desired map[string]int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Scale down / remove task types no longer desired.
	for taskTypeID, workers := range p.workers {
		want := desired[taskTypeID]
		for len(p.workers[taskTypeID]) > want {
			last := len(p.workers[taskTypeID]) - 1
			w := p.workers[taskTypeID][last]
			w.cancel()
			p.workers[taskTypeID] = p.workers[taskTypeID][:last]
		}
		if len(p.workers[taskTypeID]) == 0 {
			delete(p.workers, taskTypeID)
		}
		_ = workers
	}

	// Scale up.
	for taskTypeID, want := range desired {
		if p.unworkable[taskTypeID] {
			continue
		}
		for len(p.workers[taskTypeID]) < want {
			w := p.startWorker(ctx, taskTypeID)
			p.workers[taskTypeID] = append(p.workers[taskTypeID], w)
		}
	}
}

// startWorker launches one worker subprocess for a task type and watches it.
// When the process exits (recycle/idle/error) the worker removes itself from
// the pool so the next Reconcile can replace it if still desired.
func (p *Pool) startWorker(ctx context.Context, taskTypeID string) *worker {
	p.nextID++
	// Belt-and-braces deadline: PHP honours --timeout only between tasks, so we
	// independently cancel a little later. The slack lets the well-behaved case
	// exit on its own and be logged as a clean recycle.
	var wctx context.Context
	var cancel context.CancelFunc
	if p.workerTimeout > 0 {
		hardLimit := time.Duration(p.workerTimeout)*time.Second + gracePeriod
		wctx, cancel = context.WithTimeout(ctx, hardLimit)
	} else {
		wctx, cancel = context.WithCancel(ctx)
	}
	w := &worker{
		id:         p.nextID,
		taskTypeID: taskTypeID,
		cancel:     cancel,
		done:       make(chan struct{}),
	}

	args := append([]string{}, p.occCommand[1:]...)
	args = append(args, "taskprocessing-util:run", "--taskType", taskTypeID)
	if p.workerTimeout > 0 {
		args = append(args, "--timeout", fmt.Sprintf("%d", p.workerTimeout))
	}

	p.logger.Info("starting worker", "id", w.id, "taskType", taskTypeID)

	// Snapshot the tasks already in flight for this type. They belong to other
	// live workers, so if *this* worker later has to be killed they must not be
	// failed along with the one it was actually holding.
	preexisting := p.runningTaskIDs(ctx, taskTypeID)

	go func() {
		defer close(w.done)
		defer cancel()

		cmd := exec.CommandContext(wctx, p.occCommand[0], args...)
		// occ refuses to start outside the Nextcloud root ("This script can be
		// run from the Nextcloud root directory only"), so run it there rather
		// than inheriting whatever directory the service was started in.
		cmd.Dir = p.occDir
		// Keep the tail of stderr: without it a worker that dies during
		// bootstrap reports a bare "exit status 1" and the actual reason —
		// wrong user, missing app, maintenance mode — is lost.
		errTail := &tailBuffer{limit: stderrTailBytes}
		cmd.Stderr = errTail
		// New process group so signals reach the PHP process and anything it
		// spawned, not just the leader.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		// On cancel, ask the whole group to stop so PHP can finish its current
		// task and release it. Without this Go would SIGKILL immediately,
		// stranding the task in RUNNING.
		cmd.Cancel = func() error {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
		// If it has not exited within the grace period, Go escalates to SIGKILL
		// so a wedged worker can never block Shutdown forever.
		cmd.WaitDelay = gracePeriod

		err := cmd.Run()
		switch {
		case errors.Is(wctx.Err(), context.DeadlineExceeded):
			// PHP overran its own --timeout, so it was stuck inside a task
			// (typically a provider not returning) and was killed before it
			// could report a result. Fail the task it was holding, otherwise it
			// stays RUNNING forever and the user never sees an outcome.
			p.logger.Warn("worker exceeded hard timeout and was terminated",
				"id", w.id, "taskType", taskTypeID, "timeout", p.workerTimeout)
			p.failOrphanedTasks(taskTypeID, preexisting)
		case wctx.Err() != nil:
			// Deliberately stopped by scale-down or shutdown.
			p.logger.Debug("worker stopped", "id", w.id, "taskType", taskTypeID)
		case isExitCode(err, exitNoSyncProvider):
			// Permanently unworkable: respawning would burn a full Nextcloud
			// bootstrap every poll tick and never process anything.
			p.logger.Warn("task type has no preferred synchronous provider; not starting further workers for it",
				"id", w.id, "taskType", taskTypeID, "stderr", errTail.String())
			p.markUnworkable(taskTypeID)
		case err != nil:
			p.logger.Warn("worker exited with error",
				"id", w.id, "taskType", taskTypeID, "err", err, "stderr", errTail.String())
		default:
			p.logger.Debug("worker exited cleanly", "id", w.id, "taskType", taskTypeID)
		}

		p.remove(taskTypeID, w)
	}()

	return w
}

// isExitCode reports whether err is a process exit with the given status.
func isExitCode(err error, code int) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == code
}

// markUnworkable records a task type as having no synchronous provider.
func (p *Pool) markUnworkable(taskTypeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unworkable[taskTypeID] = true
}

// runningTaskIDs snapshots the tasks already in flight for a task type.
//
// Best-effort: on error we return nil, which only means a later orphan cleanup
// has no exclusions. Failing to start a worker over this would be worse.
func (p *Pool) runningTaskIDs(ctx context.Context, taskTypeID string) []int64 {
	if p.failer == nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	ids, err := p.failer.RunningTaskIDs(cctx, taskTypeID)
	if err != nil {
		p.logger.Warn("could not snapshot running tasks; orphan cleanup may be imprecise",
			"taskType", taskTypeID, "err", err)
		return nil
	}
	return ids
}

// failOrphanedTasks marks tasks left RUNNING by a killed worker as failed.
//
// Uses its own context: the worker's context is already expired, and during
// shutdown the parent is cancelled too, but this cleanup still has to run.
func (p *Pool) failOrphanedTasks(taskTypeID string, exclude []int64) {
	if p.failer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 15*time.Second)
	defer cancel()

	const errMsg = "Worker exceeded its time limit and was terminated by the task processing supervisor"
	n, err := p.failer.FailRunningTasks(ctx, taskTypeID, errMsg, exclude)
	if err != nil {
		p.logger.Error("failed to mark orphaned tasks as failed", "taskType", taskTypeID, "err", err)
		return
	}
	if n > 0 {
		p.logger.Warn("marked orphaned tasks as failed", "taskType", taskTypeID, "count", n)
	}
}

// remove drops a worker from the pool's bookkeeping once it has exited.
func (p *Pool) remove(taskTypeID string, w *worker) {
	p.mu.Lock()
	defer p.mu.Unlock()
	workers := p.workers[taskTypeID]
	for i, x := range workers {
		if x == w {
			p.workers[taskTypeID] = append(workers[:i], workers[i+1:]...)
			break
		}
	}
	if len(p.workers[taskTypeID]) == 0 {
		delete(p.workers, taskTypeID)
	}
}

// Shutdown cancels all workers and waits for them to exit.
func (p *Pool) Shutdown() {
	p.mu.Lock()
	var all []*worker
	for _, workers := range p.workers {
		all = append(all, workers...)
	}
	p.mu.Unlock()

	for _, w := range all {
		w.cancel()
	}
	for _, w := range all {
		<-w.done
	}
}

// Running returns the current worker count per task type (for logging/metrics).
func (p *Pool) Running() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]int{}
	for taskTypeID, workers := range p.workers {
		out[taskTypeID] = len(workers)
	}
	return out
}
