package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/decfrr/pbotd/internal/ipc"
	"github.com/decfrr/pbotd/internal/model"
)

func TestForeignGPUProcessBlocksOnlyGPUWork(t *testing.T) {
	r := setup(t, 1)
	r.stop()
	const uuid = "GPU-00000000-0000-0000-0000-000000000001"
	occupancy := filepath.Join(r.root, "occupancy")
	if err := os.WriteFile(occupancy, []byte(fmt.Sprintf("%s, %d\n", uuid, os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(r.root, "probe.sh")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\ncat '"+occupancy+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	r.configure("foreign_process_policy='ignore'", "foreign_process_policy='avoid'\ncommand='"+probe+"'")
	r.start("")
	r.until("foreign process detected", func() bool {
		response, err := r.call(ipc.Request{Operation: "status"})
		return err == nil && len(response.Node.Inventory.Devices) == 1 && response.Node.Inventory.Devices[0].Reason == "ForeignProcess"
	})
	id := r.submit(gatedScript, "-qgpu")
	r.waitState(r.submit("exit 0\n"), model.Completed)
	if r.job(id).State != model.Queued || r.exists("started."+id) {
		t.Fatal("foreign GPU ignored")
	}
	if err := os.WriteFile(occupancy, nil, 0600); err != nil {
		t.Fatal(err)
	}
	r.until("GPU released by foreign process", func() bool { return r.exists("started." + id) })
	r.release(id)
	r.waitState(id, model.Completed)
}
