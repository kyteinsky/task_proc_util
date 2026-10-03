// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

// Command supervisor is the persistent service that auto-scales Task Processing
// PHP workers based on the Nextcloud task queue.
//
// Usage: task_proc_util-supervisor [flags] /path/to/nextcloud/config/config.php
//
// On each tick it:
//  1. reads admin config from the Nextcloud DB (min/max workers, poll interval, enabled),
//  2. queries each task type's queue depth directly from the DB,
//  3. computes a fair per-type worker allocation,
//  4. reconciles the running worker subprocesses to match.
//
// No app password or HTTP credentials are needed — the supervisor runs on the
// same host as Nextcloud and reads config.php directly, like notify_push.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kyteinsky/task_proc_util/supervisor/internal/config"
	"github.com/kyteinsky/task_proc_util/supervisor/internal/db"
	"github.com/kyteinsky/task_proc_util/supervisor/internal/pool"
	"github.com/kyteinsky/task_proc_util/supervisor/internal/scheduler"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.FromArgs(os.Args[1:])
	if err != nil {
		logger.Error("failed to load config", "err", err)
		os.Exit(2)
	}

	ncDB, err := db.Open(cfg.DBDriver, cfg.DBDSN, cfg.DBPrefix)
	if err != nil {
		logger.Error("failed to connect to database", "err", err)
		os.Exit(1)
	}
	defer ncDB.Close()

	workerPool := pool.New(cfg.OccCommand, cfg.OccDir, cfg.WorkerTimeout, ncDB, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Verify occ actually works before entering the poll loop. Otherwise every
	// worker dies on spawn and the supervisor churns through PHP processes
	// forever without the queue ever moving.
	if err := checkOcc(ctx, cfg); err != nil {
		logger.Error("occ is not runnable; workers would fail to start", "err", err)
		os.Exit(1)
	}

	logger.Info("supervisor starting",
		"nextcloudURL", cfg.NextcloudURL, "db", cfg.DBDriver, "occDir", cfg.OccDir)

	run(ctx, logger, ncDB, workerPool)

	logger.Info("shutting down, stopping workers")
	workerPool.Shutdown()
	logger.Info("supervisor stopped")
}

// checkOcc runs `occ taskprocessing-util:run --help` once at startup. It proves
// occ is invokable, that the working directory is right, and that this app's
// command is actually registered — the three ways every worker silently dies.
func checkOcc(ctx context.Context, cfg *config.Config) error {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	args := append([]string{}, cfg.OccCommand[1:]...)
	args = append(args, "taskprocessing-util:run", "--help")

	cmd := exec.CommandContext(cctx, cfg.OccCommand[0], args...)
	cmd.Dir = cfg.OccDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = "(no output)"
		}
		return fmt.Errorf("`%s taskprocessing-util:run --help` in %q failed: %w: %s",
			strings.Join(cfg.OccCommand, " "), cfg.OccDir, err, msg)
	}
	return nil
}

// supervisor carries the state shared by the two loop cadences: the fast poll
// that reconciles workers against the queue, and the slow refresh that re-reads
// admin config.
type supervisor struct {
	logger     *slog.Logger
	ncDB       *db.DB
	workerPool *pool.Pool

	// cfg is the most recent config successfully read from the database. Every
	// poll uses it until the next refresh replaces it, so a database blip
	// leaves the supervisor scaling on the last known-good values rather than
	// stalling or falling back to defaults.
	cfg db.SupervisorConfig
}

// run is the main control loop. Config reads and queue polls run on separate
// cadences: the queue is volatile and needs frequent polling, while admin
// settings change rarely, so re-reading them at the poll rate is pure overhead.
// Both intervals are themselves admin-configurable.
func run(ctx context.Context, logger *slog.Logger, ncDB *db.DB, workerPool *pool.Pool) {
	s := &supervisor{
		logger:     logger,
		ncDB:       ncDB,
		workerPool: workerPool,
		cfg:        db.DefaultConfig(),
	}

	// Read config once up front so the first poll scales on real settings
	// rather than on defaults.
	s.refreshConfig(ctx)

	pollTimer := time.NewTimer(0)
	defer pollTimer.Stop()
	configTimer := time.NewTimer(s.configInterval())
	defer configTimer.Stop()

	pollEvery := s.pollInterval()

	for {
		select {
		case <-ctx.Done():
			return

		case <-configTimer.C:
			s.refreshConfig(ctx)
			configTimer.Reset(s.configInterval())

			// Apply a changed poll interval immediately. Without this, a
			// shortened interval would not take effect until the pending
			// timer expired at the old, longer interval.
			if next := s.pollInterval(); next != pollEvery {
				pollEvery = next
				pollTimer.Stop()
				pollTimer.Reset(pollEvery)
			}

		case <-pollTimer.C:
			s.poll(ctx)
			pollEvery = s.pollInterval()
			pollTimer.Reset(pollEvery)
		}
	}
}

