package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/decfrr/pbotd/internal/executor"
	"github.com/decfrr/pbotd/internal/fsutil"
	"github.com/decfrr/pbotd/internal/model"
)

func TestCrashBoundariesDoNotDuplicateExecution(t *testing.T) {
	for _, point := range []string{"reserved", "spawned", "authorized", "running", "result", "finalized"} {
		t.Run(point, func(t *testing.T) {
			r := setup(t, 1)
			r.stop()
			r.start(point)
			id := r.submit("printf x >> count\nexit 7\n", "-qgpu")
			r.until("checkpoint "+point, func() bool { return r.exists("state/checkpoint") })
			r.stop()
			r.start("")
			job := r.waitState(id, model.Failed)
			if job.ExitCode == nil || *job.ExitCode != 7 {
				t.Fatalf("lost actual result: %+v", job)
			}
			body, err := os.ReadFile(filepath.Join(r.root, "count"))
			if err != nil || string(body) != "x" {
				t.Fatalf("duplicate/missing execution: %q %v", body, err)
			}
			next := r.submit("exit 0\n", "-qgpu")
			r.waitState(next, model.Completed)
		})
	}
}

func TestTimeoutWhileDaemonStopped(t *testing.T) {
	r := setup(t, 1)
	id := r.submit("echo ready > ready\ntrap '' TERM\nsleep 30 &\nwait\n", "-qgpu", "-lwalltime=1")
	r.until("workload ready", func() bool { return r.exists("ready") })
	j := r.waitState(id, model.Running)
	r.stop()
	path := filepath.Join(executor.AttemptDir(r.paths.State, j.ID, j.AttemptID), "result.json")
	var result model.Result
	r.until("runner enforces walltime", func() bool { return fsutil.ReadJSON(path, &result) == nil })
	if result.State != model.Timeout {
		t.Fatalf("%+v", result)
	}
	r.start("")
	r.waitState(id, model.Timeout)
	r.waitState(r.submit("exit 0\n", "-qgpu"), model.Completed)
}

func TestLostRunnerTerminatesWorkloadBeforeRelease(t *testing.T) {
	r := setup(t, 1)
	id := r.submit("trap '' TERM\n"+gatedScript, "-qgpu")
	r.until("workload ready", func() bool { return r.exists("started." + id) })
	j := r.waitState(id, model.Running)
	var started executor.Started
	if err := fsutil.ReadJSON(filepath.Join(executor.AttemptDir(r.paths.State, j.ID, j.AttemptID), "started.json"), &started); err != nil {
		t.Fatal(err)
	}
	next := r.submit("exit 0\n", "-qgpu")
	if err := executor.SignalIdentity(started.Runner, 9); err != nil {
		t.Fatal(err)
	}
	failed := r.waitState(id, model.Failed)
	if failed.Reason != "LostRunner" || executor.Alive(started.Workload) {
		t.Fatalf("unsafe runner recovery: %+v", failed)
	}
	r.waitState(next, model.Completed)
}
