package daemon

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/decfrr/pbotd/internal/ipc"
	"github.com/decfrr/pbotd/internal/model"
	"github.com/decfrr/pbotd/internal/pbs"
	"github.com/decfrr/pbotd/internal/store"
)

func (d *Daemon) dispatch(request ipc.Request) ipc.Response {
	switch request.Operation {
	case "submit":
		id, err := d.submit(request.Submission)
		if err != nil {
			return ipc.Failure(err, 2)
		}
		return ipc.Response{JobID: id}
	case "status", "nodes":
		node, err := d.node()
		if err != nil {
			return ipc.Failure(err, 1)
		}
		return ipc.Response{Node: &node}
	case "jobs":
		return d.query(request)
	case "wake":
		return ipc.Response{}
	case "delete", "hold", "release", "alter", "rerun":
		if len(request.IDs) == 0 {
			return ipc.Failure(fmt.Errorf("at least one job ID is required"), 2)
		}
		var failures []string
		for _, id := range request.IDs {
			if err := d.control(request.Operation, id, request.Args); err != nil {
				failures = append(failures, id+": "+err.Error())
			}
		}
		if len(failures) > 0 {
			return ipc.Failure(fmt.Errorf("%s", strings.Join(failures, "\n")), 1)
		}
		return ipc.Response{}
	default:
		return ipc.Failure(fmt.Errorf("unknown operation %q", request.Operation), 2)
	}
}

func (d *Daemon) node() (ipc.Node, error) {
	jobs, err := d.store.Jobs()
	if err != nil {
		return ipc.Node{}, err
	}
	host, err := os.Hostname()
	if err != nil {
		return ipc.Node{}, err
	}
	node := ipc.Node{Hostname: host, Capacity: d.capacity, Inventory: d.classifiedInventory(jobs), Counts: make(map[model.State]int)}
	for _, job := range jobs {
		if job.ArrayParent {
			continue
		}
		node.Counts[job.State]++
		if job.State.Active() {
			node.Reserved.CPUs += job.Request.CPUs
			node.Reserved.Memory += job.Request.Memory
			node.Reserved.GPUs += job.Request.GPUs
			for i := range node.Inventory.Devices {
				for _, uuid := range job.GPUs {
					if node.Inventory.Devices[i].UUID == uuid {
						node.Inventory.Devices[i].Available = false
						node.Inventory.Devices[i].AllocatedTo = job.PublicID
						node.Inventory.Devices[i].Reason = "Reserved"
					}
				}
			}
		}
	}
	return node, nil
}

func (d *Daemon) query(request ipc.Request) ipc.Response {
	jobs, err := d.store.Jobs()
	if err != nil {
		return ipc.Failure(err, 1)
	}
	children := make(map[int64][]model.Job)
	for _, job := range jobs {
		if job.ParentID != 0 {
			children[job.ParentID] = append(children[job.ParentID], job)
		}
	}
	selected := make(map[int64]bool)
	var failures []string
	for _, id := range request.IDs {
		id, err := model.NormalizeID(id)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		job, err := d.store.Find(id)
		if err != nil {
			failures = append(failures, "unknown job "+id)
			continue
		}
		selected[job.ID] = true
		if job.ArrayParent {
			for _, child := range children[job.ID] {
				selected[child.ID] = true
			}
		}
	}
	var response ipc.Response
	for _, job := range jobs {
		if job.ArrayParent {
			job = model.Aggregate(job, children[job.ID])
		}
		if len(request.IDs) > 0 {
			if !selected[job.ID] {
				continue
			}
		} else if job.ParentID != 0 || !request.All && job.State.Terminal() {
			continue
		}
		job.Environment = nil
		response.Jobs = append(response.Jobs, job)
		if request.Full {
			attempts, err := d.store.Attempts(job.ID)
			if err != nil {
				return ipc.Failure(err, 1)
			}
			response.Attempts = append(response.Attempts, attempts...)
			events, err := d.store.Events(job.ID)
			if err != nil {
				return ipc.Failure(err, 1)
			}
			response.Events = append(response.Events, events...)
		}
	}
	if len(failures) > 0 {
		response.Error = strings.Join(failures, "\n")
		response.Code = 1
	}
	return response
}

