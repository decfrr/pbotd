package executor

import (
	"path/filepath"
	"strconv"
	"time"

	"github.com/decfrr/pbotd/internal/model"
)

type Spec struct {
	Job        model.Job     `json:"job"`
	AttemptID  string        `json:"attempt_id"`
	Shell      string        `json:"shell"`
	TermGrace  time.Duration `json:"term_grace"`
	Socket     string        `json:"socket"`
	CgroupMode string        `json:"cgroup_mode"`
	CgroupBase string        `json:"cgroup_base"`
	Affinity   bool          `json:"affinity"`
	Command    []string      `json:"command,omitempty"`
	HookResult *model.Result `json:"hook_result,omitempty"`
	Timeout    time.Duration `json:"timeout,omitempty"`
}

type Authorization struct {
	AttemptID string `json:"attempt_id"`
}

type Control struct {
	AttemptID string    `json:"attempt_id"`
	Kind      string    `json:"kind"`
	At        time.Time `json:"at"`
}

type Started struct {
	AttemptID string         `json:"attempt_id"`
	Runner    model.Identity `json:"runner"`
	Workload  model.Identity `json:"workload"`
	PGID      int            `json:"pgid"`
	At        time.Time      `json:"at"`
	Cgroup    string         `json:"cgroup,omitempty"`
}

func JobDir(state string, jobID int64) string {
	return filepath.Join(state, "jobs", strconv.FormatInt(jobID, 10))
}
func AttemptDir(state string, jobID int64, attemptID string) string {
	return filepath.Join(JobDir(state, jobID), "attempts", attemptID)
}
