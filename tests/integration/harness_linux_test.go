package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/decfrr/pbotd/internal/config"
	"github.com/decfrr/pbotd/internal/daemon"
	"github.com/decfrr/pbotd/internal/executor"
	"github.com/decfrr/pbotd/internal/fsutil"
	"github.com/decfrr/pbotd/internal/ipc"
	"github.com/decfrr/pbotd/internal/model"
)

var binary string

func TestMain(m *testing.M) {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "runner", "workload":
			var err error
			if os.Args[1] == "runner" {
				err = executor.Run(os.Args[2])
			} else {
				err = executor.Workload(os.Args[2])
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		case "daemon-helper":
			paths, err := config.ResolvePaths()
			if err != nil {
				panic(err)
			}
			c, err := config.Load(paths.Config)
			if err != nil {
				panic(err)
			}
			err = daemon.Run(context.Background(), c, paths, daemon.Options{Checkpoint: func(name string) {
				if name == os.Getenv("PBOTD_TEST_CHECKPOINT") {
					if err := os.WriteFile(filepath.Join(paths.State, "checkpoint"), []byte(name), 0600); err != nil {
						panic(err)
					}
					select {}
				}
			}})
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	dir, err := os.MkdirTemp("", "pbotd-integration-build-")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "pbotd")
	args := []string{"build", "-o", binary}
	if raceEnabled {
		args = append(args, "-race")
	}
	args = append(args, "./cmd/pbotd")
	cmd := exec.Command("go", args...)
	cmd.Dir = filepath.Join("..", "..")
	if output, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s", err, output)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type runtime struct {
	t      *testing.T
	root   string
	paths  config.Paths
	env    []string
	daemon *exec.Cmd
	seq    atomic.Int64
}

