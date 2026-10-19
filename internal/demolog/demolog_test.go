package demolog

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/localcheck"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/localreport"
)

func TestWriteMakesAVerifiedSyntheticWeek(t *testing.T) {
	end := time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "demo")
	r, err := Write(dir, 7, 7, end)
	if err != nil {
		t.Fatal(err)
	}
	if r.Batches != 7*24 || !r.To.Equal(time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)) || !r.From.Equal(r.To.Add(-7*24*time.Hour)) {
		t.Fatalf("result = %+v", r)
	}
	s, err := localreport.Read(dir, localreport.Range{})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Verified || s.Batches != 7*24 || s.AgentID != r.AgentID || s.LLMCalls == 0 || len(s.Transitions) != 2 || len(s.Canaries) != 4 {
		t.Fatalf("summary: verified %v, %d batches, %d calls, %d transitions, %d canary kinds", s.Verified, s.Batches, s.LLMCalls, len(s.Transitions), len(s.Canaries))
	}
	missed := false
	for _, c := range s.Canaries {
		missed = missed || c.Caught < c.Run
	}
	if !missed {
		t.Error("the demo week should show canaries getting through during the deploy without the guardrail")
	}

	// The same seed and end give the same figures (under a new identity).
	again := filepath.Join(t.TempDir(), "again")
	if _, err := Write(again, 7, 7, end); err != nil {
		t.Fatal(err)
	}
	s2, _ := localreport.Read(again, localreport.Range{})
	if s2.LLMCalls != s.LLMCalls || s2.Blocked != s.Blocked || s2.AgentID == s.AgentID {
		t.Errorf("not reproducible: %d/%d calls, %d/%d blocked, agents %s %s", s.LLMCalls, s2.LLMCalls, s.Blocked, s2.Blocked, s.AgentID, s2.AgentID)
	}
}

func TestWriteRefusesADirectoryInUse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(dir, 1, 7, time.Now()); err == nil {
		t.Fatal("wrote a demo log into a directory that was not empty")
	}
	empty := t.TempDir()
	if _, err := Write(empty, 1, 7, time.Now()); err != nil {
		t.Fatalf("an empty directory should do: %v", err)
	}
}

func TestThePolicyShowsObjectivesPassingAndFailing(t *testing.T) {
	end := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "demo")
	r, err := Write(dir, 7, 7, end)
	if err != nil {
		t.Fatal(err)
	}
	p := Policy(r)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	rep, err := localcheck.Run(localcheck.Options{DataDir: dir, Policy: p, FromFirst: true, Now: func() time.Time { return end }})
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, o := range rep.Objectives {
		status[o.ID] = o.Status
	}
	// A week that mostly passes, where the checks catch the night deploy
	// without the guardrail and the unrecorded prompt change. With no
	// provider usage, trace coverage needs attention.
	want := map[string]string{"EO-LOG-01": "attention", "EO-LIN-01": "pass", "EO-CHG-01": "fail", "EO-PII-01": "pass", "EO-CNT-01": "pass",
		"EO-GRD-01": "fail", "EO-EVAL-01": "pass", "EO-INC-01": "fail", "EO-AGT-01": "pass", "EO-RES-01": "pass", "EO-CAN-01": "fail"}
	for id, w := range want {
		if status[id] != w {
			t.Errorf("%s is %s, want %s", id, status[id], w)
		}
	}
	if len(status) != len(want) {
		t.Errorf("objectives = %v", status)
	}
}
