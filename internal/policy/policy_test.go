package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const good = `{
  "$schema": "https://raw.githubusercontent.com/Lumetrix-Advanced-Technologies/tellproof/main/spec/policy.schema.json",
  "organisation": "Kestrel", "ai_system": "Claims Triage Agent", "environment": "production", "timezone": "Australia/Sydney",
  "declared_models": [{"provider": "openai", "model": "gpt-4.1-mini", "role": "triage"}],
  "declared_regions": ["us-east-1"],
  "changes": [{"date": "2026-10-14", "kind": "model", "what": "Sonnet 4.5 to 4.6"}],
  "incidents": [{"id": "INC-7", "opened": "2026-10-13T03:10:00Z", "acknowledged": "2026-10-13T03:40:00Z", "summary": "Guardrail spike"}],
  "evals": [{"suite": "triage-regression", "model": "gpt-4.1-mini", "prompt_version": "14", "ran_at": "2026-10-12T00:00:00Z", "pass_rate": 0.95}]
}`

func TestParseAGoodPolicy(t *testing.T) {
	p, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	s := p.System("2026-10-01", "2026-10-31")
	if s.AISystem != "Claims Triage Agent" || s.Timezone != "Australia/Sydney" || len(s.DeclaredModels) != 1 || s.Period.End != "2026-10-31" {
		t.Errorf("system %+v", s)
	}
	// A missing timezone is UTC; empty lists are fine.
	p, err = Parse([]byte(`{"ai_system": "x"}`))
	if err != nil || p.Timezone != "UTC" || p.System("2026-10-01", "2026-10-01").DeclaredModels == nil {
		t.Errorf("minimal policy: %v %+v", err, p)
	}
}

func TestParseRefusesMistakes(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown key (a typo)":     `{"ai_system": "x", "declared_model": []}`,
		"no ai_system":             `{"organisation": "x"}`,
		"bad timezone":             `{"ai_system": "x", "timezone": "Sydney"}`,
		"model with spaces":        `{"ai_system": "x", "declared_models": [{"provider": "open ai", "model": "m"}]}`,
		"change kind":              `{"ai_system": "x", "changes": [{"date": "2026-10-14", "kind": "config", "what": "w"}]}`,
		"change date":              `{"ai_system": "x", "changes": [{"date": "14/10/2026", "kind": "model", "what": "w"}]}`,
		"incident time":            `{"ai_system": "x", "incidents": [{"id": "I1", "opened": "yesterday"}]}`,
		"acknowledged before open": `{"ai_system": "x", "incidents": [{"id": "I1", "opened": "2026-10-13T03:10:00Z", "acknowledged": "2026-10-13T03:00:00Z"}]}`,
		"pass rate above 1":        `{"ai_system": "x", "evals": [{"suite": "s", "model": "m", "ran_at": "2026-10-12T00:00:00Z", "pass_rate": 95}]}`,
		"trailing data":            `{"ai_system": "x"} {}`,
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.HasPrefix(err.Error(), "policy:") {
			t.Errorf("%s: error %q does not say it is the policy", name, err)
		}
	}
}

func TestFindLooksInOrder(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	_ = os.MkdirAll(data, 0o700)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	_ = os.Chdir(dir)
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got := Find(getenv, data); got != DefaultFile {
		t.Errorf("nothing anywhere: %s", got)
	}
	_ = os.WriteFile(LegacyFile, []byte(good), 0o644)
	if got := Find(getenv, data); got != LegacyFile {
		t.Errorf("the name before the rename: %s", got)
	}
	_ = os.WriteFile(filepath.Join(data, DefaultFile), []byte(good), 0o644)
	if got := Find(getenv, data); got != filepath.Join(data, DefaultFile) {
		t.Errorf("in the data directory: %s", got)
	}
	_ = os.WriteFile(DefaultFile, []byte(good), 0o644)
	if got := Find(getenv, data); got != DefaultFile {
		t.Errorf("the working directory comes first: %s", got)
	}
	env["TELLPROOF_POLICY"] = "/etc/tellproof.json"
	if got := Find(getenv, data); got != "/etc/tellproof.json" {
		t.Errorf("the environment comes first: %s", got)
	}
	// Write then Load round-trips.
	p, _ := Parse([]byte(good))
	if err := p.Write("again.json"); err != nil {
		t.Fatal(err)
	}
	if q, err := Load("again.json"); err != nil || q.AISystem != p.AISystem || len(q.Incidents) != 1 {
		t.Errorf("round trip: %v %+v", err, q)
	}
}
