package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/decfrr/pbotd/internal/ipc"
	"github.com/decfrr/pbotd/internal/model"
)

func TestOneGPUSequentialJobsAndPromptRelease(t *testing.T) {
	r := setup(t, 1)
	ids := []string{r.submit(gatedScript, "-qgpu"), r.submit(gatedScript, "-qgpu"), r.submit(gatedScript, "-qgpu")}
	for i, id := range ids {
		r.until("workload start", func() bool { return r.exists("started." + id) })
		for _, later := range ids[i+1:] {
			if r.exists("started." + later) {
				t.Fatalf("GPU overlap: %s and %s", id, later)
			}
		}
		job := r.job(id)
		if len(job.GPUs) != 1 {
			t.Fatalf("no GPU ownership: %+v", job)
		}
		body, _ := os.ReadFile(filepath.Join(r.root, "started."+id))
		if string(body) != job.GPUs[0] {
			t.Fatalf("visible GPU mismatch: %s vs %v", body, job.GPUs)
		}
		at := time.Now()
		r.release(id)
		r.waitState(id, model.Completed)
		if i+1 < len(ids) {
			r.until("next launch", func() bool { return r.exists("started." + ids[i+1]) })
			if elapsed := time.Since(at); elapsed > 1500*time.Millisecond {
				t.Fatalf("slow next launch: %v", elapsed)
			}
		}
	}
	response, err := r.call(ipc.Request{Operation: "status"})
	if err != nil || response.Node.Reserved.GPUs != 0 {
		t.Fatalf("reservation leaked: %+v %v", response, err)
	}
}

func TestTwoGPUsConcurrentJobs(t *testing.T) {
	r := setup(t, 2)
	a, b, c := r.submit(gatedScript, "-qgpu"), r.submit(gatedScript, "-qgpu"), r.submit(gatedScript, "-qgpu")
	r.until("two concurrent workloads", func() bool { return r.exists("started."+a) && r.exists("started."+b) })
	if r.exists("started." + c) {
		t.Fatal("third GPU job overlapped")
	}
	first, second := r.job(a), r.job(b)
	if first.GPUs[0] == second.GPUs[0] {
		t.Fatal("duplicate GPU UUID")
	}
	r.release(a)
	r.waitState(a, model.Completed)
	r.until("third workload", func() bool { return r.exists("started." + c) })
	third := r.job(c)
	if third.GPUs[0] != first.GPUs[0] {
		t.Fatal("released UUID was not reused")
	}
	r.release(b)
	r.release(c)
	r.waitState(b, model.Completed)
	r.waitState(c, model.Completed)
}

func TestDaemonRestartPreservesWorkAndExactResults(t *testing.T) {
	r := setup(t, 1)
	a, b := r.submit(gatedScript, "-qgpu"), r.submit("exit 7\n", "-qgpu")
	r.until("first running", func() bool { return r.exists("started." + a) })
	before := r.job(a)
	r.stop()
	r.start("")
	after := r.waitState(a, model.Running)
	if before.AttemptID != after.AttemptID {
		t.Fatal("running attempt replaced after restart")
	}
	if r.job(b).State != model.Queued {
		t.Fatal("GPU reservation lost after restart")
	}
	r.stop()
	r.release(a)
	r.until("completed while daemon absent", func() bool { return r.exists("finished." + a) })
	r.start("")
	completed := r.waitState(a, model.Completed)
	failed := r.waitState(b, model.Failed)
	if completed.ExitCode == nil || *completed.ExitCode != 0 || failed.ExitCode == nil || *failed.ExitCode != 7 {
		t.Fatalf("lost exit results: %+v %+v", completed, failed)
	}
}

func TestHoldAlterReleaseAndSnapshot(t *testing.T) {
	r := setup(t, 0)
	file := filepath.Join(r.root, "snapshot-test.pbs")
	if err := os.WriteFile(file, []byte("printf original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id := r.checked("qsub", "-h", file)
	if err := os.WriteFile(file, []byte("printf changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r.checked("qalter", "-N", "renamed", "-l", "ncpus=2,mem=2MiB", id)
	r.stop()
	r.start("")
	held := r.waitState(id, model.Held)
	if held.Name != "renamed" || held.Request.CPUs != 2 {
		t.Fatalf("alteration lost: %+v", held)
	}
	r.checked("qrls", id)
	done := r.waitState(id, model.Completed)
	body, _ := os.ReadFile(done.Stdout)
	if string(body) != "original" {
		t.Fatalf("script was not snapshotted: %q", body)
	}
	if _, err := r.cli("qalter", "-N", "late", id); err == nil {
		t.Fatal("terminal alteration accepted")
	}
	text := r.checked("qstat", "-f", "-x", "--json", id)
	response := decodeResponse(t, text)
	if len(response.Attempts) != 1 || len(response.Events) < 2 || response.Jobs[0].Environment != nil {
		t.Fatalf("missing history or leaked environment: %+v", response)
	}
}

func TestRerunPreservesLogsAndIgnoresOldAttempt(t *testing.T) {
	r := setup(t, 1)
	id := r.submit("printf x >> count\necho attempt=$PBOTD_ATTEMPT_ID\n"+gatedScript, "-qgpu")
	r.until("first execution", func() bool { return r.exists("started." + id) })
	first := r.waitState(id, model.Running)
	r.checked("qrerun", id)
	r.until("second execution", func() bool { body, _ := os.ReadFile(filepath.Join(r.root, "count")); return string(body) == "xx" })
	second := r.waitState(id, model.Running)
	if first.AttemptID == second.AttemptID || second.AttemptCount != 2 {
		t.Fatalf("no new attempt: %+v", second)
	}
	if _, err := r.call(ipc.Request{Operation: "wake", AttemptID: first.AttemptID}); err != nil {
		t.Fatal(err)
	}
	if r.job(id).State != model.Running {
		t.Fatal("stale attempt completed rerun")
	}
	r.release(id)
	done := r.waitState(id, model.Completed)
	output, _ := os.ReadFile(done.Stdout)
	if strings.Count(string(output), "attempt=") != 2 {
		t.Fatalf("rerun output lost: %s", output)
	}
	if _, err := r.cli("qrerun", id); err == nil {
		t.Fatal("terminal rerun accepted")
	}
}
