package executor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/decfrr/pbotd/internal/fsutil"
	"github.com/decfrr/pbotd/internal/model"
	"github.com/google/uuid"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && (os.Args[1] == "runner" || os.Args[1] == "workload") {
		var err error
		if os.Args[1] == "runner" {
			err = Run(os.Args[2])
		} else {
			err = Workload(os.Args[2])
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runnerFixture(t *testing.T, script string, walltime int64) (Spec, string) {
	t.Helper()
	root := t.TempDir()
	attempt := uuid.NewString()
	dir := filepath.Join(root, "attempt")
	if err := fsutil.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(root, "snapshot.pbs")
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	spec := Spec{AttemptID: attempt, Shell: "/bin/bash", TermGrace: 100 * time.Millisecond, CgroupMode: "off", Socket: filepath.Join(root, "absent.sock"), Job: model.Job{
		ID: 1, PublicID: "1.localhost", Name: "test", Queue: "cpu", AttemptID: attempt, CWD: root, ScriptPath: scriptPath,
		Stdout: filepath.Join(root, "logs", "out"), Stderr: filepath.Join(root, "logs", "err"),
		Request: model.Resources{CPUs: 1, Memory: 1 << 20, Walltime: walltime}, Environment: map[string]string{"PATH": "/usr/bin:/bin", "CUDA_VISIBLE_DEVICES": "bad", "PBS_JOBID": "bad"},
	}}
	path := filepath.Join(dir, "spec.json")
	if err := fsutil.WriteJSON(path, spec); err != nil {
		t.Fatal(err)
	}
	return spec, path
}

func waitFile(t *testing.T, path string, target any) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		err := fsutil.ReadJSON(path, target)
		if err == nil {
			return
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startTestRunner(t *testing.T, specPath string) *exec.Cmd {
	t.Helper()
	cmd, err := Spawn(specPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			var started Started
			if fsutil.ReadJSON(filepath.Join(filepath.Dir(specPath), "started.json"), &started) == nil {
				SignalGroup(started.PGID, started.Workload, 9)
				SignalCgroup(started.Cgroup, 9)
			}
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return cmd
}

func authorize(t *testing.T, spec Spec, path string) {
	t.Helper()
	if err := fsutil.WriteJSON(filepath.Join(filepath.Dir(path), "authorize.json"), Authorization{AttemptID: spec.AttemptID}); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerWaitsForAuthorizationAndRejectsDuplicate(t *testing.T) {
	spec, path := runnerFixture(t, "printf executed >> marker\n", 0)
	cmd := startTestRunner(t, path)
	var identity model.Identity
	waitFile(t, filepath.Join(filepath.Dir(path), "runner.json"), &identity)
	if _, err := os.Stat(filepath.Join(spec.Job.CWD, "marker")); !os.IsNotExist(err) {
		t.Fatal("workload ran before authorization")
	}
	duplicate, err := Spawn(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := duplicate.Wait(); err == nil {
		t.Fatal("second runner acquired same attempt")
	}
	authorize(t, spec, path)
	var result model.Result
	waitFile(t, filepath.Join(filepath.Dir(path), "result.json"), &result)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(filepath.Join(spec.Job.CWD, "marker"))
	if err != nil || string(marker) != "executed" {
		t.Fatalf("%q %v", marker, err)
	}
	if result.State != model.Completed || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("%+v", result)
	}
	duplicate, err = Spawn(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := duplicate.Wait(); err != nil {
		t.Fatal(err)
	}
	marker, _ = os.ReadFile(filepath.Join(spec.Job.CWD, "marker"))
	if string(marker) != "executed" {
		t.Fatal("completed attempt executed twice")
	}
}

func TestRunnerResultsEnvironmentAndTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		walltime     int64
		state        model.State
		code         int
	}{
		{"success", "printf '%s:%s:%s' \"$PBS_JOBID\" \"$PBS_NCPUS\" \"$CUDA_VISIBLE_DEVICES\"\n", 0, model.Completed, 0},
		{"failed", "exit 7\n", 0, model.Failed, 7},
		{"timeout", "sleep 30\n", 1, model.Timeout, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, path := runnerFixture(t, tc.script, tc.walltime)
			cmd := startTestRunner(t, path)
			authorize(t, spec, path)
			var result model.Result
			waitFile(t, filepath.Join(filepath.Dir(path), "result.json"), &result)
			if err := cmd.Wait(); err != nil {
				log, _ := os.ReadFile(filepath.Join(filepath.Dir(path), "runner.log"))
				t.Fatalf("%v: %s", err, log)
			}
			if result.State != tc.state {
				t.Fatalf("%+v", result)
			}
			if tc.code >= 0 && (result.ExitCode == nil || *result.ExitCode != tc.code) {
				t.Fatalf("%+v", result)
			}
			if tc.name == "success" {
				body, _ := os.ReadFile(spec.Job.Stdout)
				if string(body) != "1.localhost:1:" {
					t.Fatalf("environment output %q", body)
				}
			}
		})
	}
}

func TestCancellationKillsTERMResistantDescendantsBeforeResult(t *testing.T) {
	spec, path := runnerFixture(t, "trap '' TERM\nsleep 30 &\necho $! > child\nwait\n", 0)
	cmd := startTestRunner(t, path)
	authorize(t, spec, path)
	var started Started
	waitFile(t, filepath.Join(filepath.Dir(path), "started.json"), &started)
	childPath := filepath.Join(spec.Job.CWD, "child")
	deadline := time.Now().Add(5 * time.Second)
	var childIdentity model.Identity
	for {
		body, err := os.ReadFile(childPath)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
			if err == nil {
				p, err := Inspect(pid)
				if err == nil {
					childIdentity = p.Identity
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := fsutil.WriteJSON(filepath.Join(filepath.Dir(path), "control.json"), Control{AttemptID: spec.AttemptID, Kind: "cancel", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	var result model.Result
	waitFile(t, filepath.Join(filepath.Dir(path), "result.json"), &result)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if result.State != model.Cancelled || Alive(started.Workload) || Alive(childIdentity) {
		t.Fatalf("premature or incorrect completion: %+v", result)
	}
}

func TestBackgroundChildCleanup(t *testing.T) {
	spec, path := runnerFixture(t, "sleep 30 &\necho $! > child\nexit 0\n", 0)
	cmd := startTestRunner(t, path)
	authorize(t, spec, path)
	var result model.Result
	waitFile(t, filepath.Join(filepath.Dir(path), "result.json"), &result)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if result.State != model.Completed {
		t.Fatalf("%+v", result)
	}
	var started Started
	waitFile(t, filepath.Join(filepath.Dir(path), "started.json"), &started)
	members, err := GroupMembers(started.PGID, started.Workload)
	if err != nil || len(members) != 0 {
		t.Fatalf("background processes survived: %v %v", members, err)
	}
}
