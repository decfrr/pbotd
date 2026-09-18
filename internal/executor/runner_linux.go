package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/decfrr/pbotd/internal/fsutil"
	"github.com/decfrr/pbotd/internal/ipc"
	"github.com/decfrr/pbotd/internal/model"
	"golang.org/x/sys/unix"
)

func notify(spec Spec) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ipc.Call(ctx, spec.Socket, ipc.Request{Operation: "wake", AttemptID: spec.AttemptID})
}

func ReadControl(dir, attemptID string) (Control, error) {
	var control Control
	err := fsutil.ReadJSON(filepath.Join(dir, "control.json"), &control)
	if os.IsNotExist(err) {
		return Control{}, nil
	}
	if err != nil {
		return control, err
	}
	if control.AttemptID != attemptID {
		return Control{}, fmt.Errorf("control belongs to another attempt")
	}
	if control.Kind != "cancel" && control.Kind != "rerun" {
		return Control{}, fmt.Errorf("unknown control %q", control.Kind)
	}
	return control, nil
}

func Run(specPath string) error {
	var spec Spec
	if err := fsutil.ReadJSON(specPath, &spec); err != nil {
		return err
	}
	if spec.AttemptID == "" || spec.AttemptID != spec.Job.AttemptID {
		return fmt.Errorf("invalid runner attempt")
	}
	dir := filepath.Dir(specPath)
	lock, err := fsutil.Acquire(filepath.Join(dir, "runner.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	// Never repeat an already completed attempt, including direct internal invocation.
	if _, err := os.Stat(filepath.Join(dir, "result.json")); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return err
	}
	self, err := Inspect(os.Getpid())
	if err != nil {
		return err
	}
	if err := fsutil.WriteJSON(filepath.Join(dir, "runner.json"), self.Identity); err != nil {
		return err
	}
	notify(spec)
	result := model.Result{AttemptID: spec.AttemptID, State: model.Failed, Reason: "LaunchFailed"}
	for {
		control, err := ReadControl(dir, spec.AttemptID)
		if err != nil {
			return err
		}
		if control.Kind != "" {
			result.State = model.Cancelled
			result.Reason = "CancelledByUser"
			break
		}
		var auth Authorization
		err = fsutil.ReadJSON(filepath.Join(dir, "authorize.json"), &auth)
		if err == nil {
			if auth.AttemptID != spec.AttemptID {
				return fmt.Errorf("authorization belongs to another attempt")
			}
			result, err = execute(specPath, spec, self.Identity)
			if err != nil {
				if result.StartedAt != nil {
					return fmt.Errorf("workload cleanup requires reconciliation: %w", err)
				}
				result.State = model.Failed
				result.Reason = err.Error()
			}
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
	result.EndedAt = time.Now().UTC()
	if err := fsutil.WriteJSON(filepath.Join(dir, "result.json"), result); err != nil {
		return err
	}
	notify(spec)
	return nil
}

func execute(specPath string, spec Spec, runner model.Identity) (model.Result, error) {
	result := model.Result{AttemptID: spec.AttemptID, State: model.Failed, Reason: "LaunchFailed"}
	dir := filepath.Dir(specPath)
	for _, path := range []string{spec.Job.Stdout, spec.Job.Stderr} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return result, err
		}
	}
	stdout, err := os.OpenFile(spec.Job.Stdout, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return result, err
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(spec.Job.Stderr, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return result, err
	}
	defer stderr.Close()
	outInfo, e1 := stdout.Stat()
	errInfo, e2 := stderr.Stat()
	if e1 != nil {
		return result, e1
	}
	if e2 != nil {
		return result, e2
	}
	if os.SameFile(outInfo, errInfo) {
		stderr.Close()
		stderr = stdout
	}
	host, err := os.Hostname()
	if err != nil {
		return result, err
	}
	if err := fsutil.AtomicWrite(filepath.Join(dir, "nodes"), []byte(strings.Repeat(host+"\n", spec.Job.Request.CPUs))); err != nil {
		return result, err
	}
	gpuLines := ""
	if len(spec.Job.GPUs) > 0 {
		gpuLines = strings.Join(spec.Job.GPUs, "\n") + "\n"
	}
	if err := fsutil.AtomicWrite(filepath.Join(dir, "gpus"), []byte(gpuLines)); err != nil {
		return result, err
	}
	gateRead, gateWrite, err := os.Pipe()
	if err != nil {
		return result, err
	}
	defer gateWrite.Close()
	defer gateRead.Close()
	binary, err := os.Executable()
	if err != nil {
		return result, err
	}
	cmd := exec.Command(binary, "workload", specPath)
	cmd.Dir = spec.Job.CWD
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.ExtraFiles = []*os.File{gateRead}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return result, err
	}
	gateRead.Close()
	started := false
	defer func() {
		if !started {
			gateWrite.Close()
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	workload, err := Inspect(cmd.Process.Pid)
	if err != nil {
		return result, err
	}
	cgroup, err := SetupCgroup(spec, cmd.Process.Pid)
	if err != nil {
		if spec.CgroupMode == "required" {
			return result, err
		}
		if err := fsutil.WriteJSON(filepath.Join(dir, "warnings.json"), []string{"cgroup unavailable: " + err.Error()}); err != nil {
			return result, err
		}
	}
	if cgroup != "" {
		defer CleanupCgroup(cgroup)
	}
	at := time.Now().UTC()
	limit := time.Duration(spec.Job.Request.Walltime) * time.Second
	if spec.Timeout > 0 {
		limit = spec.Timeout
	}
	result.StartedAt = &at
	metadata := Started{AttemptID: spec.AttemptID, Runner: runner, Workload: workload.Identity, PGID: workload.PGID, At: at, Cgroup: cgroup}
	if err := fsutil.WriteJSON(filepath.Join(dir, "started.json"), metadata); err != nil {
		return result, err
	}
	if _, err := gateWrite.Write([]byte{'G'}); err != nil {
		return result, err
	}
	gateWrite.Close()
	started = true
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	notify(spec)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var stopAt time.Time
	var cause string
	mainDone := false
	for {
		select {
		case <-wait:
			mainDone = true
			wait = nil
		case <-tick.C:
		}
		now := time.Now()
		control, controlErr := ReadControl(dir, spec.AttemptID)
		if controlErr != nil && cause == "" {
			cause = "RunnerError"
		}
		if control.Kind != "" {
			if cause == "" || cause == "rerun" && control.Kind == "cancel" {
				cause = control.Kind
			}
		}
		if limit > 0 {
			deadline := at.Add(limit)
			if !now.Before(deadline) && !mainDone && (cause == "" || !control.At.IsZero() && control.At.After(deadline) && stopAt.IsZero()) {
				cause = "timeout"
			}
		}
		if mainDone || cause != "" {
			if stopAt.IsZero() {
				stopAt = now
			}
			signal := unix.SIGTERM
			if now.Sub(stopAt) >= spec.TermGrace {
				signal = unix.SIGKILL
			}
			if err := SignalGroup(metadata.PGID, metadata.Workload, signal); err != nil {
				return result, err
			}
			if err := SignalCgroup(cgroup, signal); err != nil {
				return result, err
			}
		}
		if !mainDone {
			continue
		}
		for {
			pid, err := unix.Wait4(-metadata.PGID, nil, unix.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
		}
		members, err := GroupMembers(metadata.PGID, metadata.Workload)
		if err != nil {
			return result, err
		}
		pids, err := CgroupProcesses(cgroup)
		if err != nil {
			return result, err
		}
		if len(members) == 0 && len(pids) == 0 {
			break
		}
	}
	code := cmd.ProcessState.ExitCode()
	if code >= 0 {
		result.ExitCode = &code
	}
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		result.Signal = int(status.Signal())
	}
	result.State = model.Completed
	result.Reason = "Completed"
	if code != 0 {
		result.State = model.Failed
		result.Reason = "NonZeroExitCode"
		if result.Signal != 0 {
			result.Reason = "Signal"
		}
	}
	switch cause {
	case "cancel":
		result.State = model.Cancelled
		result.Reason = "CancelledByUser"
	case "rerun":
		result.State = model.Cancelled
		result.Reason = "Rerun"
	case "timeout":
		result.State = model.Timeout
		result.Reason = "Walltime"
	case "RunnerError":
		result.State = model.Failed
		result.Reason = "InvalidControl"
	}
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_CHILDREN, &usage); err == nil {
		result.Usage.UserSeconds = float64(usage.Utime.Sec) + float64(usage.Utime.Usec)/1e6
		result.Usage.SystemSeconds = float64(usage.Stime.Sec) + float64(usage.Stime.Usec)/1e6
		result.Usage.MaxRSSBytes = usage.Maxrss * 1024
	}
	peak, oom := CgroupMemory(cgroup)
	result.Usage.MemoryPeakBytes = peak
	if oom && cause == "" {
		result.State = model.Failed
		result.Reason = "OutOfMemory"
	}
	return result, nil
}

// Workload waits behind the runner's durable start gate, then replaces itself.
func Workload(specPath string) error {
	runtime.LockOSThread()
	var spec Spec
	if err := fsutil.ReadJSON(specPath, &spec); err != nil {
		return err
	}
	gate := os.NewFile(3, "start gate")
	if gate == nil {
		return fmt.Errorf("missing start gate")
	}
	var token [1]byte
	_, err := io.ReadFull(gate, token[:])
	gate.Close()
	if err != nil || token[0] != 'G' {
		return fmt.Errorf("start gate closed without authorization")
	}
	if spec.Affinity {
		var set unix.CPUSet
		for _, cpu := range spec.Job.CPUSet {
			set.Set(cpu)
		}
		if len(spec.Job.CPUSet) != spec.Job.Request.CPUs {
			return fmt.Errorf("missing CPU allocation")
		}
		if err := unix.SchedSetaffinity(0, &set); err != nil {
			return err
		}
	}
	env := maps.Clone(spec.Job.Environment)
	if env == nil {
		env = make(map[string]string)
	}
	for key := range env {
		if strings.HasPrefix(key, "PBS_") || strings.HasPrefix(key, "PBOTD_") || key == "CUDA_VISIBLE_DEVICES" {
			delete(env, key)
		}
	}
	dir := filepath.Dir(specPath)
	maps.Copy(env, map[string]string{
		"PBS_JOBID": spec.Job.PublicID, "PBS_JOBNAME": spec.Job.Name, "PBS_QUEUE": spec.Job.Queue,
		"PBS_O_WORKDIR": spec.Job.CWD, "PBS_O_HOME": spec.Job.SubmitHome, "PBS_O_PATH": spec.Job.SubmitPath,
		"PBS_NCPUS": strconv.Itoa(spec.Job.Request.CPUs), "PBS_NGPUS": strconv.Itoa(spec.Job.Request.GPUs),
		"PBS_NODEFILE": filepath.Join(dir, "nodes"), "PBS_GPUFILE": filepath.Join(dir, "gpus"),
		"CUDA_VISIBLE_DEVICES": strings.Join(spec.Job.GPUs, ","), "PBOTD_GPU_UUIDS": strings.Join(spec.Job.GPUs, ","),
		"PBOTD_ATTEMPT_ID": spec.AttemptID,
	})
	if spec.Job.ArrayIndex != nil {
		env["PBS_ARRAY_INDEX"] = strconv.Itoa(*spec.Job.ArrayIndex)
	}
	if spec.HookResult != nil {
		env["PBOTD_JOB_STATE"] = string(spec.HookResult.State)
		env["PBOTD_JOB_REASON"] = spec.HookResult.Reason
		env["PBOTD_JOB_SIGNAL"] = strconv.Itoa(spec.HookResult.Signal)
		if spec.HookResult.ExitCode != nil {
			env["PBOTD_JOB_EXIT_CODE"] = strconv.Itoa(*spec.HookResult.ExitCode)
		}
	}
	var pairs []string
	for key, value := range env {
		pairs = append(pairs, key+"="+value)
	}
	slices.Sort(pairs)
	if len(spec.Command) > 0 {
		return unix.Exec(spec.Command[0], spec.Command, pairs)
	}
	return unix.Exec(spec.Shell, []string{spec.Shell, spec.Job.ScriptPath}, pairs)
}

// Spawn uses no daemon-owned context: runner lifetime is independent of the daemon.
func Spawn(specPath string) (*exec.Cmd, error) {
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(filepath.Dir(specPath), "runner.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	cmd := exec.Command(binary, "runner", specPath)
	cmd.Stdin = nil
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func IsAbsent(err error) bool { return errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) }
