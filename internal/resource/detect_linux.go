package resource

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/decfrr/pbotd/internal/model"
	"golang.org/x/sys/unix"
)

func Detect() (model.Capacity, error) {
	var cap model.Capacity
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		return cap, err
	}
	for n := range 1024 {
		if set.IsSet(n) {
			cap.CPUSet = append(cap.CPUSet, n)
		}
	}
	cap.CPUs = len(cap.CPUSet)
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return cap, err
	}
	mem := uint64(info.Totalram) * uint64(info.Unit)
	if mem > math.MaxInt64 {
		return cap, fmt.Errorf("host memory exceeds supported range")
	}
	cap.Memory = int64(mem)
	for _, dir := range CgroupAncestors() {
		if body, err := os.ReadFile(filepath.Join(dir, "memory.max")); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64); err == nil && n > 0 && n < cap.Memory {
				cap.Memory = n
			}
		}
		if body, err := os.ReadFile(filepath.Join(dir, "cpu.max")); err == nil {
			fields := strings.Fields(string(body))
			if len(fields) != 2 {
				continue
			}
			quota, e1 := strconv.ParseInt(fields[0], 10, 64)
			period, e2 := strconv.ParseInt(fields[1], 10, 64)
			if e1 == nil && e2 == nil && quota > 0 && period > 0 {
				cap.CPUs = min(cap.CPUs, max(1, int(quota/period)))
			}
		}
	}
	if cap.CPUs == 0 {
		return cap, fmt.Errorf("no allowed CPUs")
	}
	return cap, nil
}

// CgroupAncestors returns the current unified hierarchy up to its visible mount root.
func CgroupAncestors() []string {
	body, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil
	}
	path := "/"
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "0::") {
			path = strings.TrimPrefix(line, "0::")
		}
	}
	root := "/sys/fs/cgroup"
	dir := filepath.Join(root, filepath.Clean("/"+path))
	if _, err := os.Stat(dir); err != nil {
		dir = root
	}
	var dirs []string
	for {
		dirs = append(dirs, dir)
		if dir == root {
			break
		}
		dir = filepath.Dir(dir)
	}
	return dirs
}
