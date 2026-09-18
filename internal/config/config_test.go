package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaultsAndExplicitSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tick != time.Second || c.DefaultRequest.CPUs != 1 || c.DefaultRequest.Memory != 1<<30 || c.TermGrace != 10*time.Second {
		t.Fatalf("derived defaults missing: %+v", c)
	}
	if err := os.WriteFile(path, []byte("[resources]\ncpus=2\nmemory='8GiB'\n[defaults]\nncpus=2\nmem='2GiB'\nwalltime='01:00:00'\n[daemon]\nautostart=false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Resources.CPUs != int64(2) || c.Daemon.Autostart || c.DefaultRequest.CPUs != 2 || c.DefaultRequest.Walltime != 3600 {
		t.Fatalf("%+v", c)
	}
}

func TestInvalidConfig(t *testing.T) {
	for _, body := range []string{"[scheduler]\ntick='0s'", "[scheduler]\nunknown=true", "[resources]\ncpus='eight'", "[defaults]\nmem='0'", "[gpu]\nprovider='static'", "[gpu]\nforeign_process_policy='guess'", "[execution]\ncgroup='required'"} {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestPathOverridesAndRuntimeFallback(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PBOTD_STATE_DIR", filepath.Join(root, "state"))
	t.Setenv("PBOTD_CONFIG", filepath.Join(root, "custom.toml"))
	t.Setenv("PBOTD_SOCKET", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.Socket != filepath.Join(root, "state", "run", "pbotd.sock") || p.Config != filepath.Join(root, "custom.toml") {
		t.Fatalf("%+v", p)
	}
	t.Setenv("PBOTD_SOCKET", filepath.Join(root, "custom.sock"))
	p, err = ResolvePaths()
	if err != nil || p.Socket != filepath.Join(root, "custom.sock") {
		t.Fatalf("%+v %v", p, err)
	}
}