func (d *Daemon) control(operation, id string, args []string) error {
	canonical, err := model.NormalizeID(id)
	if err != nil {
		return err
	}
	job, err := d.store.Find(canonical)
	if err != nil {
		return fmt.Errorf("unknown job")
	}
	targets := []model.Job{job}
	if job.ArrayParent {
		if operation == "rerun" {
			return fmt.Errorf("qrerun requires an individual running job or subjob")
		}
		all, err := d.store.Jobs()
		if err != nil {
			return err
		}
		targets = nil
		for _, child := range all {
			if child.ParentID == job.ID {
				if operation == "alter" && (child.AttemptCount > 0 || child.State != model.Queued && child.State != model.Held) {
					return fmt.Errorf("array alteration requires all subjobs to be queued/held and never started")
				}
				if operation == "delete" && child.State.Terminal() {
					continue
				}
				if (operation == "hold" || operation == "release") && child.State != model.Queued && child.State != model.Held {
					continue
				}
				targets = append(targets, child)
			}
		}
	}
	var options pbs.Options
	if operation == "alter" {
		var operands []string
		options, operands, err = pbs.Parse(args, "Nl")
		if err != nil {
			return err
		}
		if len(operands) > 0 || len(options.Values) == 0 {
			return fmt.Errorf("qalter requires -N or -l")
		}
	}
	for i := range targets {
		j := &targets[i]
		switch operation {
		case "hold", "release":
			if j.State != model.Queued && j.State != model.Held {
				return fmt.Errorf("job must be queued or held")
			}
			if operation == "hold" {
				j.State = model.Held
				j.Reason = "JobHeldUser"
			} else {
				j.State = model.Queued
				j.Reason = ""
			}
		case "alter":
			if j.State != model.Queued && j.State != model.Held {
				return fmt.Errorf("job must be queued or held")
			}
			options.Values["q"] = j.Queue
			resolved, err := options.Resolve(j.Request, j.Name)
			if err != nil {
				return err
			}
			if err := d.validateCapacity(resolved.Request); err != nil {
				return err
			}
			j.Name = resolved.Name
			j.Request = resolved.Request
		case "rerun":
			if j.State != model.Running {
				return fmt.Errorf("job must be running")
			}
			j.State = model.Exiting
			j.Control = "rerun"
			j.Reason = "Rerun"
			now := time.Now().UTC()
			j.ControlAt = &now
		case "delete":
			if j.State == model.Cancelled {
				continue
			}
			if j.State.Terminal() {
				return fmt.Errorf("job has finished")
			}
			if j.State == model.Exiting && j.Control != "rerun" {
				continue
			}
			now := time.Now().UTC()
			if j.State == model.Queued || j.State == model.Held {
				j.State = model.Cancelled
				j.EndedAt = &now
				j.Reason = "CancelledByUser"
			} else {
				j.State = model.Exiting
				j.Control = "cancel"
				j.ControlAt = &now
				j.Reason = "CancelledByUser"
			}
		}
	}
	if err := d.store.Update(func(tx *store.Tx) error {
		for _, target := range targets {
			if err := tx.Save(target); err != nil {
				return err
			}
			if err := tx.Event(target, operation, target.Reason); err != nil {
				return err
			}
		}
		if job.ArrayParent && operation == "alter" {
			if len(targets) > 0 {
				job.Name = targets[0].Name
				job.Request = targets[0].Request
			}
			return tx.Save(job)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, target := range targets {
		if target.State == model.Exiting && (target.Control == "cancel" || target.Control == "rerun") {
			if err := d.publishControl(target); err != nil {
				return err
			}
		}
	}
	d.freshGPU = false
	d.requestProbe()
	return nil
}
