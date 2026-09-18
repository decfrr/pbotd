package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/decfrr/pbotd/internal/executor"
	"github.com/decfrr/pbotd/internal/fsutil"
	"github.com/decfrr/pbotd/internal/model"
	"github.com/decfrr/pbotd/internal/store"
	"golang.org/x/sys/unix"
)

func (d *Daemon) cleanOrphans() error {
	jobs, err := d.store.Jobs()
	if err != nil {
		return err
	}
	known := make(map[int64]bool)
	for _, j := range jobs {
		known[j.ID] = true
	}
	root := filepath.Join(d.paths.State, "jobs")
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil {
			continue
		}
		if !known[id] {
			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Daemon) reconcile() error {
	jobs, err := d.store.Jobs()
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.ArrayParent || !job.State.Active() {
			continue
		}
		attempt, err := d.store.Attempt(job.AttemptID)
		if err != nil {
			return err
		}
		dir := executor.AttemptDir(d.paths.State, job.ID, attempt.ID)
		var ready model.Identity
		if err := fsutil.ReadJSON(filepath.Join(dir, "runner.json"), &ready); err == nil {
			if executor.Alive(ready) {
				attempt.Runner = ready
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		var started executor.Started
		if err := fsutil.ReadJSON(filepath.Join(dir, "started.json"), &started); err == nil {
			if started.AttemptID != attempt.ID {
				return fmt.Errorf("job %s has mismatched launch metadata", job.PublicID)
			}
			attempt.Workload = started.Workload
			attempt.PGID = started.PGID
			attempt.StartedAt = &started.At
		} else if !os.IsNotExist(err) {
			return err
		}
		var result model.Result
		resultErr := fsutil.ReadJSON(filepath.Join(dir, "result.json"), &result)
		if resultErr == nil {
			if result.AttemptID != attempt.ID || !result.State.Terminal() {
				return fmt.Errorf("job %s has invalid result metadata", job.PublicID)
			}
			clean, err := workloadGone(started)
			if err != nil {
				return err
			}
			if !clean {
				continue
			}
			d.point("result")
			if err := d.finish(job, attempt, result); err != nil {
				return err
			}
			continue
		}
		if !os.IsNotExist(resultErr) {
			return resultErr
		}
		if executor.Alive(attempt.Runner) {
			if job.Control == "cancel" || job.Control == "rerun" {
				if err := d.publishControl(job); err != nil {
					return err
				}
			} else if !attempt.Authorized && ready.PID != 0 {
				attempt.Authorized = true
				if err := d.store.Update(func(tx *store.Tx) error { return tx.SaveAttempt(attempt) }); err != nil {
					return err
				}
				d.point("authorized")
			}
			if attempt.Authorized {
				if _, err := os.Stat(filepath.Join(dir, "authorize.json")); os.IsNotExist(err) {
					if err := fsutil.WriteJSON(filepath.Join(dir, "authorize.json"), executor.Authorization{AttemptID: attempt.ID}); err != nil {
						return err
					}
				}
			}
			if started.AttemptID != "" && job.State == model.Starting {
				job.State = model.Running
				job.StartedAt = &started.At
				if err := d.store.Update(func(tx *store.Tx) error {
					if err := tx.SaveAttempt(attempt); err != nil {
						return err
					}
					if err := tx.Save(job); err != nil {
						return err
					}
					return tx.Event(job, "Running", "")
				}); err != nil {
					return err
				}
				d.point("running")
			}
			continue
		}
		if !attempt.Authorized && started.AttemptID == "" {
			// A late, not-yet-ready runner must see cancellation before this reservation is released.
			if _, err := os.Stat(dir); err == nil {
				if err := fsutil.WriteJSON(filepath.Join(dir, "control.json"), executor.Control{AttemptID: attempt.ID, Kind: "cancel", At: time.Now().UTC()}); err != nil {
					return err
				}
			}
			if job.Control == "cancel" {
				if err := d.finish(job, attempt, model.Result{AttemptID: attempt.ID, State: model.Cancelled, Reason: "CancelledByUser", EndedAt: time.Now().UTC()}); err != nil {
					return err
				}
			} else {
				job.State = model.Queued
				job.GPUs = nil
				job.CPUSet = nil
				job.AttemptID = ""
				job.Reason = "InterruptedLaunch"
				if err := d.store.Update(func(tx *store.Tx) error {
					if err := tx.Release(job.ID, attempt.ID); err != nil {
						return err
					}
					if err := tx.Save(job); err != nil {
						return err
					}
					return tx.Event(job, "Requeued", "unauthorized launch interrupted")
				}); err != nil {
					return err
				}
			}
			continue
		}
		gone, err := workloadGone(started)
		if err != nil {
			return err
		}
		if !gone {
			if job.ControlAt == nil {
				now := time.Now().UTC()
				job.ControlAt = &now
				job.State = model.Exiting
				job.Reason = "LostRunner"
				if err := d.store.Update(func(tx *store.Tx) error { return tx.Save(job) }); err != nil {
					return err
				}
			}
			signal := unix.SIGTERM
			if time.Since(*job.ControlAt) >= d.config.TermGrace {
				signal = unix.SIGKILL
			}
			if err := executor.SignalGroup(started.PGID, started.Workload, signal); err != nil {
				return err
			}
			if err := executor.SignalCgroup(started.Cgroup, signal); err != nil {
				return err
			}
			continue
		}
		reason := "LostRunner"
		if started.AttemptID == "" {
			reason = "UnknownExecution"
		}
		if err := d.finish(job, attempt, model.Result{AttemptID: attempt.ID, State: model.Failed, Reason: reason, StartedAt: attempt.StartedAt, EndedAt: time.Now().UTC()}); err != nil {
			return err
		}
	}
	return d.reconcileHooks(jobs)
}

func workloadGone(started executor.Started) (bool, error) {
	if started.Workload.PID == 0 {
		return true, nil
	}
	self, err := executor.Inspect(os.Getpid())
	if err != nil {
		return false, err
	}
	if self.Identity.BootID != started.Workload.BootID {
		return true, nil
	}
	members, err := executor.GroupMembers(started.PGID, started.Workload)
	if err != nil {
		return false, err
	}
	if len(members) > 0 {
		return false, nil
	}
	pids, err := executor.CgroupProcesses(started.Cgroup)
	if os.IsNotExist(err) {
		return true, nil
	}
	return len(pids) == 0, err
}

func (d *Daemon) finish(job model.Job, attempt model.Attempt, result model.Result) error {
	if job.AttemptID != result.AttemptID {
		return nil
	}
	attempt.Result = &result
	rerun := job.Control == "rerun"
	if result.State == model.Timeout && result.StartedAt != nil && job.ControlAt != nil && !job.ControlAt.Before(result.StartedAt.Add(time.Duration(job.Request.Walltime)*time.Second)) {
		rerun = false
	}
	if job.ControlAt != nil && job.ControlAt.After(result.EndedAt) {
		rerun = false
	} else if job.Control == "cancel" {
		if result.State != model.Timeout || result.StartedAt == nil || job.ControlAt == nil || job.ControlAt.Before(result.StartedAt.Add(time.Duration(job.Request.Walltime)*time.Second)) {
			result.State = model.Cancelled
			result.Reason = "CancelledByUser"
		}
	}
	job.State = result.State
	job.ExitCode = result.ExitCode
	job.Signal = result.Signal
	job.Reason = result.Reason
	job.StartedAt = result.StartedAt
	job.EndedAt = &result.EndedAt
	job.GPUs = nil
	job.CPUSet = nil
	job.Control = ""
	job.ControlAt = nil
	if rerun {
		job.State = model.Queued
		job.QueuedAt = time.Now().UTC()
		job.StartedAt = nil
		job.EndedAt = nil
		job.ExitCode = nil
		job.Signal = 0
		job.Reason = "Rerun"
	}
	if err := d.store.Update(func(tx *store.Tx) error {
		if err := tx.SaveAttempt(attempt); err != nil {
			return err
		}
		if err := tx.Release(job.ID, attempt.ID); err != nil {
			return err
		}
		if err := tx.Save(job); err != nil {
			return err
		}
		return tx.Event(job, string(job.State), job.Reason)
	}); err != nil {
		return err
	}
	d.freshGPU = false
	d.requestProbe()
	d.point("finalized")
	return nil
}

func (d *Daemon) publishControl(job model.Job) error {
	if job.ControlAt == nil {
		return fmt.Errorf("missing durable control timestamp")
	}
	dir := executor.AttemptDir(d.paths.State, job.ID, job.AttemptID)
	if err := fsutil.PrivateDir(dir); err != nil {
		return err
	}
	return fsutil.WriteJSON(filepath.Join(dir, "control.json"), executor.Control{AttemptID: job.AttemptID, Kind: job.Control, At: *job.ControlAt})
}
