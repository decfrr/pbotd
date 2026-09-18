package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/decfrr/pbotd/internal/model"
)

func TestOutputAppendMergeAndLaunchFailure(t *testing.T) {
	r := setup(t, 1)
	path := filepath.Join(r.root, "merged")
	if err := os.WriteFile(path, []byte("existing\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id := r.submit("echo stdout\necho stderr >&2\n", "-o", path, "-joe")
	r.waitState(id, model.Completed)
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "existing\nstdout\nstderr\n" {
		t.Fatalf("merged output: %q %v", body, err)
	}
	separate := r.submit("echo stdout\necho stderr >&2\n", "-oseparate/out", "-eseparate/err")
	done := r.waitState(separate, model.Completed)
	for path, want := range map[string]string{done.Stdout: "stdout\n", done.Stderr: "stderr\n"} {
		body, err := os.ReadFile(path)
		if err != nil || string(body) != want {
			t.Fatalf("separate output: %q %v", body, err)
		}
	}
	failed := r.submit("touch must-not-execute\n", "-qgpu", "-o", r.root)
	job := r.waitState(failed, model.Failed)
	if r.exists("must-not-execute") || !strings.Contains(job.Reason, "directory") {
		t.Fatalf("invalid launch result: %+v", job)
	}
	r.waitState(r.submit("exit 0\n", "-qgpu"), model.Completed)
}
