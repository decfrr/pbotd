package pbs

import (
	"slices"
	"testing"

	"github.com/decfrr/pbotd/internal/model"
)

func TestDirectivePrecedenceAndLiteralQuoting(t *testing.T) {
	script := "#!/bin/bash\n#PBS -N 'training run'\n#PBS -l select=1:ncpus=4:mem=8gb:ngpus=2\n#PBS -l walltime=25:00:00\n#PBS -v VALUE='$(touch nope)'\necho start\n#PBS -N ignored\n"
	o, err := Directives(script)
	if err != nil {
		t.Fatal(err)
	}
	cli, _, err := Parse([]string{"-l", "mem=2GiB", "-Ncli"}, "NqloejVvJWh")
	if err != nil {
		t.Fatal(err)
	}
	o.Merge(cli)
	j, err := o.Resolve(model.Resources{CPUs: 1, Memory: 1 << 30}, "default")
	if err != nil {
		t.Fatal(err)
	}
	if j.Name != "cli" || j.Request.CPUs != 4 || j.Request.Memory != 2<<30 || j.Request.GPUs != 2 || j.Request.Walltime != 90000 || j.Queue != "gpu" {
		t.Fatalf("wrong merged job: %+v", j)
	}
	env, err := o.Export(map[string]string{"PATH": "/bin", "SECRET": "no"}, "/bin/bash")
	if err != nil {
		t.Fatal(err)
	}
	if env["VALUE"] != "$(touch nope)" || env["SECRET"] != "" {
		t.Fatalf("wrong export: %v", env)
	}
}

func TestUnsupportedDirectivesFail(t *testing.T) {
	for _, line := range []string{"-l select=2:ncpus=1", "-l select=1+1", "-l select=1:walltime=10", "-l bananas=1", "-a tomorrow", "-N 'unterminated", "-l ncpus=1\\", "-v PBS_JOBID=spoof", "-W depend=afterany:1"} {
		t.Run(line, func(t *testing.T) {
			if _, err := Directives("#PBS " + line); err == nil {
				t.Fatal("accepted invalid directive")
			}
		})
	}
}

func TestArrayRangesLimitsAndBounds(t *testing.T) {
	indices, limit, err := Array("0-6:2,2,9%3")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(indices, []int{0, 2, 4, 6, 9}) || limit != 3 {
		t.Fatalf("%v limit=%d", indices, limit)
	}
	for _, invalid := range []string{"", "0-2%0", "0-2:0", "-1", "5-1", "0-10000", "2147483648", "0-2%1%2", "0,,1"} {
		if _, _, err := Array(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestEnvironmentAndOutput(t *testing.T) {
	o, _, err := Parse([]string{"-V", "-v", "VALUE,OTHER=explicit", "-o", "logs/out", "-j", "oe"}, "NqloejVvJWh")
	if err != nil {
		t.Fatal(err)
	}
	env, err := o.Export(map[string]string{"VALUE": "a b", "OTHER": "old", "SECRET": "exported", "CUDA_VISIBLE_DEVICES": "bad", "PBS_JOBID": "bad"}, "/bin/bash")
	if err != nil {
		t.Fatal(err)
	}
	if env["VALUE"] != "a b" || env["OTHER"] != "explicit" || env["SECRET"] != "exported" || env["CUDA_VISIBLE_DEVICES"] != "" || env["PBS_JOBID"] != "" {
		t.Fatalf("%v", env)
	}
	index := 3
	stdout, stderr := o.OutputPaths(model.Job{Name: "job", CWD: "/work", ArrayIndex: &index}, 42)
	if stdout != "/work/logs/out.3" || stderr != stdout {
		t.Fatalf("%q %q", stdout, stderr)
	}
	if _, err := o.Export(nil, "/bin/bash"); err == nil {
		t.Fatal("missing bare export accepted")
	}
}

func TestQueueDefaultsAndInvalidResources(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		queue string
		gpu   int
		fail  bool
	}{
		{nil, "cpu", 0, false}, {[]string{"-qgpu"}, "gpu", 1, false}, {[]string{"-l", "ngpus=2"}, "gpu", 2, false},
		{[]string{"-qgpu", "-l", "ngpus=0"}, "", 0, true}, {[]string{"-qcpu", "-l", "ngpus=1"}, "", 0, true},
		{[]string{"-l", "ncpus=0"}, "", 0, true}, {[]string{"-l", "mem=0"}, "", 0, true}, {[]string{"-l", "walltime=00:60:00"}, "", 0, true},
	} {
		o, _, err := Parse(tc.args, "NqloejVvJWh")
		if err != nil {
			t.Fatal(err)
		}
		j, err := o.Resolve(model.Resources{CPUs: 1, Memory: 1 << 30}, "job")
		if (err != nil) != tc.fail {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if !tc.fail && (j.Queue != tc.queue || j.Request.GPUs != tc.gpu) {
			t.Fatalf("%v: %+v", tc.args, j)
		}
	}
}