func setup(t *testing.T, gpus int) *runtime {
	t.Helper()
	root := t.TempDir()
	r := &runtime{t: t, root: root, paths: config.Paths{Config: filepath.Join(root, "config.toml"), State: filepath.Join(root, "state"), Socket: filepath.Join(root, "socket")}}
	text := "[resources]\ncpus=2\nmemory='1GiB'\n[defaults]\nmem='1MiB'\n[scheduler]\ntick='50ms'\n[execution]\nterm_grace='100ms'\n[daemon]\nautostart=false\n[gpu]\nforeign_process_policy='ignore'\nrefresh='100ms'\n"
	if gpus > 0 {
		var ids []string
		for i := range gpus {
			ids = append(ids, fmt.Sprintf("\"GPU-00000000-0000-0000-0000-%012d\"", i+1))
		}
		text += "provider='static'\nstatic_uuids=[" + strings.Join(ids, ",") + "]\n"
	} else {
		text += "command='/nonexistent/pbotd-test-nvidia-smi'\n"
	}
	if err := os.WriteFile(r.paths.Config, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	r.env = append(os.Environ(), "PBOTD_CONFIG="+r.paths.Config, "PBOTD_STATE_DIR="+r.paths.State, "PBOTD_SOCKET="+r.paths.Socket, "GORACE=atexit_sleep_ms=0")
	t.Cleanup(func() {
		if r.daemon != nil {
			response, err := r.call(ipc.Request{Operation: "jobs", All: true})
			if err == nil {
				for _, j := range response.Jobs {
					if !j.State.Terminal() {
						r.call(ipc.Request{Operation: "delete", IDs: []string{j.PublicID}})
					}
				}
			}
		}
		// Detached test workloads must not outlive their temporary runtime.
		files, _ := filepath.Glob(filepath.Join(r.paths.State, "jobs", "*", "attempts", "*", "started.json"))
		hooks, _ := filepath.Glob(filepath.Join(r.paths.State, "jobs", "*", "attempts", "*", "hook", "started.json"))
		files = append(files, hooks...)
		for _, file := range files {
			var start executor.Started
			if fsutil.ReadJSON(file, &start) == nil {
				executor.SignalGroup(start.PGID, start.Workload, 9)
				executor.SignalCgroup(start.Cgroup, 9)
			}
		}
		readyFiles, _ := filepath.Glob(filepath.Join(r.paths.State, "jobs", "*", "attempts", "*", "runner.json"))
		hookRunners, _ := filepath.Glob(filepath.Join(r.paths.State, "jobs", "*", "attempts", "*", "hook", "runner.json"))
		readyFiles = append(readyFiles, hookRunners...)
		for _, file := range readyFiles {
			var identity model.Identity
			if fsutil.ReadJSON(file, &identity) == nil {
				executor.SignalIdentity(identity, 9)
			}
		}
		r.stop()
		var daemonIdentity model.Identity
		if fsutil.ReadJSON(filepath.Join(r.paths.State, "daemon.json"), &daemonIdentity) == nil {
			executor.SignalIdentity(daemonIdentity, 9)
		}
	})
	r.start("")
	return r
}

func (r *runtime) start(checkpoint string) {
	r.t.Helper()
	bin, args := binary, []string{"daemon", "--foreground"}
	if checkpoint != "" {
		var err error
		bin, err = os.Executable()
		if err != nil {
			r.t.Fatal(err)
		}
		args = []string{"daemon-helper"}
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(r.env, "PBOTD_TEST_CHECKPOINT="+checkpoint)
	log, err := os.OpenFile(filepath.Join(r.root, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		r.t.Fatal(err)
	}
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		log.Close()
		r.t.Fatal(err)
	}
	log.Close()
	r.daemon = cmd
	r.until("daemon ready", func() bool { _, err := r.call(ipc.Request{Operation: "status"}); return err == nil })
}

func (r *runtime) stop() {
	if r.daemon == nil {
		return
	}
	r.daemon.Process.Kill()
	r.daemon.Wait()
	r.daemon = nil
}

func (r *runtime) call(request ipc.Request) (ipc.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := ipc.Call(ctx, r.paths.Socket, request)
	if err == nil && response.Error != "" {
		err = fmt.Errorf("%s", response.Error)
	}
	return response, err
}

func (r *runtime) cli(args ...string) (string, error) {
	cmd := exec.Command(binary, args...)
	cmd.Env = r.env
	cmd.Dir = r.root
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func (r *runtime) checked(args ...string) string {
	r.t.Helper()
	output, err := r.cli(args...)
	if err != nil {
		r.t.Fatalf("%v: %v\n%s\n%s", args, err, output, r.log())
	}
	return output
}

func (r *runtime) submit(script string, args ...string) string {
	r.t.Helper()
	file := filepath.Join(r.root, "script-"+strconv.FormatInt(r.seq.Add(1), 10)+".pbs")
	if err := os.WriteFile(file, []byte(script), 0600); err != nil {
		r.t.Fatal(err)
	}
	return r.checked(append(append([]string{"qsub"}, args...), file)...)
}

func (r *runtime) log() string {
	body, _ := os.ReadFile(filepath.Join(r.root, "daemon.log"))
	return string(body)
}

func (r *runtime) until(description string, check func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if check() {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("timeout: %s\n%s", description, r.log())
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func (r *runtime) job(id string) model.Job {
	r.t.Helper()
	response, err := r.call(ipc.Request{Operation: "jobs", IDs: []string{id}, Full: true})
	if err != nil || len(response.Jobs) == 0 {
		r.t.Fatalf("query %s: %+v %v\n%s", id, response, err, r.log())
	}
	return response.Jobs[0]
}

func (r *runtime) waitState(id string, state model.State) model.Job {
	r.t.Helper()
	var job model.Job
	r.until(id+" "+string(state), func() bool { job = r.job(id); return job.State == state })
	return job
}

func (r *runtime) exists(name string) bool {
	_, err := os.Stat(filepath.Join(r.root, name))
	return err == nil
}
func (r *runtime) release(id string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.root, "release."+id), nil, 0600); err != nil {
		r.t.Fatal(err)
	}
}

const gatedScript = `printf '%s' "$CUDA_VISIBLE_DEVICES" > "started.$PBS_JOBID"
while [ ! -e "release.$PBS_JOBID" ]; do sleep 0.02; done
printf '%s' "$PBOTD_ATTEMPT_ID" >> "finished.$PBS_JOBID"
`

func decodeResponse(t *testing.T, text string) ipc.Response {
	t.Helper()
	var response ipc.Response
	if err := json.Unmarshal([]byte(text), &response); err != nil {
		t.Fatal(err)
	}
	return response
}
