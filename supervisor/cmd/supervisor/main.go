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

// run is the main control loop. It polls at the interval reported by the app
// config (re-read each tick so changes take effect without a restart).
func run(ctx context.Context, logger *slog.Logger, ncDB *db.DB, workerPool *pool.Pool) {
	// Default interval until the first successful config fetch.
	interval := 10 * time.Second
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		next := tick(ctx, logger, ncDB, workerPool, interval)
		interval = next
		timer.Reset(interval)
	}
}

// lastConfig is the config seen on the previous tick, used to detect changes.
var lastConfig db.SupervisorConfig

// tick performs one poll/schedule/reconcile cycle and returns the interval to
// wait before the next cycle.
func tick(ctx context.Context, logger *slog.Logger, ncDB *db.DB, workerPool *pool.Pool, fallback time.Duration) time.Duration {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cfg, err := ncDB.GetConfig(cctx)
	if err != nil {
		logger.Warn("failed to read config from db, keeping previous interval", "err", err)
		return fallback
	}

	// Config changed: a provider may have been configured since, so re-test
	// task types previously found to have no synchronous provider.
	if cfg != lastConfig {
		workerPool.ResetUnworkable()
		lastConfig = cfg
	}

	interval := time.Duration(cfg.PollInterval) * time.Second
	if interval <= 0 {
		interval = fallback
	}

	if !cfg.Enabled {
		// Disabled: drain all workers and idle.
		workerPool.Reconcile(ctx, map[string]int{})
		logger.Debug("supervisor disabled, no workers running")
		return interval
	}

	taskTypes, err := ncDB.ListTaskTypes(cctx)
	if err != nil {
		logger.Warn("failed to list task types", "err", err)
		return interval
	}

	running := workerPool.Running()
	demands := make([]scheduler.Demand, 0, len(taskTypes))
	for _, id := range taskTypes {
		stats, err := ncDB.GetQueueStats(cctx, id)
		if err != nil {
			logger.Warn("failed to fetch queue stats", "taskType", id, "err", err)
			continue
		}
		demands = append(demands, scheduler.Demand{
			TaskTypeID: id,
			Scheduled:  stats.Scheduled,
			Running:    stats.Running,
			Workers:    running[id],
		})
	}

	plan := scheduler.Compute(demands, cfg.MaxWorkers)
	workerPool.Reconcile(ctx, plan)

	logger.Info("reconciled workers",
		"max", cfg.MaxWorkers,
		"plan", plan,
		"workers", workerPool.Running(),
	)

	return interval
}
