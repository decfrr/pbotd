package executor

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/decfrr/pbotd/internal/model"
	"golang.org/x/sys/unix"
)

type Process struct {
	Identity model.Identity
	PGID     int
	Parent   int
	State    byte
}

func Inspect(pid int) (Process, error) {
	var p Process
	if pid <= 0 {
		return p, fmt.Errorf("invalid PID %d", pid)
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return p, err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return p, err
	}
	return parseStat(pid, string(stat), strings.TrimSpace(string(boot)))
}

func parseStat(pid int, stat, boot string) (Process, error) {
	var p Process
	// comm may itself contain parentheses and spaces; fields start after the last ')'.
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return p, fmt.Errorf("malformed process stat")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 20 || len(fields[0]) != 1 {
		return p, fmt.Errorf("short process stat")
	}
	parent, e1 := strconv.Atoi(fields[1])
	pgid, e2 := strconv.Atoi(fields[2])
	start, e3 := strconv.ParseUint(fields[19], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return p, fmt.Errorf("malformed process identity")
	}
	return Process{Identity: model.Identity{PID: pid, StartTime: start, BootID: boot}, Parent: parent, PGID: pgid, State: fields[0][0]}, nil
}

func Alive(identity model.Identity) bool {
	p, err := Inspect(identity.PID)
	return err == nil && p.Identity == identity && p.State != 'Z' && p.State != 'X'
}

func GroupMembers(pgid int, leader model.Identity) ([]Process, error) {
	if pgid <= 1 || leader.PID != pgid || leader.StartTime == 0 {
		return nil, fmt.Errorf("invalid workload process group %d", pgid)
	}
	if current, err := Inspect(pgid); err == nil {
		if current.Identity != leader {
			return nil, nil
		}
	} else if !IsAbsent(err) {
		return nil, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var members []Process
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		p, err := Inspect(pid)
		if err != nil {
			if IsAbsent(err) || errors.Is(err, os.ErrPermission) {
				continue
			}
			return nil, err
		}
		if p.PGID == pgid && p.Identity.BootID == leader.BootID && p.Identity.StartTime >= leader.StartTime && p.State != 'Z' && p.State != 'X' {
			members = append(members, p)
		}
	}
	return members, nil
}

// SignalIdentity uses a pidfd to avoid signalling a reused PID after observation.
func SignalIdentity(identity model.Identity, signal unix.Signal) error {
	fd, err := unix.PidfdOpen(identity.PID, 0)
	if err == nil {
		defer unix.Close(fd)
		if !Alive(identity) {
			return nil
		}
		return unix.PidfdSendSignal(fd, signal, nil, 0)
	}
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL) {
		return err
	}
	if !Alive(identity) {
		return nil
	}
	return unix.Kill(identity.PID, signal)
}

func SignalGroup(pgid int, leader model.Identity, signal unix.Signal) error {
	members, err := GroupMembers(pgid, leader)
	if err != nil {
		return err
	}
	for _, p := range members {
		if err := SignalIdentity(p.Identity, signal); err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
	}
	return nil
}
