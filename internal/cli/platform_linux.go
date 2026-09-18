package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/decfrr/pbotd/internal/config"
	"github.com/decfrr/pbotd/internal/daemon"
	"github.com/decfrr/pbotd/internal/executor"
	"github.com/decfrr/pbotd/internal/fsutil"
	"github.com/decfrr/pbotd/internal/gpu"
	"github.com/decfrr/pbotd/internal/ipc"
	"github.com/decfrr/pbotd/internal/store"
)

func internalCommand(command string, args []string, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "internal command requires a spec path")
		return 2
	}
	var err error
	if command == "runner" {
		err = executor.Run(args[0])
	} else {
		err = executor.Workload(args[0])
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runDaemon(c config.Config, paths config.Paths) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return daemon.Run(ctx, c, paths, daemon.Options{})
}

func autostart(ctx context.Context, paths config.Paths) error {
	if err := fsutil.PrivateDir(paths.State); err != nil {
		return err
	}
	lock, err := fsutil.Acquire(filepath.Join(paths.State, "daemon.lock"))
	if err == nil {
		lock.Close()
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		log, err := os.OpenFile(filepath.Join(paths.State, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		defer log.Close()
		cmd := exec.Command(binary, "daemon", "--foreground")
		cmd.Stdout = log
		cmd.Stderr = log
		cmd.Stdin = nil
		cmd.Env = append(os.Environ(), "PBOTD_CONFIG="+paths.Config, "PBOTD_STATE_DIR="+paths.State, "PBOTD_SOCKET="+paths.Socket)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		go cmd.Wait()
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		return err
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		probe, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		response, err := ipc.Call(probe, paths.Socket, ipc.Request{Operation: "status"})
		cancel()
		if err == nil {
			if response.Error != "" {
				return fmt.Errorf("%s", response.Error)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("daemon did not become ready; inspect %s", filepath.Join(paths.State, "daemon.log"))
		case <-tick.C:
		}
	}
}

func doctor(c config.Config, paths config.Paths, args []string, out, stderr io.Writer) int {
	jsonOutput := len(args) == 1 && args[0] == "--json"
	if len(args) > 0 && !jsonOutput {
		fmt.Fprintln(stderr, "usage: pbotd doctor [--json]")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	checks := map[string]any{"config": paths.Config, "state": paths.State, "socket": paths.Socket, "configuration": "valid"}
	failed := false
	permissions := make(map[string]string)
	for _, path := range []string{paths.State, filepath.Dir(paths.Socket), paths.Socket, filepath.Join(paths.State, "pbotd.db")} {
		info, err := os.Lstat(path)
		if err != nil {
			permissions[path] = err.Error()
			failed = true
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0077 != 0 || info.Mode()&os.ModeSymlink != 0 {
			permissions[path] = "requires current UID ownership, no group/other access, and no symlink"
			failed = true
		} else {
			permissions[path] = "private"
		}
	}
	checks["permissions"] = permissions
	if err := store.Check(ctx, filepath.Join(paths.State, "pbotd.db")); err != nil {
		checks["database_error"] = err.Error()
		failed = true
	} else {
		checks["database"] = "schema 1; quick_check ok"
	}
	if c.Execution.Cgroup != "off" {
		checks["cgroup_mode"] = c.Execution.Cgroup
		body, err := os.ReadFile(filepath.Join(c.Execution.CgroupBase, "cgroup.subtree_control"))
		if err != nil {
			checks["cgroup_error"] = err.Error()
			if c.Execution.Cgroup == "required" {
				failed = true
			}
		} else {
			checks["cgroup_controllers"] = string(body)
		}
	}
	capacity, err := c.Capacity()
	if err != nil {
		checks["resources_error"] = err.Error()
	} else {
		checks["capacity"] = capacity
	}
	response, connectErr := ipc.Call(ctx, paths.Socket, ipc.Request{Operation: "status"})
	if connectErr == nil && response.Error != "" {
		connectErr = fmt.Errorf("%s", response.Error)
	}
	if connectErr != nil {
		checks["daemon_error"] = connectErr.Error()
		checks["gpu"] = gpu.New(c).Refresh(ctx)
	} else {
		checks["node"] = response.Node
	}
	if !jsonOutput {
		fmt.Fprintln(out, "pbotd diagnostic report:")
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if encodeErr := encoder.Encode(checks); encodeErr != nil {
		fmt.Fprintln(stderr, encodeErr)
		return 1
	}
	if failed || err != nil || connectErr != nil {
		return 1
	}
	return 0
}
