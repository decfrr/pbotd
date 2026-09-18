package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/decfrr/pbotd/internal/model"
)

func TestSystemdUserServicePreservesRunners(t *testing.T) {
	if os.Getenv("PBOTD_TEST_SYSTEMD") != "1" {
		t.Skip("set PBOTD_TEST_SYSTEMD=1 with an available systemd user manager")
	}
	r := setup(t, 1)
	r.stop()
	unit := fmt.Sprintf("pbotd-test-%d-%d.service", os.Getpid(), time.Now().UnixNano())
	body, err := os.ReadFile(filepath.Join("..", "..", "contrib", "systemd", "pbotd.service"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(body), "ExecStart=%h/.local/bin/pbotd daemon --foreground", "ExecStart="+strconv.Quote(binary)+" daemon --foreground", 1)
	text = strings.Replace(text, "[Service]", "[Service]\nEnvironment="+strconv.Quote("PBOTD_CONFIG="+r.paths.Config)+"\nEnvironment="+strconv.Quote("PBOTD_STATE_DIR="+r.paths.State)+"\nEnvironment="+strconv.Quote("PBOTD_SOCKET="+r.paths.Socket), 1)
	path := filepath.Join(r.root, unit)
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	ctl := func(args ...string) {
		t.Helper()
		cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("systemctl %v: %s %v", args, output, err)
		}
	}
	ctl("link", path)
	t.Cleanup(func() {
		exec.Command("systemctl", "--user", "stop", unit).Run()
		exec.Command("systemctl", "--user", "disable", unit).Run()
		exec.Command("systemctl", "--user", "daemon-reload").Run()
	})
	ctl("start", unit)
	r.until("service ready", func() bool { _, err := r.cli("status"); return err == nil })
	id := r.submit(gatedScript, "-qgpu")
	r.until("workload ready", func() bool { return r.exists("started." + id) })
	before := r.waitState(id, model.Running)
	ctl("restart", unit)
	r.until("service restarted", func() bool { _, err := r.cli("status"); return err == nil })
	after := r.waitState(id, model.Running)
	if before.AttemptID != after.AttemptID {
		t.Fatal("systemd restart replaced running work")
	}
	ctl("stop", unit)
	r.release(id)
	r.until("work completes with service stopped", func() bool { return r.exists("finished." + id) })
	ctl("start", unit)
	r.until("service ready again", func() bool { _, err := r.cli("status"); return err == nil })
	j := r.waitState(id, model.Completed)
	if j.ExitCode == nil || *j.ExitCode != 0 {
		t.Fatalf("lost result %+v", j)
	}
}
