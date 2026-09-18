package resource

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var memoryPattern = regexp.MustCompile(`^([0-9]+)([a-zA-Z]*)$`)

func Memory(value string) (int64, error) {
	m := memoryPattern.FindStringSubmatch(value)
	if m == nil {
		return 0, fmt.Errorf("invalid memory value %q", value)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory value %q", value)
	}
	var shift uint
	switch strings.ToLower(m[2]) {
	case "", "b":
	case "k", "kb", "kib":
		shift = 10
	case "m", "mb", "mib":
		shift = 20
	case "g", "gb", "gib":
		shift = 30
	case "t", "tb", "tib":
		shift = 40
	default:
		return 0, fmt.Errorf("unsupported memory unit in %q", value)
	}
	if n > math.MaxInt64>>shift {
		return 0, fmt.Errorf("memory value overflows: %q", value)
	}
	return n << shift, nil
}

func Walltime(value string) (int64, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 1 && len(parts) != 3 {
		return 0, fmt.Errorf("walltime must be seconds or HH:MM:SS")
	}
	var seconds int64
	for i, part := range parts {
		if part == "" || strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, fmt.Errorf("invalid walltime %q", value)
		}
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil || (i > 0 && n >= 60) || seconds > (math.MaxInt64-n)/60 {
			return 0, fmt.Errorf("invalid or overflowing walltime %q", value)
		}
		seconds = seconds*60 + n
	}
	// time.Duration bounds the runner's timer as well as parsing.
	if seconds <= 0 || seconds > math.MaxInt64/1_000_000_000 {
		return 0, fmt.Errorf("walltime must be positive and fit a duration")
	}
	return seconds, nil
}
