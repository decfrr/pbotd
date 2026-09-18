package pbs

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

func Array(value string) ([]int, int, error) {
	ranges, capText, capped := strings.Cut(value, "%")
	limit := 0
	if capped {
		n, err := strconv.ParseUint(capText, 10, 31)
		if err != nil || n == 0 {
			return nil, 0, fmt.Errorf("array concurrency limit must be positive")
		}
		limit = int(n)
	}
	seen := make(map[int]bool)
	for _, segment := range strings.Split(ranges, ",") {
		span, stepText, stepped := strings.Cut(segment, ":")
		step := uint64(1)
		if stepped {
			n, err := strconv.ParseUint(stepText, 10, 31)
			if err != nil || n == 0 {
				return nil, 0, fmt.Errorf("array step must be positive")
			}
			step = n
		}
		startText, endText, ranged := strings.Cut(span, "-")
		start, err := strconv.ParseUint(startText, 10, 31)
		if err != nil {
			return nil, 0, fmt.Errorf("invalid array index %q", startText)
		}
		end := start
		if ranged {
			end, err = strconv.ParseUint(endText, 10, 31)
			if err != nil || end < start {
				return nil, 0, fmt.Errorf("invalid array range %q", span)
			}
		} else if stepped {
			return nil, 0, fmt.Errorf("array steps require a range")
		}
		if (end-start)/step+1 > 10000 {
			return nil, 0, fmt.Errorf("array exceeds 10000 subjobs")
		}
		for n := start; n <= end; n += step {
			seen[int(n)] = true
			if len(seen) > 10000 {
				return nil, 0, fmt.Errorf("array exceeds 10000 subjobs")
			}
		}
	}
	indices := make([]int, 0, len(seen))
	for n := range seen {
		indices = append(indices, n)
	}
	slices.Sort(indices)
	return indices, limit, nil
}
