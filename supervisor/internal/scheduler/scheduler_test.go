// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import "testing"

func total(p Plan) int {
	t := 0
	for _, v := range p {
		t += v
	}
	return t
}

func TestNoDemandNoWorkers(t *testing.T) {
	plan := Compute(nil, 1, 4)
	if len(plan) != 0 {
		t.Fatalf("expected empty plan, got %v", plan)
	}
}

func TestEveryActiveTypeGetsAtLeastOne(t *testing.T) {
	// One type floods the queue; others have a single task each. With enough
	// max capacity, none of the small types should be starved.
	demands := []Demand{
		{TaskTypeID: "big", Scheduled: 100},
		{TaskTypeID: "small1", Scheduled: 1},
		{TaskTypeID: "small2", Scheduled: 1},
	}
	plan := Compute(demands, 1, 4)
	if total(plan) != 4 {
		t.Fatalf("expected 4 total workers, got %d (%v)", total(plan), plan)
	}
	if plan["small1"] < 1 || plan["small2"] < 1 {
		t.Fatalf("small types starved: %v", plan)
	}
}

func TestCapAtMax(t *testing.T) {
	demands := []Demand{{TaskTypeID: "a", Scheduled: 1000}}
	plan := Compute(demands, 1, 4)
	if plan["a"] != 4 {
		t.Fatalf("expected cap at 4, got %v", plan)
	}
}

func TestNeverMoreWorkersThanTasks(t *testing.T) {
	demands := []Demand{{TaskTypeID: "a", Scheduled: 2}}
	plan := Compute(demands, 1, 10)
	if plan["a"] != 2 {
		t.Fatalf("expected 2 (limited by task count), got %v", plan)
	}
}

// Claiming a task flips it SCHEDULED -> RUNNING. If allocation counted only
// scheduled tasks the busy worker's own task would be invisible and the worker
// would be scaled away mid-task on the very next poll tick.
func TestRunningTaskKeepsItsWorker(t *testing.T) {
	plan := Compute([]Demand{{TaskTypeID: "a", Scheduled: 0, Running: 1, Workers: 1}}, 1, 4)
	if plan["a"] != 1 {
		t.Fatalf("expected the in-flight task to keep its worker, got %v", plan)
	}
}

// A RUNNING row with no worker behind it is an orphan: a crashed worker, or an
// async provider driven outside this supervisor. It must not create demand,
// otherwise the pool spawns workers for a type with nothing schedulable while
// types that do have pending tasks go unserved.
func TestOrphanedRunningTaskGetsNoWorker(t *testing.T) {
	plan := Compute([]Demand{
		{TaskTypeID: "ghost", Scheduled: 0, Running: 3, Workers: 0},
		{TaskTypeID: "real", Scheduled: 2},
	}, 1, 4)

	if plan["ghost"] != 0 {
		t.Errorf("orphaned RUNNING tasks must not get workers, got %v", plan)
	}
	if plan["real"] == 0 {
		t.Errorf("type with pending work was starved, got %v", plan)
	}
}

// A type with nothing scheduled and nothing running needs no workers, even
// though min is 1: min applies only to types that have work.
func TestIdleTypeGetsNoWorkers(t *testing.T) {
	plan := Compute([]Demand{{TaskTypeID: "a", Scheduled: 0, Running: 0}}, 1, 4)
	if len(plan) != 0 {
		t.Fatalf("expected empty plan when nothing is outstanding, got %v", plan)
	}
}

func TestProportionalDistribution(t *testing.T) {
	demands := []Demand{
		{TaskTypeID: "a", Scheduled: 30},
		{TaskTypeID: "b", Scheduled: 10},
	}
	plan := Compute(demands, 1, 8)
	if total(plan) != 8 {
		t.Fatalf("expected 8 total, got %d (%v)", total(plan), plan)
	}
	// "a" has 3x the demand of "b" so it should get more workers.
	if plan["a"] <= plan["b"] {
		t.Fatalf("expected a > b, got %v", plan)
	}
}
