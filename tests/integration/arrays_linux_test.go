package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/decfrr/pbotd/internal/ipc"
	"github.com/decfrr/pbotd/internal/model"
)

func TestArrayLimitDependenciesAndRestart(t *testing.T) {
	r := setup(t, 2)
	parent := r.submit("echo $PBS_ARRAY_INDEX\n"+gatedScript, "-J1-3%1", "-qgpu", "-oarray.out")
	child := func(i string) string { return strings.Replace(parent, "[]", "["+i+"]", 1) }
	dependent := r.submit("echo success > dependent\n", "-Wdepend=afterok:"+parent)
	r.until("first array task", func() bool { return r.exists("started." + child("1")) })
	r.stop()
	r.start("")
	for _, index := range []string{"1", "2", "3"} {
		id := child(index)
		r.until("array task "+index, func() bool { return r.exists("started." + id) })
		response, err := r.call(ipc.Request{Operation: "jobs", IDs: []string{parent}})
		if err != nil {
			t.Fatal(err)
		}
		active := 0
		for _, j := range response.Jobs {
			if !j.ArrayParent && j.State.Active() {
				active++
			}
		}
		if active != 1 || r.exists("dependent") {
			t.Fatalf("array concurrency/dependency violation: %+v", response.Jobs)
		}
		r.release(id)
		r.waitState(id, model.Completed)
		body, err := os.ReadFile(filepath.Join(r.root, "array.out."+index))
		if err != nil || strings.TrimSpace(string(body)) != index {
			t.Fatalf("array output %q %v", body, err)
		}
	}
	r.waitState(parent, model.Completed)
	r.waitState(dependent, model.Completed)
	if !r.exists("dependent") {
		t.Fatal("dependency never executed")
	}
}

func TestDependencyFailureAndArrayControls(t *testing.T) {
	r := setup(t, 2)
	parent := r.submit("echo unexpected\n", "-h", "-J0-2%2", "-qgpu", "-lngpus=2")
	r.checked("qalter", "-Nrenamed", "-lmem=2MiB", parent)
	response, err := r.call(ipc.Request{Operation: "jobs", IDs: []string{parent}})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range response.Jobs {
		if j.Request.GPUs != 2 || j.Name != "renamed" {
			t.Fatalf("partial array alteration: %+v", j)
		}
	}
	child := strings.Replace(parent, "[]", "[1]", 1)
	dependent := r.submit("touch must-not-run\n", "-Wdepend=afterok:"+child)
	r.checked("qdel", child)
	failed := r.waitState(dependent, model.Failed)
	if failed.Reason != "DependencyFailed" || r.exists("must-not-run") {
		t.Fatalf("%+v", failed)
	}
	r.checked("qdel", parent)
	r.waitState(parent, model.Cancelled)
	if _, err := r.cli("qrerun", parent); err == nil {
		t.Fatal("parent rerun accepted")
	}
	if _, err := r.cli("qalter", "-Nlate", parent); err == nil {
		t.Fatal("terminal array alteration accepted")
	}
}

func TestMixedControlsAndCancellation(t *testing.T) {
	r := setup(t, 1)
	held := r.submit("exit 0\n", "-h")
	if _, err := r.cli("qrls", "999999", held); err == nil {
		t.Fatal("unknown ID accepted")
	}
	r.waitState(held, model.Completed)
	id := r.submit("trap '' TERM\n"+gatedScript, "-qgpu")
	r.until("running", func() bool { return r.exists("started." + id) })
	r.checked("qdel", id)
	r.stop()
	r.start("")
	r.waitState(id, model.Cancelled)
	r.waitState(r.submit("exit 0\n", "-qgpu"), model.Completed)
}
