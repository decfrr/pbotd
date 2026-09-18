package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/decfrr/pbotd/internal/fsutil"
	"github.com/decfrr/pbotd/internal/model"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE jobs (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 public_id TEXT UNIQUE,
 parent_id INTEGER REFERENCES jobs(id),
 state TEXT NOT NULL,
 queued_at INTEGER NOT NULL,
 data TEXT NOT NULL CHECK(json_valid(data))
);
CREATE INDEX jobs_queue ON jobs(state, queued_at, id);
CREATE INDEX jobs_parent ON jobs(parent_id);
CREATE TABLE attempts (
 id TEXT PRIMARY KEY,
 job_id INTEGER NOT NULL REFERENCES jobs(id),
 data TEXT NOT NULL CHECK(json_valid(data)),
 UNIQUE(id, job_id)
);
CREATE TABLE allocations (
 gpu_uuid TEXT PRIMARY KEY,
 job_id INTEGER NOT NULL REFERENCES jobs(id),
 attempt_id TEXT NOT NULL,
 created_at TEXT NOT NULL,
 FOREIGN KEY(attempt_id, job_id) REFERENCES attempts(id, job_id)
);
CREATE TABLE events (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 job_id INTEGER NOT NULL REFERENCES jobs(id),
 attempt_id TEXT NOT NULL,
 timestamp TEXT NOT NULL,
 type TEXT NOT NULL,
 message TEXT NOT NULL
);
CREATE INDEX events_job ON events(job_id, id);
PRAGMA user_version = 1;
`

type Store struct{ db *sql.DB }
type Tx struct{ tx *sql.Tx }

func Open(path string) (*Store, error) {
	if err := fsutil.PrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: path}
	query := url.Values{}
	for _, pragma := range []string{"foreign_keys(1)", "busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)"} {
		query.Add("_pragma", pragma)
	}
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version > 1 {
		db.Close()
		return nil, fmt.Errorf("database schema %d is newer than supported schema 1", version)
	}
	if version == 0 {
		if err := s.Update(func(tx *Tx) error { _, err := tx.tx.Exec(schema); return err }); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Update(fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(&Tx{tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

func decodeJob(row interface{ Scan(...any) error }) (model.Job, error) {
	var body string
	if err := row.Scan(&body); err != nil {
		return model.Job{}, err
	}
	var j model.Job
	err := json.Unmarshal([]byte(body), &j)
	return j, err
}

func (s *Store) Get(id int64) (model.Job, error) {
	return decodeJob(s.db.QueryRow("SELECT data FROM jobs WHERE id=?", id))
}
func (t *Tx) Get(id int64) (model.Job, error) {
	return decodeJob(t.tx.QueryRow("SELECT data FROM jobs WHERE id=?", id))
}

func listJobs(rows *sql.Rows, err error) ([]model.Job, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []model.Job
	for rows.Next() {
		j, err := decodeJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *Store) Jobs() ([]model.Job, error) {
	return listJobs(s.db.Query("SELECT data FROM jobs ORDER BY queued_at, id"))
}
func (t *Tx) Jobs() ([]model.Job, error) {
	return listJobs(t.tx.Query("SELECT data FROM jobs ORDER BY queued_at, id"))
}

func (s *Store) Find(publicID string) (model.Job, error) {
	j, err := decodeJob(s.db.QueryRow("SELECT data FROM jobs WHERE public_id=?", publicID))
	if errors.Is(err, sql.ErrNoRows) && !strings.Contains(publicID, "[") {
		return decodeJob(s.db.QueryRow("SELECT data FROM jobs WHERE public_id=?", strings.Replace(publicID, ".localhost", "[].localhost", 1)))
	}
	return j, err
}

func (t *Tx) Create(j *model.Job) error {
	var parent any
	if j.ParentID != 0 {
		parent = j.ParentID
	}
	result, err := t.tx.Exec("INSERT INTO jobs(parent_id,state,queued_at,data) VALUES(?,?,?,?)", parent, j.State, j.QueuedAt.UnixNano(), "{}")
	if err != nil {
		return err
	}
	j.ID, err = result.LastInsertId()
	if err != nil {
		return err
	}
	switch {
	case j.ArrayParent:
		j.PublicID = fmt.Sprintf("%d[].localhost", j.ID)
	case j.ArrayIndex != nil:
		j.PublicID = fmt.Sprintf("%d[%d].localhost", j.ParentID, *j.ArrayIndex)
	default:
		j.PublicID = fmt.Sprintf("%d.localhost", j.ID)
	}
	if err := t.Save(*j); err != nil {
		return err
	}
	return t.Event(*j, "Submitted", "")
}

func (t *Tx) Save(j model.Job) error {
	body, err := json.Marshal(j)
	if err != nil {
		return err
	}
	result, err := t.tx.Exec("UPDATE jobs SET public_id=?,state=?,queued_at=?,data=? WHERE id=?", j.PublicID, j.State, j.QueuedAt.UnixNano(), string(body), j.ID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("job %d does not exist", j.ID)
	}
	return nil
}

func (t *Tx) Event(j model.Job, kind, message string) error {
	_, err := t.tx.Exec("INSERT INTO events(job_id,attempt_id,timestamp,type,message) VALUES(?,?,?,?,?)", j.ID, j.AttemptID, time.Now().UTC().Format(time.RFC3339Nano), kind, message)
	return err
}

func (s *Store) Events(jobID int64) ([]model.Event, error) {
	rows, err := s.db.Query("SELECT id,job_id,attempt_id,timestamp,type,message FROM events WHERE job_id=? ORDER BY id", jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []model.Event
	for rows.Next() {
		var e model.Event
		var timestamp string
		if err := rows.Scan(&e.ID, &e.JobID, &e.AttemptID, &timestamp, &e.Type, &e.Message); err != nil {
			return nil, err
		}
		e.At, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) Attempt(id string) (model.Attempt, error) {
	var a model.Attempt
	var body string
	if err := s.db.QueryRow("SELECT data FROM attempts WHERE id=?", id).Scan(&body); err != nil {
		return a, err
	}
	err := json.Unmarshal([]byte(body), &a)
	return a, err
}

func (s *Store) Attempts(jobID int64) ([]model.Attempt, error) {
	rows, err := s.db.Query("SELECT data FROM attempts WHERE job_id=? ORDER BY rowid", jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attempts []model.Attempt
	for rows.Next() {
		var a model.Attempt
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(body), &a); err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}

func (t *Tx) SaveAttempt(a model.Attempt) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec("INSERT INTO attempts(id,job_id,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", a.ID, a.JobID, string(body))
	return err
}

// Reserve atomically checks admission and persists STARTING plus GPU ownership.
// The DB uniqueness constraint is the final guard against duplicate device allocation.
func (s *Store) Reserve(id int64, gpus []string, cpus []int, capacity model.Capacity) (model.Job, model.Attempt, error) {
	var job model.Job
	var attempt model.Attempt
	err := s.Update(func(tx *Tx) error {
		var err error
		job, err = tx.Get(id)
		if err != nil {
			return err
		}
		if job.State != model.Queued || job.ArrayParent {
			return fmt.Errorf("job %s is not eligible for reservation", job.PublicID)
		}
		if len(gpus) != job.Request.GPUs {
			return fmt.Errorf("GPU allocation does not match request")
		}
		jobs, err := tx.Jobs()
		if err != nil {
			return err
		}
		availableCPU, availableMem := capacity.CPUs, capacity.Memory
		arrayActive := 0
		for _, j := range jobs {
			if !j.ArrayParent && j.State.Active() {
				availableCPU -= j.Request.CPUs
				availableMem -= j.Request.Memory
				if job.ParentID != 0 && j.ParentID == job.ParentID {
					arrayActive++
				}
			}
		}
		if job.Request.CPUs > availableCPU || job.Request.Memory > availableMem {
			return fmt.Errorf("insufficient CPU/memory capacity")
		}
		if job.ArrayLimit > 0 && arrayActive >= job.ArrayLimit {
			return fmt.Errorf("array concurrency limit reached")
		}
		attempt = model.Attempt{ID: uuid.NewString(), JobID: job.ID}
		job.State, job.AttemptID, job.Reason = model.Starting, attempt.ID, ""
		job.AttemptCount++
		job.GPUs, job.CPUSet = append([]string(nil), gpus...), append([]int(nil), cpus...)
		job.ExitCode, job.StartedAt, job.EndedAt, job.ControlAt = nil, nil, nil, nil
		job.Control, job.Signal = "", 0
		if err := tx.SaveAttempt(attempt); err != nil {
			return err
		}
		for _, gpu := range gpus {
			if _, err := tx.tx.Exec("INSERT INTO allocations(gpu_uuid,job_id,attempt_id,created_at) VALUES(?,?,?,?)", gpu, job.ID, attempt.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return fmt.Errorf("reserve GPU %s: %w", gpu, err)
			}
		}
		if err := tx.Save(job); err != nil {
			return err
		}
		return tx.Event(job, "Starting", "resources reserved")
	})
	return job, attempt, err
}

func (t *Tx) Release(jobID int64, attemptID string) error {
	_, err := t.tx.Exec("DELETE FROM allocations WHERE job_id=? AND attempt_id=?", jobID, attemptID)
	return err
}

func (s *Store) Allocations() (map[string]int64, error) {
	rows, err := s.db.Query("SELECT gpu_uuid,job_id FROM allocations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]int64)
	for rows.Next() {
		var uuid string
		var id int64
		if err := rows.Scan(&uuid, &id); err != nil {
			return nil, err
		}
		result[uuid] = id
	}
	return result, rows.Err()
}

func (s *Store) Integrity() error {
	var result string
	if err := s.db.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("database integrity: %s", result)
	}
	return nil
}

// Check opens an existing database read-only; diagnostics never create or migrate it.
func Check(ctx context.Context, path string) error {
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(2000)"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return err
	}
	defer db.Close()
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("database schema %d; expected 1", version)
	}
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("database integrity: %s", result)
	}
	for _, table := range []string{"jobs", "attempts", "allocations", "events"} {
		var name string
		if err := db.QueryRowContext(ctx, "SELECT name FROM sqlite_schema WHERE type='table' AND name=?", table).Scan(&name); err != nil {
			return fmt.Errorf("missing %s table: %w", table, err)
		}
	}
	return nil
}
