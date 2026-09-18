package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/decfrr/pbotd/internal/model"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "pbotd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func addJob(t *testing.T, s *Store) model.Job {
	t.Helper()
	j := model.Job{Name: "test", State: model.Queued, Queue: "gpu", Request: model.Resources{CPUs: 1, Memory: 1024, GPUs: 1}, SubmittedAt: time.Now().UTC(), QueuedAt: time.Now().UTC()}
	if err := s.Update(func(tx *Tx) error { return tx.Create(&j) }); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestReservationRollbackAndRestart(t *testing.T) {
	s := newStore(t)
	a, b := addJob(t, s), addJob(t, s)
	capacity := model.Capacity{CPUs: 4, Memory: 8192}
	if _, _, err := s.Reserve(a.ID, []string{"GPU-A"}, nil, capacity); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Reserve(b.ID, []string{"GPU-A"}, nil, capacity); err == nil {
		t.Fatal("duplicate UUID allocated")
	}
	b, err := s.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.State != model.Queued || b.AttemptCount != 0 || b.AttemptID != "" {
		t.Fatalf("partial failed reservation: %+v", b)
	}
	attempts, err := s.Attempts(b.ID)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("orphan attempts: %v %v", attempts, err)
	}
	allocations, err := s.Allocations()
	if err != nil || len(allocations) != 1 || allocations["GPU-A"] != a.ID {
		t.Fatalf("%v %v", allocations, err)
	}
	if err := s.Integrity(); err != nil {
		t.Fatal(err)
	}
}

func TestActiveStatesKeepCapacity(t *testing.T) {
	s := newStore(t)
	a, b := addJob(t, s), addJob(t, s)
	capacity := model.Capacity{CPUs: 1, Memory: 1024}
	a, attempt, err := s.Reserve(a.ID, []string{"GPU-A"}, nil, capacity)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []model.State{model.Starting, model.Running, model.Exiting} {
		a.State = state
		if err := s.Update(func(tx *Tx) error { return tx.Save(a) }); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Reserve(b.ID, []string{"GPU-B"}, nil, capacity); err == nil {
			t.Fatalf("capacity released in %s", state)
		}
	}
	if err := s.Update(func(tx *Tx) error {
		a.State = model.Completed
		if err := tx.Save(a); err != nil {
			return err
		}
		return tx.Release(a.ID, attempt.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Reserve(b.ID, []string{"GPU-A"}, nil, capacity); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentGPUReservations(t *testing.T) {
	s := newStore(t)
	jobs := []model.Job{addJob(t, s), addJob(t, s)}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, j := range jobs {
		wg.Go(func() {
			_, _, err := s.Reserve(j.ID, []string{"GPU-shared"}, nil, model.Capacity{CPUs: 4, Memory: 8192})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d reservations succeeded", succeeded)
	}
}

func TestSubmissionRollbackAndMigrationPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pbotd.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	j := addJob(t, s)
	failure := errors.New("injected failure")
	if err := s.Update(func(tx *Tx) error {
		bad := j
		bad.ID = 0
		if err := tx.Create(&bad); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	jobs, err := s.Jobs()
	if err != nil || len(jobs) != 1 || jobs[0].PublicID != j.PublicID {
		t.Fatalf("%v %v", jobs, err)
	}
	events, err := s.Events(j.ID)
	if err != nil || len(events) != 1 || events[0].Type != "Submitted" {
		t.Fatalf("%v %v", events, err)
	}
}
