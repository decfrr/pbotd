package daemon

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/decfrr/pbotd/internal/executor"
	"github.com/decfrr/pbotd/internal/fsutil"
	"github.com/decfrr/pbotd/internal/model"
	"github.com/decfrr/pbotd/internal/store"
)

// Hooks are best effort, at most once per completed execution attempt. Claiming
// before spawn deliberately favors no duplicate side effects after a crash.
func (d *Daemon) reconcileHooks(jobs []model.Job) error {
	if len(d.config.Execution.Hook) == 0 {
		return nil
	}
	for _, job := range jobs {
		if job.ArrayParent {
			continue
		}
		attempts, err := d.store.Attempts(job.ID)
		if err != nil {
			return err
		}
		for _, attempt := range attempts {
			if attempt.Result == nil {
				continue
			}
			dir := filepath.Join(executor.AttemptDir(d.paths.State, job.ID, attempt.ID), "hook")
			if attempt.HookState == "claimed" {
				var result model.Result
				if err := fsutil.ReadJSON(filepath.Join(dir, "result.json"), &result); err == nil && result.AttemptID == attempt.ID {
					attempt.HookState = string(result.State)
					if err := d.store.Update(func(tx *store.Tx) error {
						if err := tx.SaveAttempt(attempt); err != nil {
							return err
						}
						job.AttemptID = attempt.ID
						return tx.Event(job, "CompletionHook", attempt.HookState)
					}); err != nil {
						return err
					}
				}
				continue
			}
			if attempt.HookState != "" {
				continue
			}
			if err := fsutil.PrivateDir(dir); err != nil {
				return err
			}
			hookJob := job
			hookJob.AttemptID = attempt.ID
			hookJob.GPUs = nil
			hookJob.Request = model.Resources{CPUs: 1, Memory: 1 << 20}
			hookJob.Environment = map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin"}
			hookJob.Stdout = filepath.Join(dir, "output.log")
			hookJob.Stderr = hookJob.Stdout
			spec := executor.Spec{Job: hookJob, AttemptID: attempt.ID, Shell: d.config.Execution.Shell, TermGrace: 100 * time.Millisecond, Socket: d.paths.Socket, CgroupMode: "off", Command: d.config.Execution.Hook, HookResult: attempt.Result, Timeout: d.config.HookTimeout}
			path := filepath.Join(dir, "spec.json")
			if err := fsutil.WriteJSON(path, spec); err != nil {
				return err
			}
			if err := fsutil.WriteJSON(filepath.Join(dir, "authorize.json"), executor.Authorization{AttemptID: attempt.ID}); err != nil {
				return err
			}
			attempt.HookState = "claimed"
			if err := d.store.Update(func(tx *store.Tx) error { return tx.SaveAttempt(attempt) }); err != nil {
				return err
			}
			cmd, err := executor.Spawn(path)
			if err != nil {
				attempt.HookState = "LaunchFailed"
				if err := d.store.Update(func(tx *store.Tx) error {
					if e := tx.SaveAttempt(attempt); e != nil {
						return e
					}
					job.AttemptID = attempt.ID
					return tx.Event(job, "CompletionHook", fmt.Sprint(err))
				}); err != nil {
					return err
				}
			} else {
				go func() {
					cmd.Wait()
					select {
					case d.wake <- struct{}{}:
					default:
					}
				}()
			}
		}
	}
	return nil
}
