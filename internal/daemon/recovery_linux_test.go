package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/decfrr/pbotd/internal/model"
	"github.com/decfrr/pbotd/internal/store"
)

func TestTerminationPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, control string
		at            time.Duration
		want          model.State
	}{
		{"rerun before deadline", "rerun", 500 * time.Millisecond, model.Queued},
		{"timeout before rerun", "rerun", 1500 * time.Millisecond, model.Timeout},
		{"cancel before deadline", "cancel", 500 * time.Millisecond, model.Cancelled},
		{"timeout before cancel", "cancel", 1500 * time.Millisecond, model.Timeout},
		{"result before rerun", "rerun", 3 * time.Second, model.Timeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "pbotd.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			start := time.Now().UTC()
			controlAt := start.Add(tc.at)
			job := model.Job{Name: "test", State: model.Queued, QueuedAt: start, SubmittedAt: start, Request: model.Resources{CPUs: 1, Memory: 1, Walltime: 1}}
			if err := db.Update(func(tx *store.Tx) error { return tx.Create(&job) }); err != nil {
				t.Fatal(err)
			}
			job, attempt, err := db.Reserve(job.ID, nil, nil, model.Capacity{CPUs: 1, Memory: 1})
			if err != nil {
				t.Fatal(err)
			}
			job.Control = tc.control
			job.ControlAt = &controlAt
			d := Daemon{store: db}
			if err := d.finish(job, attempt, model.Result{AttemptID: attempt.ID, State: model.Timeout, Reason: "Walltime", StartedAt: &start, EndedAt: start.Add(2 * time.Second)}); err != nil {
				t.Fatal(err)
			}
			got, err := db.Get(job.ID)
			if err != nil || got.State != tc.want {
				t.Fatalf("state=%s want=%s err=%v", got.State, tc.want, err)
			}
		})
	}
}
