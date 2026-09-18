package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/decfrr/pbotd/internal/config"
	"github.com/decfrr/pbotd/internal/executor"
	"github.com/decfrr/pbotd/internal/fsutil"
	"github.com/decfrr/pbotd/internal/gpu"
	"github.com/decfrr/pbotd/internal/ipc"
	"github.com/decfrr/pbotd/internal/model"
	"github.com/decfrr/pbotd/internal/scheduler"
	"github.com/decfrr/pbotd/internal/store"
)

type Options struct {
	// Checkpoint permits deterministic crash tests without adding a public fault API.
	Checkpoint func(string)
}

type call struct {
	request ipc.Request
	reply   chan ipc.Response
}

type Daemon struct {
	config     config.Config
	paths      config.Paths
	store      *store.Store
	capacity   model.Capacity
	inventory  model.Inventory
	freshGPU   bool
	requests   chan call
	probe      chan struct{}
	probes     chan model.Inventory
	wake       chan struct{}
	checkpoint func(string)
	ctx        context.Context
}

func Run(ctx context.Context, c config.Config, paths config.Paths, options Options) error {
	capacity, err := c.Capacity()
	if err != nil {
		return err
	}
	if err := fsutil.PrivateDir(paths.State); err != nil {
		return err
	}
	lock, err := fsutil.Acquire(filepath.Join(paths.State, "daemon.lock"))
	if err != nil {
		return fmt.Errorf("another daemon owns this state directory: %w", err)
	}
	defer lock.Close()
	self, err := executor.Inspect(os.Getpid())
	if err != nil {
		return err
	}
	if err := fsutil.WriteJSON(filepath.Join(paths.State, "daemon.json"), self.Identity); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Join(paths.State, "jobs"), filepath.Dir(paths.Socket)} {
		if err := fsutil.PrivateDir(dir); err != nil {
			return err
		}
	}
	db, err := store.Open(filepath.Join(paths.State, "pbotd.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d := &Daemon{config: c, paths: paths, store: db, capacity: capacity, requests: make(chan call, 64), probe: make(chan struct{}, 1), probes: make(chan model.Inventory, 1), wake: make(chan struct{}, 1), checkpoint: options.Checkpoint, ctx: ctx}
	if err := d.cleanOrphans(); err != nil {
		return err
	}
	if err := d.reconcile(); err != nil {
		return err
	}
	if err := os.Remove(paths.Socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: paths.Socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(paths.Socket, 0600); err != nil {
		return err
	}
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- ipc.Serve(ctx, listener, func(ctx context.Context, request ipc.Request) ipc.Response {
			pending := call{request: request, reply: make(chan ipc.Response, 1)}
			select {
			case d.requests <- pending:
			case <-ctx.Done():
				return ipc.Failure(ctx.Err(), 1)
			}
			select {
			case response := <-pending.reply:
				return response
			case <-ctx.Done():
				return ipc.Failure(ctx.Err(), 1)
			}
		})
	}()
	go d.inventoryWorker()
	d.requestProbe()
	tick := time.NewTicker(c.Tick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serverErrors:
			return err
		case inventory := <-d.probes:
			d.inventory = inventory
			d.freshGPU = true
		case pending := <-d.requests:
			if err := d.reconcile(); err != nil {
				return err
			}
			pending.reply <- d.dispatch(pending.request)
		case <-d.wake:
		case <-tick.C:
		}
		if err := d.reconcile(); err != nil {
			return err
		}
		if err := d.schedule(); err != nil {
			return err
		}
	}
}

func (d *Daemon) point(name string) {
	if d.checkpoint != nil {
		d.checkpoint(name)
	}
}
func (d *Daemon) requestProbe() {
	select {
	case d.probe <- struct{}{}:
	default:
	}
}

func (d *Daemon) inventoryWorker() {
	provider := gpu.New(d.config)
	ticker := time.NewTicker(d.config.Refresh)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
		case <-d.probe:
		}
		inventory := provider.Refresh(d.ctx)
		select {
		case d.probes <- inventory:
		case <-d.ctx.Done():
			return
		}
	}
}