// pollInterval is how long to wait between queue polls.
func (s *supervisor) pollInterval() time.Duration {
	if s.cfg.PollInterval <= 0 {
		return time.Duration(db.DefaultConfig().PollInterval) * time.Second
	}
	return time.Duration(s.cfg.PollInterval) * time.Second
}

// configInterval is how long to wait between config re-reads.
func (s *supervisor) configInterval() time.Duration {
	if s.cfg.ConfigInterval <= 0 {
		return time.Duration(db.DefaultConfig().ConfigInterval) * time.Second
	}
	return time.Duration(s.cfg.ConfigInterval) * time.Second
}

// refreshConfig re-reads admin config. On failure the previous config is kept,
// so a transient database error does not disrupt scaling.
func (s *supervisor) refreshConfig(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cfg, err := s.ncDB.GetConfig(cctx)
	if err != nil {
		s.logger.Warn("failed to read config from db, keeping previous values", "err", err)
		return
	}

	// Re-test the task types previously found to have no synchronous provider,
	// on every refresh rather than only when this config changed.
	//
	// Provider preferences are not part of this config. Nextcloud keeps them in
	// oc_appconfig under core/ai.taskprocessing_provider_preferences, which is
	// readable from here, but the value is a {taskTypeId: providerId} map and a
	// provider id says nothing about whether that provider is synchronous: that
	// is an `instanceof ISynchronousProvider` check against a class resolved
	// from the runtime service container. The map is also absent whenever no
	// preference was set, since Nextcloud then falls back to the first
	// registered provider for the type.
	//
	// So an admin switching a task type from an asynchronous provider to a
	// synchronous one changes nothing the supervisor can evaluate. Resetting
	// only on a visible change would keep that type blacklisted until the
	// service restarts.
	//
	// The cost of being wrong is one PHP bootstrap per blacklisted type per
	// config interval, which is why the interval is minutes rather than
	// seconds.
	s.workerPool.ResetUnworkable()

	if cfg == s.cfg {
		return
	}
	s.cfg = cfg

	s.logger.Info("config reloaded",
		"max", cfg.MaxWorkers,
		"pollInterval", cfg.PollInterval,
		"configInterval", cfg.ConfigInterval,
		"enabled", cfg.Enabled,
	)
}

// poll performs one queue-read/schedule/reconcile cycle against the config
// cached by the most recent refresh.
func (s *supervisor) poll(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if !s.cfg.Enabled {
		// Disabled: drain all workers and idle.
		s.workerPool.Reconcile(ctx, map[string]int{})
		s.logger.Debug("supervisor disabled, no workers running")
		return
	}

	queueStats, err := s.ncDB.GetQueueStats(cctx)
	if err != nil {
		s.logger.Warn("failed to fetch queue stats", "err", err)
		return
	}

	running := s.workerPool.Running()
	demands := make([]scheduler.Demand, 0, len(queueStats))
	skipped := 0
	for id, stats := range queueStats {
		// The queue query cannot distinguish a task awaiting a synchronous
		// provider from one driven by an asynchronous provider elsewhere: the
		// database holds no provider information, and synchronicity is a PHP
		// type check on runtime-registered classes. A task type is known to be
		// asynchronous only once a worker for it has exited reporting no
		// synchronous provider.
		//
		// Such a type must be dropped here rather than later, when workers are
		// started. Its tasks are real and often numerous, so leaving it in the
		// demand set wins it a proportional share of the worker budget, and
		// those workers are then never started — starving the task types that
		// could have used them.
		if s.workerPool.IsUnworkable(id) {
			skipped++
			continue
		}
		demands = append(demands, scheduler.Demand{
			TaskTypeID: id,
			Scheduled:  stats.Scheduled,
			Running:    stats.Running,
			Workers:    running[id],
		})
	}

	plan := scheduler.Compute(demands, s.cfg.MaxWorkers)
	s.workerPool.Reconcile(ctx, plan)

	s.logger.Info("reconciled workers",
		"max", s.cfg.MaxWorkers,
		"plan", plan,
		"workers", s.workerPool.Running(),
		"skippedAsyncTypes", skipped,
	)
}
