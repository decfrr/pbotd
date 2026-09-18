package gpu

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/decfrr/pbotd/internal/config"
)

const testUUID = "GPU-8932f937-d72c-4106-c12f-20bd9faed9f6"

func TestProbeTimeoutBoundsInheritedOutputPipes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slow-probe")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 1 &\nwait\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	c.GPU.Command = path
	c.GPU.ProbeTimeout = "20ms"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	inv := New(c).Refresh(context.Background())
	if inv.Error == "" || time.Since(start) > 750*time.Millisecond {
		t.Fatalf("unbounded failed probe: %+v (%v)", inv, time.Since(start))
	}
}

func TestCSVAndUnknownOccupancy(t *testing.T) {
	devices, err := ParseInventory("0, " + testUUID + ", \"GPU, model\", 24576\n")
	if err != nil || len(devices) != 1 || devices[0].Model != "GPU, model" || devices[0].Memory != 24576<<20 {
		t.Fatalf("%+v %v", devices, err)
	}
	processes, err := ParseProcesses(testUUID + ", 123\n")
	if err != nil || len(processes[testUUID]) != 1 {
		t.Fatalf("%v %v", processes, err)
	}
	for _, body := range []string{"[Not Supported]", testUUID + ", [N/A]", testUUID + ", 0"} {
		if _, err := ParseProcesses(body); err == nil {
			t.Fatalf("accepted unknown process data %q", body)
		}
	}
}

func TestStaticFallbackStillRequiresOccupancy(t *testing.T) {
	c := config.Default()
	c.GPU.Static = []string{testUUID}
	c.GPU.Command = filepath.Join(t.TempDir(), "missing")
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	p := New(c)
	inv := p.Refresh(context.Background())
	if !inv.Known || len(inv.Devices) != 1 || inv.Devices[0].Available || inv.Devices[0].Reason != "GPUOccupancyUnknown" {
		t.Fatalf("%+v", inv)
	}
	c.GPU.ForeignPolicy = "ignore"
	inv = New(c).Refresh(context.Background())
	if len(inv.Devices) != 1 || !inv.Devices[0].Available {
		t.Fatalf("explicit static fallback failed: %+v", inv)
	}
}

func TestDiscoveryFailurePreservesKnownDevices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nvidia-smi")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '0, "+testUUID+", Test, 1024'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	c.GPU.Command = path
	c.GPU.ForeignPolicy = "ignore"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	p := New(c)
	first := p.Refresh(context.Background())
	if !first.Known || len(first.Devices) != 1 {
		t.Fatalf("%+v", first)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	next := p.Refresh(context.Background())
	if !next.Known || len(next.Devices) != 1 || next.Devices[0].Available || next.Devices[0].UUID != testUUID {
		t.Fatalf("%+v", next)
	}
}
