package scheduler

import (
	"slices"
	"time"

	"github.com/decfrr/pbotd/internal/model"
)

type Allocation struct {
	JobID int64
	GPUs  []string
	CPUs  []int
}

type Plan struct {
	Start            []Allocation
	Pending          map[int64]string
	DependencyFailed []int64
}

// Decide is a pure admission pass. STARTING and EXITING retain their reservations.
// Device availability must already include foreign-process classification.
func Decide(jobs []model.Job, inventory model.Inventory, capacity model.Capacity, now time.Time, reservationAfter time.Duration, affinity bool) Plan {
	plan := Plan{Pending: make(map[int64]string)}
	jobs = append([]model.Job(nil), jobs...)
	slices.SortFunc(jobs, func(a, b model.Job) int {
		if c := a.QueuedAt.Compare(b.QueuedAt); c != 0 {
			return c
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	byID := make(map[string]model.Job)
	children := make(map[int64][]model.Job)
	for _, j := range jobs {
		if j.ParentID != 0 {
			children[j.ParentID] = append(children[j.ParentID], j)
		}
	}
	for _, j := range jobs {
		if j.ArrayParent {
			j = model.Aggregate(j, children[j.ID])
		}
		byID[j.PublicID] = j
	}
	freeCPU, freeMem := capacity.CPUs, capacity.Memory
	usedGPU := make(map[string]bool)
	usedCPU := make(map[int]bool)
	arrays := make(map[int64]int)
	for _, j := range jobs {
		if !j.ArrayParent && j.State.Active() {
			freeCPU -= j.Request.CPUs
			freeMem -= j.Request.Memory
			for _, id := range j.GPUs {
				usedGPU[id] = true
			}
			for _, id := range j.CPUSet {
				usedCPU[id] = true
			}
			if j.ParentID != 0 {
				arrays[j.ParentID]++
			}
		}
	}
	var freeGPU []string
	usableGPU := 0
	for _, d := range inventory.Devices {
		if d.Available {
			usableGPU++
			if !usedGPU[d.UUID] {
				freeGPU = append(freeGPU, d.UUID)
			}
		}
	}
	slices.Sort(freeGPU)
	var freeSet []int
	for _, cpu := range capacity.CPUSet {
		if !usedCPU[cpu] {
			freeSet = append(freeSet, cpu)
		}
	}
	var protected *model.Job
	for i := range jobs {
		j := &jobs[i]
		if j.ArrayParent || j.State.Terminal() || j.State.Active() {
			continue
		}
		reason := ""
		for _, dep := range j.Dependencies {
			prior, ok := byID[dep]
			if !ok || prior.State.Terminal() && prior.State != model.Completed {
				reason = "DependencyFailed"
				break
			}
			if prior.State != model.Completed {
				reason = "Dependency"
			}
		}
		if reason == "DependencyFailed" {
			plan.DependencyFailed = append(plan.DependencyFailed, j.ID)
			continue
		}
		if j.State == model.Held {
			plan.Pending[j.ID] = "JobHeldUser"
			continue
		}
		if reason != "" {
			plan.Pending[j.ID] = reason
			continue
		}
		if j.ArrayLimit > 0 && arrays[j.ParentID] >= j.ArrayLimit {
			plan.Pending[j.ID] = "ArrayLimit"
			continue
		}
		if protected == nil && reservationAfter > 0 && now.Sub(j.QueuedAt) >= reservationAfter && j.Request.CPUs <= capacity.CPUs && j.Request.Memory <= capacity.Memory && j.Request.GPUs <= usableGPU {
			copy := *j
			protected = &copy
		}
		availableCPU, availableMem, availableGPU := freeCPU, freeMem, len(freeGPU)
		if protected != nil && protected.ID != j.ID {
			availableCPU -= min(max(0, availableCPU), protected.Request.CPUs)
			availableMem -= min(max(int64(0), availableMem), protected.Request.Memory)
			availableGPU -= min(availableGPU, protected.Request.GPUs)
		}
		if j.Request.CPUs > availableCPU || j.Request.Memory > availableMem || j.Request.GPUs > availableGPU || affinity && j.Request.CPUs > len(freeSet) {
			reason = "Resources"
			if j.Request.GPUs > 0 && (!inventory.Known || j.Request.GPUs > usableGPU) {
				reason = "GPUUnavailable"
			}
			if protected != nil && protected.ID != j.ID {
				reason = "Reservation"
			}
			plan.Pending[j.ID] = reason
			continue
		}
		allocation := Allocation{JobID: j.ID, GPUs: append([]string(nil), freeGPU[:j.Request.GPUs]...)}
		freeGPU = freeGPU[j.Request.GPUs:]
		freeCPU -= j.Request.CPUs
		freeMem -= j.Request.Memory
		if affinity {
			allocation.CPUs = append([]int(nil), freeSet[:j.Request.CPUs]...)
			freeSet = freeSet[j.Request.CPUs:]
		}
		plan.Start = append(plan.Start, allocation)
		if j.ParentID != 0 {
			arrays[j.ParentID]++
		}
		if protected != nil && protected.ID == j.ID {
			protected = nil
		}
	}
	return plan
}
