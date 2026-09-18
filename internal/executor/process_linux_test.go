package executor

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestIdentityRejectsPIDReuseAndBootChange(t *testing.T) {
	p, err := Inspect(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !Alive(p.Identity) {
		t.Fatal("current process not alive")
	}
	wrong := p.Identity
	wrong.StartTime++
	if Alive(wrong) {
		t.Fatal("reused PID accepted")
	}
	wrong = p.Identity
	wrong.BootID = "other boot"
	if Alive(wrong) {
		t.Fatal("other boot accepted")
	}
}

func TestStatCommandWithParentheses(t *testing.T) {
	fields := []string{"S", "7", "42"}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, "999")
	p, err := parseStat(42, strconv.Itoa(42)+" (a tricky ) name) "+strings.Join(fields, " "), "boot")
	if err != nil || p.PGID != 42 || p.Parent != 7 || p.Identity.StartTime != 999 {
		t.Fatalf("%+v %v", p, err)
	}
}
