package scheduler

import (
	"testing"
	"time"

	"github.com/decfrr/pbotd/internal/model"
)

func TestBackfillAndStarvationProtection(t *testing.T) {
	now := time.Now()
	jobs := []model.Job{
		{ID: 1, PublicID: "1.localhost", State: model.Running, Request: model.Resources{CPUs: 1, Memory: 10, GPUs: 1}, GPUs: []string{"A"}},
		{ID: 2, PublicID: "2.localhost", State: model.Queued, QueuedAt: now.Add(-3 * time.Hour), Request: model.Resources{CPUs: 2, Memory: 20, GPUs: 2}},
		{ID: 3, PublicID: "3.localhost", State: model.Queued, QueuedAt: now.Add(-time.Minute), Request: model.Resources{CPUs: 1, Memory: 10, GPUs: 1}},
	}
	inv := model.Inventory{Known: true, Devices: []model.Device{{UUID: "A", Available: true}, {UUID: "B", Available: true}}}
	cap := model.Capacity{CPUs: 4, Memory: 100}
	plan := Decide(jobs, inv, cap, now, 0, false)
	if len(plan.Start) != 1 || plan.Start[0].JobID != 3 || plan.Start[0].GPUs[0] != "B" {
		t.Fatalf("backfill: %+v", plan)
	}
	plan = Decide(jobs, inv, cap, now, 2*time.Hour, false)
	if len(plan.Start) != 0 || plan.Pending[3] != "Reservation" {
		t.Fatalf("reservation: %+v", plan)
	}
	jobs[0].State = model.Completed
	plan = Decide(jobs, inv, cap, now, 2*time.Hour, false)
	if len(plan.Start) != 1 || plan.Start[0].JobID != 2 {
		t.Fatalf("protected job did not start: %+v", plan)
	}
}

func TestArrayLimitsDependenciesAndExitingResources(t *testing.T) {
	now := time.Now()
	jobs := []model.Job{
		{ID: 1, PublicID: "1.localhost", State: model.Failed},
		{ID: 2, PublicID: "2.localhost", State: model.Held, Dependencies: []string{"1.localhost"}},
		{ID: 3, PublicID: "3[].localhost", ArrayParent: true},
		{ID: 4, PublicID: "3[0].localhost", ParentID: 3, ArrayLimit: 1, State: model.Exiting, Request: model.Resources{CPUs: 1, Memory: 10}},
		{ID: 5, PublicID: "3[1].localhost", ParentID: 3, ArrayLimit: 1, State: model.Queued, Request: model.Resources{CPUs: 1, Memory: 10}},
		{ID: 6, PublicID: "6.localhost", State: model.Queued, Request: model.Resources{CPUs: 1, Memory: 10}, Dependencies: []string{"3[].localhost"}},
	}
	plan := Decide(jobs, model.Inventory{}, model.Capacity{CPUs: 4, Memory: 100}, now, 0, false)
	if len(plan.Start) != 0 || len(plan.DependencyFailed) != 1 || plan.DependencyFailed[0] != 2 || plan.Pending[5] != "ArrayLimit" || plan.Pending[6] != "Dependency" {
		t.Fatalf("%+v", plan)
	}
}

func TestCPUAndMemoryProtectionAlsoPreventsStarvation(t *testing.T) {
	now := time.Now()
	jobs := []model.Job{
		{ID: 1, State: model.Starting, Request: model.Resources{CPUs: 1, Memory: 60}},
		{ID: 2, State: model.Queued, QueuedAt: now.Add(-3 * time.Hour), Request: model.Resources{CPUs: 4, Memory: 90}},
		{ID: 3, State: model.Queued, QueuedAt: now.Add(-time.Minute), Request: model.Resources{CPUs: 1, Memory: 20}},
	}
	plan := Decide(jobs, model.Inventory{}, model.Capacity{CPUs: 4, Memory: 100}, now, 2*time.Hour, false)
	if len(plan.Start) != 0 {
		t.Fatalf("protected CPU/memory consumed: %+v", plan)
	}
}
