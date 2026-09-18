package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/decfrr/pbotd/internal/model"
	"github.com/decfrr/pbotd/internal/resource"
	"github.com/pelletier/go-toml/v2"
)

type Paths struct {
	Config string `json:"config"`
	State  string `json:"state"`
	Socket string `json:"socket"`
}

func ResolvePaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	host, err := os.Hostname()
	if err != nil {
		return Paths{}, err
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		config = filepath.Join(home, ".config")
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		state = filepath.Join(home, ".local", "state")
	}
	p := Paths{Config: filepath.Join(config, "pbotd", "config.toml"), State: filepath.Join(state, "pbotd", host)}
	if v := os.Getenv("PBOTD_CONFIG"); v != "" {
		p.Config = v
	}
	if v := os.Getenv("PBOTD_STATE_DIR"); v != "" {
		p.State = v
	}
	if runtime := os.Getenv("XDG_RUNTIME_DIR"); runtime != "" {
		p.Socket = filepath.Join(runtime, "pbotd", "pbotd.sock")
	} else {
		p.Socket = filepath.Join(p.State, "run", "pbotd.sock")
	}
	if v := os.Getenv("PBOTD_SOCKET"); v != "" {
		p.Socket = v
	}
	for _, path := range []*string{&p.Config, &p.State, &p.Socket} {
		*path, err = filepath.Abs(*path)
		if err != nil {
			return Paths{}, err
		}
	}
	return p, nil
}

type Config struct {
	Scheduler struct {
		Policy           string `toml:"policy"`
		Tick             string `toml:"tick"`
		ReservationAfter string `toml:"reservation_after"`
	} `toml:"scheduler"`
	Resources struct {
		CPUs          any    `toml:"cpus"`
		Memory        string `toml:"memory"`
		MemoryReserve string `toml:"memory_reserve"`
	} `toml:"resources"`
	Defaults struct {
		CPUs     int    `toml:"ncpus"`
		Memory   string `toml:"mem"`
		Walltime string `toml:"walltime"`
	} `toml:"defaults"`
	GPU struct {
		Provider      string   `toml:"provider"`
		ForeignPolicy string   `toml:"foreign_process_policy"`
		Allow         []string `toml:"allow_uuids"`
		Deny          []string `toml:"deny_uuids"`
		Static        []string `toml:"static_uuids"`
		Refresh       string   `toml:"refresh"`
		ProbeTimeout  string   `toml:"probe_timeout"`
		Command       string   `toml:"command"`
	} `toml:"gpu"`
	Execution struct {
		Shell       string   `toml:"shell"`
		TermGrace   string   `toml:"term_grace"`
		Cgroup      string   `toml:"cgroup"`
		CgroupBase  string   `toml:"cgroup_base"`
		Affinity    bool     `toml:"cpu_affinity"`
		Hook        []string `toml:"completion_hook"`
		HookTimeout string   `toml:"hook_timeout"`
	} `toml:"execution"`
	Daemon struct {
		Autostart bool `toml:"autostart"`
	} `toml:"daemon"`
	Tick             time.Duration   `toml:"-"`
	ReservationAfter time.Duration   `toml:"-"`
	Refresh          time.Duration   `toml:"-"`
	ProbeTimeout     time.Duration   `toml:"-"`
	TermGrace        time.Duration   `toml:"-"`
	HookTimeout      time.Duration   `toml:"-"`
	DefaultRequest   model.Resources `toml:"-"`
}

func Default() Config {
	var c Config
	c.Scheduler.Policy, c.Scheduler.Tick, c.Scheduler.ReservationAfter = "backfill", "1s", "2h"
	c.Resources.CPUs, c.Resources.Memory, c.Resources.MemoryReserve = "auto", "auto", "2GiB"
	c.Defaults.CPUs, c.Defaults.Memory, c.Defaults.Walltime = 1, "1GiB", "unlimited"
	c.GPU.Provider, c.GPU.ForeignPolicy, c.GPU.Refresh, c.GPU.ProbeTimeout = "nvidia-smi", "avoid", "5s", "2s"
	c.Execution.Shell, c.Execution.TermGrace = "/bin/bash", "10s"
	c.Execution.Cgroup, c.Execution.HookTimeout = "off", "10s"
	c.Daemon.Autostart = true
	return c
}

func Load(path string) (Config, error) {
	c := Default()
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return c, err
	}
	if err == nil {
		if err := toml.NewDecoder(bytes.NewReader(body)).DisallowUnknownFields().Decode(&c); err != nil {
			return c, fmt.Errorf("config %s: %w", path, err)
		}
	}
	err = c.Validate()
	return c, err
}

