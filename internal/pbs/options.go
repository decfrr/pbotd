package pbs

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/decfrr/pbotd/internal/model"
	"github.com/decfrr/pbotd/internal/resource"
)

type Options struct {
	Values      map[string]string `json:"values"`
	Environment map[string]string `json:"environment"`
	ExportAll   bool              `json:"export_all"`
	Hold        bool              `json:"hold"`
}

func NewOptions() Options {
	return Options{Values: make(map[string]string), Environment: make(map[string]string)}
}

func (o *Options) Merge(other Options) {
	maps.Copy(o.Values, other.Values)
	maps.Copy(o.Environment, other.Environment)
	o.ExportAll = o.ExportAll || other.ExportAll
	o.Hold = o.Hold || other.Hold
}

func Parse(args []string, allowed string) (Options, []string, error) {
	o := NewOptions()
	var operands []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			operands = append(operands, arg)
			continue
		}
		if len(arg) < 2 || !strings.ContainsRune(allowed, rune(arg[1])) {
			return o, nil, fmt.Errorf("unsupported option %q", arg)
		}
		flag := arg[1:2]
		if flag == "V" || flag == "h" {
			if len(arg) != 2 {
				return o, nil, fmt.Errorf("unsupported option %q", arg)
			}
			if flag == "V" {
				o.ExportAll = true
			} else {
				o.Hold = true
			}
			continue
		}
		value := arg[2:]
		if value == "" {
			i++
			if i >= len(args) {
				return o, nil, fmt.Errorf("missing value for -%s", flag)
			}
			value = args[i]
		}
		switch flag {
		case "l":
			if err := o.resources(value); err != nil {
				return o, nil, err
			}
		case "v":
			for _, item := range strings.Split(value, ",") {
				key, _, _ := strings.Cut(item, "=")
				if !envName.MatchString(key) || Managed(key) {
					return o, nil, fmt.Errorf("invalid or reserved environment variable %q", key)
				}
				o.Environment[key] = item
			}
		case "W":
			if !strings.HasPrefix(value, "depend=afterok:") {
				return o, nil, fmt.Errorf("only -W depend=afterok:... is supported")
			}
			o.Values["depend"] = strings.TrimPrefix(value, "depend=afterok:")
		default:
			o.Values[flag] = value
		}
	}
	return o, operands, nil
}

func (o *Options) resources(value string) error {
	for _, entry := range strings.Split(value, ",") {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" {
			return fmt.Errorf("invalid resource %q", entry)
		}
		if key == "select" {
			chunks := strings.Split(value, ":")
			if chunks[0] != "1" || strings.Contains(value, "+") {
				return fmt.Errorf("select supports exactly one chunk with count 1")
			}
			for _, chunk := range chunks[1:] {
				name, _, _ := strings.Cut(chunk, "=")
				if name != "ncpus" && name != "mem" && name != "ngpus" {
					return fmt.Errorf("unsupported select resource %q", name)
				}
				if err := o.resources(chunk); err != nil {
					return err
				}
			}
			continue
		}
		if key != "ncpus" && key != "mem" && key != "ngpus" && key != "walltime" {
			return fmt.Errorf("unsupported resource %q", key)
		}
		o.Values[key] = value
	}
	return nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func Managed(key string) bool {
	return strings.HasPrefix(key, "PBS_") || strings.HasPrefix(key, "PBOTD_") || key == "CUDA_VISIBLE_DEVICES"
}

