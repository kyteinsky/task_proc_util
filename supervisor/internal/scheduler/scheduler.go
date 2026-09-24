// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package scheduler decides how many worker slots each task type should get,
// given the current queue depth per type and the global min/max worker bounds.
//
// Fairness goal: a flood of one task type must not starve the others. We first
// guarantee every task type with pending work at least one slot (round-robin
// style), then distribute any remaining slots proportionally to queue depth.
package scheduler

import "sort"

// Demand is the pending (scheduled) and in-flight (running) task count for a
// single task type, plus how many workers we currently have on it.
type Demand struct {
	TaskTypeID string
	Scheduled  int
	Running    int
	// Workers is the number of live worker processes this supervisor has for
	// the task type right now.
	Workers int
}

// work is the total number of tasks this type needs a worker for.
//
// Running tasks count only up to the number of workers we actually have. A
// worker that claims a task flips it SCHEDULED -> RUNNING, so ignoring Running
// entirely would make the busy worker's own task invisible and scale it away
// mid-task. But a RUNNING row with no worker behind it is an orphan — left by a
// crashed worker or an async provider driven elsewhere — and must create no
// demand, or the supervisor keeps spawning workers for a type that has nothing
// schedulable while starving types that do.
func (d Demand) work() int {
	inFlight := d.Running
	if inFlight > d.Workers {
		inFlight = d.Workers
	}
	return d.Scheduled + inFlight
}

// Plan maps a task type ID to the desired number of concurrent workers.
type Plan map[string]int

// Compute returns the desired worker allocation per task type.
//
//   - min: minimum total workers to keep running even when idle.
//   - max: maximum total workers allowed at once.
//
// A type's demand is its scheduled plus running tasks, so a worker is not
// scaled away while the task it already claimed is still in flight.
//
// Allocation algorithm:
//  1. Every task type with outstanding work gets one slot (fair baseline,
//     prevents starvation).
//  2. Remaining capacity up to max is distributed proportionally to each
//     type's outstanding work (largest-remainder method for stable rounding).
//  3. Nothing outstanding anywhere means no workers: min only applies to types
//     that actually have work to do.
func Compute(demands []Demand, min, max int) Plan {
	plan := Plan{}
	if max < 1 {
		max = 1
	}
	if min < 0 {
		min = 0
	}
	if min > max {
		min = max
	}

	// Only task types with outstanding work are candidates for workers.
	active := make([]Demand, 0, len(demands))
	totalScheduled := 0
	for _, d := range demands {
		if d.work() > 0 {
			active = append(active, d)
			totalScheduled += d.work()
		}
	}

	if len(active) == 0 {
		// Nothing pending. Keep min workers? There is no type to assign them to,
		// so the pool stays empty; min only matters when there is work.
		return plan
	}

	// Stable order: most outstanding work first, then by ID for determinism.
	sort.Slice(active, func(i, j int) bool {
		if active[i].work() != active[j].work() {
			return active[i].work() > active[j].work()
		}
		return active[i].TaskTypeID < active[j].TaskTypeID
	})

	// Total slots we want to run this round: at least min, at most max, and
	// never more than there is pending work.
	target := totalScheduled
	if target > max {
		target = max
	}
	if target < min {
		target = min
	}

	// Step 1: one slot per active type (baseline fairness), capped at target.
	for _, d := range active {
		if sum(plan) >= target {
			break
		}
		plan[d.TaskTypeID] = 1
	}

	// Step 2: distribute the remainder proportionally to scheduled counts using
	// the largest-remainder method.
	remaining := target - sum(plan)
	if remaining > 0 {
		distributeProportional(plan, active, totalScheduled, remaining)
	}

	// Never assign more workers to a type than it has tasks to work on.
	for _, d := range active {
		if plan[d.TaskTypeID] > d.work() {
			plan[d.TaskTypeID] = d.work()
		}
	}

	return plan
}

// distributeProportional hands out `remaining` extra slots across active types
// in proportion to their scheduled counts, using the largest-remainder method
// for fair, stable rounding.
func distributeProportional(plan Plan, active []Demand, totalScheduled, remaining int) {
	type share struct {
		id        string
		whole     int
		remainder float64
	}
	shares := make([]share, 0, len(active))
	for _, d := range active {
		exact := float64(remaining) * float64(d.work()) / float64(totalScheduled)
		whole := int(exact)
		shares = append(shares, share{id: d.TaskTypeID, whole: whole, remainder: exact - float64(whole)})
		plan[d.TaskTypeID] += whole
	}

	assigned := 0
	for _, s := range shares {
		assigned += s.whole
	}
	leftover := remaining - assigned

	// Hand out leftover slots to the largest remainders first.
	sort.Slice(shares, func(i, j int) bool {
		if shares[i].remainder != shares[j].remainder {
			return shares[i].remainder > shares[j].remainder
		}
		return shares[i].id < shares[j].id
	})
	for i := 0; i < leftover && i < len(shares); i++ {
		plan[shares[i].id]++
	}
}

func sum(p Plan) int {
	total := 0
	for _, v := range p {
		total += v
	}
	return total
}
