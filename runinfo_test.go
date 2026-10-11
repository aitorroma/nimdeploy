package main

import (
	"strings"
	"testing"
)

// TestRunInfo: the state carries the commit title and the phases of the run.
func TestRunInfo(t *testing.T) {
	e2 := newEnv(t, `sleep 0.2`, "")
	e2.cfg.Deploy["agency"].Before = "true"
	e2.push(t, pushOpts{delivery: "ri1"})
	st := e2.waitIdle(t)
	if st.Message != "fix footer" {
		t.Errorf("message %q", st.Message)
	}
	var names []string
	for _, p := range st.Phases {
		names = append(names, p.Name)
		if p.DurationMS < 0 || p.StartMS < 0 {
			t.Errorf("phase %+v", p)
		}
	}
	if strings.Join(names, ",") != "before,command" {
		t.Errorf("phases %v", st.Phases)
	}
	for _, p := range st.Phases {
		if p.Name == "command" && (p.DurationMS < 150 || p.Failed) {
			t.Errorf("command phase %+v", p)
		}
	}
	if got := commitTitle("  first line \r\nsecond\n"); got != "first line" {
		t.Errorf("commitTitle %q", got)
	}
}
