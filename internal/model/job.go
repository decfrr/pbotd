package model

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

type State string

const (
	Queued    State = "QUEUED"
	Held      State = "HELD"
	Starting  State = "STARTING"
	Running   State = "RUNNING"
	Exiting   State = "EXITING"
	Completed State = "COMPLETED"
	Failed    State = "FAILED"
	Cancelled State = "CANCELLED"
	Timeout   State = "TIMEOUT"
)

func (s State) Active() bool { return s == Starting || s == Running || s == Exiting }
func (s State) Terminal() bool {
	return s == Completed || s == Failed || s == Cancelled || s == Timeout
}
func (s State) Code() string {
	switch s {
	case Queued, Starting:
		return "Q"
	case Held:
		return "H"
	case Running:
		return "R"
	case Exiting:
		return "E"
	default:
		return "F"
	}
}

type Resources struct {
	CPUs     int   `json:"ncpus"`
	Memory   int64 `json:"mem_bytes"`
	GPUs     int   `json:"ngpus"`
	Walltime int64 `json:"walltime_sec"`
}

func (r Resources) Validate() error {
	if r.CPUs < 1 || r.Memory < 1 || r.GPUs < 0 || r.Walltime < 0 {
		return fmt.Errorf("ncpus and mem must be positive; ngpus and walltime must be nonnegative")
	}
	return nil
}

type Job struct {
	ID           int64             `json:"id"`
	PublicID     string            `json:"job_id"`
	Name         string            `json:"name"`
	State        State             `json:"state"`
	Queue        string            `json:"queue"`
	Request      Resources         `json:"resources"`
	CWD          string            `json:"cwd"`
	ScriptPath   string            `json:"script_path"`
	Environment  map[string]string `json:"environment,omitempty"`
	SubmitHome   string            `json:"submit_home"`
	SubmitPath   string            `json:"submit_path"`
	Stdout       string            `json:"stdout"`
	Stderr       string            `json:"stderr"`
	SubmittedAt  time.Time         `json:"submitted_at"`
	QueuedAt     time.Time         `json:"queued_at"`
	StartedAt    *time.Time        `json:"started_at,omitempty"`
	EndedAt      *time.Time        `json:"ended_at,omitempty"`
	AttemptID    string            `json:"attempt_id,omitempty"`
	AttemptCount int               `json:"attempt_count"`
	GPUs         []string          `json:"gpu_uuids,omitempty"`
	CPUSet       []int             `json:"cpu_set,omitempty"`
	ExitCode     *int              `json:"exit_code,omitempty"`
	Signal       int               `json:"signal,omitempty"`
	Reason       string            `json:"reason,omitempty"`
	Control      string            `json:"control,omitempty"`
	ControlAt    *time.Time        `json:"control_at,omitempty"`
	ParentID     int64             `json:"parent_id,omitempty"`
	ArrayIndex   *int              `json:"array_index,omitempty"`
	ArrayParent  bool              `json:"array_parent,omitempty"`
	ArrayLimit   int               `json:"array_limit,omitempty"`
	Dependencies []string          `json:"dependencies,omitempty"`
	StateCounts  map[State]int     `json:"state_counts,omitempty"`
}

type Identity struct {
	PID       int    `json:"pid"`
	StartTime uint64 `json:"start_time"`
	BootID    string `json:"boot_id"`
}

type Usage struct {
	UserSeconds     float64 `json:"user_seconds"`
	SystemSeconds   float64 `json:"system_seconds"`
	MaxRSSBytes     int64   `json:"max_rss_bytes"`
	MemoryPeakBytes int64   `json:"memory_peak_bytes,omitempty"`
}

type Result struct {
	AttemptID string     `json:"attempt_id"`
	State     State      `json:"state"`
	Reason    string     `json:"reason"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Signal    int        `json:"signal,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   time.Time  `json:"ended_at"`
	Usage     Usage      `json:"usage"`
}

type Attempt struct {
	ID         string     `json:"id"`
	JobID      int64      `json:"job_id"`
	Authorized bool       `json:"authorized"`
	Runner     Identity   `json:"runner"`
	Workload   Identity   `json:"workload"`
	PGID       int        `json:"pgid"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	Result     *Result    `json:"result,omitempty"`
	HookState  string     `json:"hook_state,omitempty"`
}

type Event struct {
	ID        int64     `json:"id"`
	JobID     int64     `json:"job_id"`
	AttemptID string    `json:"attempt_id,omitempty"`
	At        time.Time `json:"at"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
}

type Device struct {
	UUID        string `json:"uuid"`
	Index       *int   `json:"index,omitempty"`
	Model       string `json:"model,omitempty"`
	Memory      int64  `json:"mem_bytes,omitempty"`
	Processes   []int  `json:"processes,omitempty"`
	Available   bool   `json:"available"`
	Reason      string `json:"reason,omitempty"`
	AllocatedTo string `json:"allocated_to,omitempty"`
}

type Inventory struct {
	Devices []Device  `json:"devices"`
	Known   bool      `json:"known"`
	At      time.Time `json:"at"`
	Error   string    `json:"error,omitempty"`
}

type Capacity struct {
	CPUs   int   `json:"ncpus"`
	Memory int64 `json:"mem_bytes"`
	CPUSet []int `json:"cpu_set"`
}

var publicIDPattern = regexp.MustCompile(`^([1-9][0-9]*)(\[([0-9]*)\])?(?:\.localhost)?$`)

// NormalizeID accepts local PBS identifiers without accepting paths or remote servers.
func NormalizeID(s string) (string, error) {
	m := publicIDPattern.FindStringSubmatch(s)
	if m == nil {
		return "", fmt.Errorf("invalid local job ID %q", s)
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid job sequence: %w", err)
	}
	out := strconv.FormatInt(id, 10)
	if m[2] != "" {
		if m[3] == "" {
			out += "[]"
		} else {
			index, err := strconv.ParseUint(m[3], 10, 31)
			if err != nil {
				return "", fmt.Errorf("invalid array index: %w", err)
			}
			out += fmt.Sprintf("[%d]", index)
		}
	}
	return out + ".localhost", nil
}

func Aggregate(parent Job, children []Job) Job {
	parent.StateCounts = make(map[State]int)
	for _, j := range children {
		parent.StateCounts[j.State]++
	}
	counts := parent.StateCounts
	switch {
	case counts[Running]+counts[Starting] > 0:
		parent.State = Running
	case counts[Exiting] > 0:
		parent.State = Exiting
	case counts[Queued] > 0:
		parent.State = Queued
	case counts[Held] > 0:
		parent.State = Held
	case counts[Failed] > 0:
		parent.State = Failed
	case counts[Timeout] > 0:
		parent.State = Timeout
	case counts[Cancelled] > 0:
		parent.State = Cancelled
	default:
		parent.State = Completed
	}
	if parent.State.Terminal() {
		for _, j := range children {
			if j.EndedAt != nil && (parent.EndedAt == nil || j.EndedAt.After(*parent.EndedAt)) {
				parent.EndedAt = j.EndedAt
			}
		}
	}
	return parent
}
