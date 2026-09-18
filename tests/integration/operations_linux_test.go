package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/decfrr/pbotd/internal/executor"
	"github.com/decfrr/pbotd/internal/fsutil"
	"github.com/decfrr/pbotd/internal/ipc"
	"github.com/decfrr/pbotd/internal/model"
)

func (r *runtime) configure(old, new string) {
	r.t.Helper()
	body, err := os.ReadFile(r.paths.Config)
	if err != nil {
		r.t.Fatal(err)
	}
	if !strings.Contains(string(body), old) {
		r.t.Fatalf("configuration does not contain %q", old)
	}
	if err := os.WriteFile(r.paths.Config, []byte(strings.Replace(string(body), old, new, 1)), 0600); err != nil {
		r.t.Fatal(err)
	}
}

func TestSingletonDifferentSocketsAndDoctor(t *testing.T) {
	r := setup(t, 0)
	cmd := exec.Command(binary, "daemon")
	cmd.Env = append(r.env, "PBOTD_SOCKET="+filepath.Join(r.root, "other.sock"))
	if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "another daemon") {
		t.Fatalf("singleton: %s %v", output, err)
	}
	if _, err := r.call(ipc.Request{Operation: "status"}); err != nil {
		t.Fatal(err)
	}
	text := r.checked("doctor", "--json")
	if !strings.Contains(text, "quick_check ok") || !strings.Contains(text, "private") {
		t.Fatal(text)
	}
	if err := os.Chmod(r.paths.Socket, 0666); err != nil {
		t.Fatal(err)
	}
	if output, err := r.cli("doctor", "--json"); err == nil || !strings.Contains(output, "no group/other access") {
		t.Fatalf("missed insecure socket: %s %v", output, err)
	}
}

func TestAutostartRaceAndAliases(t *testing.T) {
	r := setup(t, 0)
	r.stop()
	r.configure("autostart=false", "autostart=true")
	script := filepath.Join(r.root, "held.pbs")
	if err := os.WriteFile(script, []byte("exit 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	const n = 8
	results := make(chan string, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { id, err := r.cli("qsub", "-h", script); results <- id; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]bool{}
	for id := range results {
		ids[id] = true
	}
	if len(ids) != n {
		t.Fatalf("duplicate submissions: %v", ids)
	}
	response, err := r.call(ipc.Request{Operation: "jobs", All: true})
	if err != nil || len(response.Jobs) != n {
		t.Fatalf("%+v %v", response, err)
	}
	alias := filepath.Join(r.root, "qstat")
	if err := os.Symlink(binary, alias); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(alias, "--json")
	cmd.Env = r.env
	body, err := cmd.CombinedOutput()
	if err != nil || len(decodeResponse(t, string(body)).Jobs) != n {
		t.Fatalf("alias: %s %v", body, err)
	}
}

func TestCompletionHookAtMostOnceAndTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "success"
		if timeout {
			name = "timeout"
		}
		t.Run(name, func(t *testing.T) {
			r := setup(t, 0)
			r.stop()
			script := "printf '%s:%s:%s' \"$PBS_JOBID\" \"$PBOTD_JOB_STATE\" \"$PBOTD_JOB_EXIT_CODE\" >> hook-count\n"
			if timeout {
				script += "trap '' TERM\nsleep 30 &\nwait\n"
			}
			hook := filepath.Join(r.root, "hook.sh")
			if err := os.WriteFile(hook, []byte(script), 0600); err != nil {
				t.Fatal(err)
			}
			r.configure("[execution]", "[execution]\ncompletion_hook=['/bin/bash', '"+hook+"']\nhook_timeout='300ms'")
			r.start("")
			id := r.submit("exit 7\n")
			r.waitState(id, model.Failed)
			r.until("hook launched", func() bool { return r.exists("hook-count") })
			r.stop()
			r.start("")
			want := "COMPLETED"
			if timeout {
				want = "TIMEOUT"
			}
			r.until("hook result", func() bool {
				response, err := r.call(ipc.Request{Operation: "jobs", IDs: []string{id}, Full: true})
				return err == nil && len(response.Attempts) == 1 && response.Attempts[0].HookState == want
			})
			body, err := os.ReadFile(filepath.Join(r.root, "hook-count"))
			if err != nil || string(body) != id+":FAILED:7" {
				t.Fatalf("hook duplicated or wrong environment: %q %v", body, err)
			}
			if r.job(id).State != model.Failed {
				t.Fatal("hook changed job result")
			}
		})
	}
}

func TestCPUAffinityAndCgroupFallback(t *testing.T) {
	for _, mode := range []string{"auto", "required"} {
		t.Run(mode, func(t *testing.T) {
			r := setup(t, 0)
			r.stop()
			r.configure("[execution]", "[execution]\ncpu_affinity=true\ncgroup='"+mode+"'\ncgroup_base='/nonexistent/pbotd-test-cgroup'")
			r.start("")
			id := r.submit("echo ready > ready\n" + gatedScript)
			if mode == "required" {
				r.waitState(id, model.Failed)
				if r.exists("ready") {
					t.Fatal("required enforcement silently bypassed")
				}
				return
			}
			r.until("affinity workload", func() bool { return r.exists("ready") })
			j := r.job(id)
			var started executor.Started
			dir := executor.AttemptDir(r.paths.State, j.ID, j.AttemptID)
			if err := fsutil.ReadJSON(filepath.Join(dir, "started.json"), &started); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(started.Workload.PID), "status"))
			if err != nil {
				t.Fatal(err)
			}
			want := "Cpus_allowed_list:\t" + strconv.Itoa(j.CPUSet[0]) + "\n"
			if !strings.Contains(string(body), want) {
				t.Fatalf("affinity mismatch; expected %q\n%s", want, body)
			}
			if _, err := os.Stat(filepath.Join(dir, "warnings.json")); err != nil {
				t.Fatal("missing cgroup fallback diagnostic")
			}
			r.release(id)
			r.waitState(id, model.Completed)
		})
	}
}
