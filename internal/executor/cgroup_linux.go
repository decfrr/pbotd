package executor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func SetupCgroup(spec Spec, pid int) (string, error) {
	if spec.CgroupMode == "off" || spec.CgroupMode == "" {
		return "", nil
	}
	if spec.CgroupBase == "" {
		return "", fmt.Errorf("no delegated cgroup base configured")
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(spec.CgroupBase, &stat); err != nil {
		return "", err
	}
	if stat.Type != unix.CGROUP2_SUPER_MAGIC {
		return "", fmt.Errorf("cgroup base is not a cgroup v2 filesystem")
	}
	dir := filepath.Join(spec.CgroupBase, "pbotd-"+spec.AttemptID)
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	success := false
	defer func() {
		if !success {
			os.Remove(dir)
		}
	}()
	for _, setting := range []struct{ name, value string }{
		{"memory.max", strconv.FormatInt(spec.Job.Request.Memory, 10)},
		{"memory.oom.group", "1"},
		{"cpu.max", fmt.Sprintf("%d 100000", int64(spec.Job.Request.CPUs)*100000)},
		{"cgroup.procs", strconv.Itoa(pid)},
	} {
		if err := os.WriteFile(filepath.Join(dir, setting.name), []byte(setting.value), 0600); err != nil {
			return "", fmt.Errorf("configure cgroup %s: %w", setting.name, err)
		}
	}
	success = true
	return dir, nil
}

func CgroupProcesses(dir string) ([]int, error) {
	if dir == "" {
		return nil, nil
	}
	var pids []int
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
		if err != nil {
			return err
		}
		for _, field := range strings.Fields(string(body)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				return err
			}
			pids = append(pids, pid)
		}
		return nil
	})
	return pids, err
}

func SignalCgroup(dir string, signal unix.Signal) error {
	if dir == "" {
		return nil
	}
	if signal == unix.SIGKILL {
		if err := os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0600); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	pids, err := CgroupProcesses(dir)
	if err != nil {
		return err
	}
	for _, pid := range pids {
		p, err := Inspect(pid)
		if IsAbsent(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := SignalIdentity(p.Identity, signal); err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
	}
	return nil
}

func CleanupCgroup(dir string) error {
	if dir == "" {
		return nil
	}
	var dirs []string
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		return err
	}
	slices.Reverse(dirs)
	for _, path := range dirs {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func CgroupMemory(dir string) (int64, bool) {
	if dir == "" {
		return 0, false
	}
	peakText, _ := os.ReadFile(filepath.Join(dir, "memory.peak"))
	peak, _ := strconv.ParseInt(strings.TrimSpace(string(peakText)), 10, 64)
	events, _ := os.ReadFile(filepath.Join(dir, "memory.events"))
	for _, line := range strings.Split(string(events), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "oom_kill" {
			n, _ := strconv.Atoi(fields[1])
			return peak, n > 0
		}
	}
	return peak, false
}
