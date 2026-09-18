package integration

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/decfrr/pbotd/internal/executor"
	"github.com/decfrr/pbotd/internal/fsutil"
	"github.com/decfrr/pbotd/internal/model"
)

// The caller supplies an empty delegated subtree with cpu and memory enabled.
func TestDelegatedCgroupEnforcement(t *testing.T) {
	base := os.Getenv("PBOTD_TEST_CGROUP_BASE")
	if base == "" {
		t.Skip("set PBOTD_TEST_CGROUP_BASE to test kernel enforcement")
	}
	r := setup(t, 0)
	r.stop()
	r.configure("[execution]", "[execution]\ncgroup='required'\ncgroup_base='"+base+"'")
	r.start("")
	id := r.submit("setsid sh -c 'echo $$ > escaped; exec sleep 60' &\nwait\n", "-lmem=128MiB")
	r.until("escaped descendant", func() bool { return r.exists("escaped") })
	j := r.waitState(id, model.Running)
	var started executor.Started
	if err := fsutil.ReadJSON(filepath.Join(executor.AttemptDir(r.paths.State, j.ID, j.AttemptID), "started.json"), &started); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"cpu.max": "100000 100000", "memory.max": "134217728"} {
		body, err := os.ReadFile(filepath.Join(started.Cgroup, key))
		if err != nil || strings.TrimSpace(string(body)) != want {
			t.Fatalf("%s=%s %v", key, body, err)
		}
	}
	body, err := os.ReadFile(filepath.Join(r.root, "escaped"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	process, err := executor.Inspect(pid)
	if err != nil {
		t.Fatal(err)
	}
	r.checked("qdel", id)
	r.waitState(id, model.Cancelled)
	if executor.Alive(process.Identity) {
		t.Fatal("setsid descendant survived cgroup cancellation")
	}
	r.until("cgroup removed", func() bool { _, err := os.Stat(started.Cgroup); return os.IsNotExist(err) })
}