func (c *Config) Validate() error {
	for _, item := range []struct {
		value string
		out   *time.Duration
		zero  bool
	}{
		{c.Scheduler.Tick, &c.Tick, false}, {c.Scheduler.ReservationAfter, &c.ReservationAfter, true},
		{c.GPU.Refresh, &c.Refresh, false}, {c.GPU.ProbeTimeout, &c.ProbeTimeout, false},
		{c.Execution.TermGrace, &c.TermGrace, true}, {c.Execution.HookTimeout, &c.HookTimeout, false},
	} {
		d, err := time.ParseDuration(item.value)
		if err != nil || d < 0 || (!item.zero && d == 0) {
			return fmt.Errorf("invalid duration %q", item.value)
		}
		*item.out = d
	}
	if c.Scheduler.Policy != "backfill" {
		return fmt.Errorf("scheduler.policy must be backfill")
	}
	if c.GPU.Provider != "nvidia-smi" && c.GPU.Provider != "static" {
		return fmt.Errorf("gpu.provider must be nvidia-smi or static")
	}
	if c.GPU.ForeignPolicy != "avoid" && c.GPU.ForeignPolicy != "ignore" {
		return fmt.Errorf("gpu.foreign_process_policy must be avoid or ignore")
	}
	if c.Execution.Cgroup != "off" && c.Execution.Cgroup != "auto" && c.Execution.Cgroup != "required" {
		return fmt.Errorf("execution.cgroup must be off, auto, or required")
	}
	if c.Execution.Cgroup == "required" && c.Execution.CgroupBase == "" {
		return fmt.Errorf("execution.cgroup_base is required for cgroup enforcement")
	}
	if !filepath.IsAbs(c.Execution.Shell) {
		return fmt.Errorf("execution.shell must be an absolute path")
	}
	if len(c.Execution.Hook) > 0 && !filepath.IsAbs(c.Execution.Hook[0]) {
		return fmt.Errorf("execution.completion_hook must start with an absolute executable path")
	}
	if c.Execution.CgroupBase != "" && !filepath.IsAbs(c.Execution.CgroupBase) {
		return fmt.Errorf("execution.cgroup_base must be an absolute path")
	}
	for _, list := range [][]string{c.GPU.Allow, c.GPU.Deny, c.GPU.Static} {
		seen := make(map[string]bool)
		for _, uuid := range list {
			if !ValidUUID(uuid) || seen[uuid] {
				return fmt.Errorf("invalid or duplicate physical GPU UUID %q", uuid)
			}
			seen[uuid] = true
		}
	}
	if c.GPU.Provider == "static" && len(c.GPU.Static) == 0 {
		return fmt.Errorf("static GPU provider requires static_uuids")
	}
	switch v := c.Resources.CPUs.(type) {
	case string:
		if v != "auto" {
			return fmt.Errorf("resources.cpus must be auto or a positive integer")
		}
	case int64:
		if v < 1 || v > 1<<31-1 {
			return fmt.Errorf("invalid resources.cpus")
		}
	case int:
		if v < 1 {
			return fmt.Errorf("invalid resources.cpus")
		}
	default:
		return fmt.Errorf("resources.cpus must be auto or a positive integer")
	}
	if c.Resources.Memory != "auto" {
		n, err := resource.Memory(c.Resources.Memory)
		if err != nil || n == 0 {
			return fmt.Errorf("invalid resources.memory %q", c.Resources.Memory)
		}
	}
	if _, err := resource.Memory(c.Resources.MemoryReserve); err != nil {
		return err
	}
	mem, err := resource.Memory(c.Defaults.Memory)
	if err != nil {
		return err
	}
	c.DefaultRequest = model.Resources{CPUs: c.Defaults.CPUs, Memory: mem}
	if c.Defaults.Walltime != "unlimited" {
		c.DefaultRequest.Walltime, err = resource.Walltime(c.Defaults.Walltime)
		if err != nil {
			return err
		}
	}
	return c.DefaultRequest.Validate()
}

func ValidUUID(s string) bool {
	if len(s) != 40 || !strings.HasPrefix(s, "GPU-") {
		return false
	}
	for i, r := range s[4:] {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

func (c Config) Capacity() (model.Capacity, error) {
	cap, err := resource.Detect()
	if err != nil {
		return cap, err
	}
	switch n := c.Resources.CPUs.(type) {
	case int64:
		cap.CPUs = int(n)
	case int:
		cap.CPUs = n
	}
	if cap.CPUs > len(cap.CPUSet) {
		return cap, fmt.Errorf("configured CPUs exceed allowed CPU set")
	}
	if c.Resources.Memory == "auto" {
		reserve, _ := resource.Memory(c.Resources.MemoryReserve)
		cap.Memory -= reserve
	} else {
		cap.Memory, _ = resource.Memory(c.Resources.Memory)
	}
	if cap.Memory < c.DefaultRequest.Memory || cap.CPUs < c.DefaultRequest.CPUs {
		return cap, fmt.Errorf("default job does not fit configured capacity; adjust resources/defaults")
	}
	return cap, nil
}