func (o Options) Resolve(defaults model.Resources, name string) (model.Job, error) {
	j := model.Job{Name: name, Request: defaults, Queue: o.Values["q"], State: model.Queued}
	if v, ok := o.Values["N"]; ok {
		j.Name = v
	}
	if j.Name == "" || strings.ContainsAny(j.Name, `/\`) || strings.IndexFunc(j.Name, unicode.IsControl) >= 0 {
		return j, fmt.Errorf("invalid job name %q", j.Name)
	}
	for _, key := range []string{"ncpus", "ngpus"} {
		if v, ok := o.Values[key]; ok {
			n, err := strconv.ParseUint(v, 10, 31)
			if err != nil {
				return j, fmt.Errorf("invalid %s: %q", key, v)
			}
			if key == "ncpus" {
				j.Request.CPUs = int(n)
			} else {
				j.Request.GPUs = int(n)
			}
		}
	}
	if v, ok := o.Values["mem"]; ok {
		n, err := resource.Memory(v)
		if err != nil {
			return j, err
		}
		j.Request.Memory = n
	}
	if v, ok := o.Values["walltime"]; ok {
		n, err := resource.Walltime(v)
		if err != nil {
			return j, err
		}
		j.Request.Walltime = n
	}
	if j.Queue == "" {
		j.Queue = "cpu"
		if j.Request.GPUs > 0 {
			j.Queue = "gpu"
		}
	}
	if j.Queue == "gpu" {
		if _, ok := o.Values["ngpus"]; !ok && j.Request.GPUs == 0 {
			j.Request.GPUs = 1
		}
	}
	if j.Queue != "cpu" && j.Queue != "gpu" {
		return j, fmt.Errorf("unknown queue %q", j.Queue)
	}
	if (j.Queue == "gpu") != (j.Request.GPUs > 0) {
		return j, fmt.Errorf("cpu queue requires ngpus=0; gpu queue requires ngpus>0")
	}
	if err := j.Request.Validate(); err != nil {
		return j, err
	}
	if o.Hold {
		j.State = model.Held
	}
	if v, ok := o.Values["j"]; ok && v != "oe" && v != "eo" && v != "n" {
		return j, fmt.Errorf("-j requires oe, eo, or n")
	}
	if v, ok := o.Values["depend"]; ok {
		for _, id := range strings.Split(v, ":") {
			normalized, err := model.NormalizeID(id)
			if err != nil {
				return j, err
			}
			j.Dependencies = append(j.Dependencies, normalized)
		}
	}
	for _, key := range []string{"o", "e"} {
		if v, ok := o.Values[key]; ok && (v == "" || strings.ContainsRune(v, 0) || strings.Contains(v, ":")) {
			return j, fmt.Errorf("invalid local output path %q", v)
		}
	}
	return j, nil
}

func (o Options) Export(env map[string]string, shell string) (map[string]string, error) {
	out := make(map[string]string)
	for key, value := range env {
		if Managed(key) {
			continue
		}
		if o.ExportAll || key == "HOME" || key == "USER" || key == "LOGNAME" || key == "PATH" || key == "LANG" || key == "TZ" || strings.HasPrefix(key, "LC_") {
			out[key] = value
		}
	}
	for key, entry := range o.Environment {
		_, value, ok := strings.Cut(entry, "=")
		if !ok {
			value, ok = env[key]
			if !ok {
				return nil, fmt.Errorf("environment variable %s is not set", key)
			}
		}
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("environment value %s contains NUL", key)
		}
		out[key] = value
	}
	if _, ok := out["PATH"]; !ok {
		out["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	}
	out["SHELL"] = shell
	return out, nil
}

func (o Options) OutputPaths(j model.Job, sequence int64) (string, string) {
	paths := []string{o.Values["o"], o.Values["e"]}
	for i, kind := range []string{"o", "e"} {
		if paths[i] == "" {
			paths[i] = fmt.Sprintf("%s.%s%d", j.Name, kind, sequence)
		}
		if !filepath.IsAbs(paths[i]) {
			paths[i] = filepath.Join(j.CWD, paths[i])
		}
		if j.ArrayIndex != nil {
			paths[i] += fmt.Sprintf(".%d", *j.ArrayIndex)
		}
	}
	if o.Values["j"] == "oe" {
		paths[1] = paths[0]
	}
	if o.Values["j"] == "eo" {
		paths[0] = paths[1]
	}
	return paths[0], paths[1]
}

// Arguments sends the parsed CLI overrides for independent validation by the daemon.
func (o Options) Arguments() []string {
	var args []string
	keys := slices.Sorted(maps.Keys(o.Values))
	for _, key := range keys {
		value := o.Values[key]
		switch key {
		case "ncpus", "mem", "ngpus", "walltime":
			args = append(args, "-l", key+"="+value)
		case "depend":
			args = append(args, "-W", "depend=afterok:"+value)
		default:
			args = append(args, "-"+key, value)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(o.Environment)) {
		args = append(args, "-v", o.Environment[key])
	}
	if o.ExportAll {
		args = append(args, "-V")
	}
	if o.Hold {
		args = append(args, "-h")
	}
	return args
}
