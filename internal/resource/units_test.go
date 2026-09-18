package resource

import "testing"

func TestMemoryUnitsAndOverflow(t *testing.T) {
	for input, expected := range map[string]int64{"1": 1, "0": 0, "2kb": 2048, "8GB": 8 << 30, "3GiB": 3 << 30, "1t": 1 << 40} {
		n, err := Memory(input)
		if err != nil || n != expected {
			t.Fatalf("%s: %d %v", input, n, err)
		}
	}
	for _, input := range []string{"-1g", "1.5GiB", "8watts", "9223372036854775807t", ""} {
		if _, err := Memory(input); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestWalltime(t *testing.T) {
	for input, expected := range map[string]int64{"90": 90, "00:01:30": 90, "100:00:00": 360000} {
		n, err := Walltime(input)
		if err != nil || n != expected {
			t.Fatalf("%s: %d %v", input, n, err)
		}
	}
	for _, input := range []string{"0", "-1", "1:30", "1:60:0", "1:0:60", "999999999999:0:0", "9223372036854775807"} {
		if _, err := Walltime(input); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}