func (d *Daemon) classifiedInventory(jobs []model.Job) model.Inventory {
	inv := d.inventory
	inv.Devices = append([]model.Device(nil), inv.Devices...)
	groups := make(map[int]model.Identity)
	for _, j := range jobs {
		if j.State.Active() && !j.ArrayParent {
			var metadata executor.Started
			if fsutil.ReadJSON(filepath.Join(executor.AttemptDir(d.paths.State, j.ID, j.AttemptID), "started.json"), &metadata) == nil && metadata.AttemptID == j.AttemptID {
				groups[metadata.PGID] = metadata.Workload
			}
		}
	}
	for i := range inv.Devices {
		for _, pid := range inv.Devices[i].Processes {
			p, err := executor.Inspect(pid)
			if executor.IsAbsent(err) {
				continue
			}
			owned := false
			if err == nil {
				if leader, ok := groups[p.PGID]; ok && p.Identity.BootID == leader.BootID && p.Identity.StartTime >= leader.StartTime {
					current, leaderErr := executor.Inspect(leader.PID)
					owned = executor.IsAbsent(leaderErr) || leaderErr == nil && current.Identity == leader
				}
			}
			if !owned {
				inv.Devices[i].Available = false
				inv.Devices[i].Reason = "ForeignProcess"
				break
			}
		}
	}
	return inv
}

func (d *Daemon) schedule() error {
	jobs, err := d.store.Jobs()
	if err != nil {
		return err
	}
	plan := scheduler.Decide(jobs, d.classifiedInventory(jobs), d.capacity, time.Now(), d.config.ReservationAfter, d.config.Execution.Affinity)
	byID := make(map[int64]model.Job)
	for _, j := range jobs {
		byID[j.ID] = j
	}
	for _, id := range plan.DependencyFailed {
		j := byID[id]
		j.State = model.Failed
		j.Reason = "DependencyFailed"
		now := time.Now().UTC()
		j.EndedAt = &now
		if err := d.store.Update(func(tx *store.Tx) error {
			if err := tx.Save(j); err != nil {
				return err
			}
			return tx.Event(j, "Failed", j.Reason)
		}); err != nil {
			return err
		}
	}
	for id, reason := range plan.Pending {
		j := byID[id]
		if j.Reason == reason {
			continue
		}
		j.Reason = reason
		if err := d.store.Update(func(tx *store.Tx) error { return tx.Save(j) }); err != nil {
			return err
		}
	}
	for _, allocation := range plan.Start {
		if len(allocation.GPUs) > 0 && d.config.GPU.ForeignPolicy == "avoid" && !d.freshGPU {
			d.requestProbe()
			continue
		}
		job, attempt, err := d.store.Reserve(allocation.JobID, allocation.GPUs, allocation.CPUs, d.capacity)
		if err != nil {
			return err
		}
		d.point("reserved")
		if len(allocation.GPUs) > 0 {
			d.freshGPU = false
			d.requestProbe()
		}
		if err := d.launch(job, attempt); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) launch(job model.Job, attempt model.Attempt) error {
	dir := executor.AttemptDir(d.paths.State, job.ID, attempt.ID)
	launchErr := fsutil.PrivateDir(dir)
	if launchErr == nil {
		spec := executor.Spec{Job: job, AttemptID: attempt.ID, Shell: d.config.Execution.Shell, TermGrace: d.config.TermGrace, Socket: d.paths.Socket, CgroupMode: d.config.Execution.Cgroup, CgroupBase: d.config.Execution.CgroupBase, Affinity: d.config.Execution.Affinity}
		launchErr = fsutil.WriteJSON(filepath.Join(dir, "spec.json"), spec)
	}
	if launchErr == nil {
		cmd, err := executor.Spawn(filepath.Join(dir, "spec.json"))
		launchErr = err
		if err == nil {
			go func() {
				if err := cmd.Wait(); err != nil {
					slog.Warn("runner exited", "job", job.PublicID, "attempt", attempt.ID, "error", err)
				}
				select {
				case d.wake <- struct{}{}:
				default:
				}
			}()
			d.point("spawned")
			if process, err := executor.Inspect(cmd.Process.Pid); err == nil {
				attempt.Runner = process.Identity
			}
			return d.store.Update(func(tx *store.Tx) error { return tx.SaveAttempt(attempt) })
		}
	}
	return d.finish(job, attempt, model.Result{AttemptID: attempt.ID, State: model.Failed, Reason: "LaunchFailed: " + launchErr.Error(), EndedAt: time.Now().UTC()})
}
