package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/aggregate"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/batchsign"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/commitment"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/enrol"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/localreport"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/otlp"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/queue"
)

// sampleLog writes the concierge's OTLP sample as a local evidence log.
func sampleLog(t *testing.T) string {
	t.Helper()
	m, _ := otlp.DefaultMapping()
	raw, err := os.ReadFile(filepath.Join("..", "..", "concierge", "testdata", "otlp-sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	a := aggregate.New(aggregate.DefaultConfig())
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			res, _ := otlp.Parse(line, m)
			a.Add(res.Spans, res.Spans[0].Start)
		}
	}
	dir := t.TempDir()
	id, _ := enrol.LocalIdentity(dir)
	b, _ := batchsign.NewBuilder(id, m.Version)
	q, _ := queue.Open(filepath.Join(dir, localreport.Dir))
	for _, w := range a.FlushAll() {
		env, _, next, err := b.Build(context.Background(), q.State(), w, commitment.ForWindow([]byte("s"), w.TraceIDs).Commit, nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = q.Enqueue(env, next)
	}
	return dir
}

func cli(args ...string) (int, string) {
	var out bytes.Buffer
	code := run(context.Background(), args, func(string) string { return "" }, &out, &out)
	return code, out.String()
}

// Acceptance (D-055): declare from the log, check it in CI, export an AI-BOM.
func TestPolicyInitCheckAndAIBOM(t *testing.T) {
	data := sampleLog(t)
	pol := filepath.Join(t.TempDir(), "tellproof.policy.json")

	code, out := cli("policy", "init", "--policy", pol, "--data-dir", data, "--name", "Claims Triage Agent", "--timezone", "UTC")
	if code != 0 || !strings.Contains(out, "3 models and 2 regions") || !strings.Contains(out, "2 model or prompt change(s)") {
		t.Fatalf("policy init: %d %s", code, out)
	}
	if code, out := cli("policy", "validate", "--policy", pol); code != 0 || !strings.Contains(out, "is valid: Claims Triage Agent, 3 models, 2 regions") {
		t.Errorf("policy validate: %d %s", code, out)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"ai_system": "x", "declared_model": []}`), 0o644)
	if code, out := cli("policy", "validate", "--policy", bad); code != 1 || !strings.Contains(out, "declared_model") {
		t.Errorf("policy validate on a typo: %d %s", code, out)
	}
	if code, out := cli("policy", "init", "--policy", pol, "--data-dir", data); code != 2 || !strings.Contains(out, "exists") {
		t.Errorf("policy init over an existing file: %d %s", code, out)
	}

	// The sample's changes are not recorded, so the check fails (exit 1) and says what is missing.
	junit := filepath.Join(t.TempDir(), "check.xml")
	code, out = cli("check", "--policy", pol, "--data-dir", data, "--since", "2026-09-01", "--until", "2026-09-01", "--junit", junit)
	if code != 1 || !strings.Contains(out, "✓ PASS       EO-LIN-01") || !strings.Contains(out, "missing change:") {
		t.Fatalf("check: %d %s", code, out)
	}
	if x, err := os.ReadFile(junit); err != nil || !bytes.Contains(x, []byte(`<testsuite name="tellproof: Claims Triage Agent`)) {
		t.Errorf("junit: %v", err)
	}
	code, out = cli("check", "--policy", pol, "--data-dir", data, "--since", "2026-09-01", "--until", "2026-09-01", "--json")
	var rep struct {
		Objectives []struct{ ID, Status string } `json:"objectives"`
	}
	if code != 1 || json.Unmarshal([]byte(out), &rep) != nil || len(rep.Objectives) != 11 {
		t.Errorf("check --json: %d %s", code, out)
	}

	// Usage and policy errors are 2; no policy says how to make one.
	if code, out := cli("check", "--policy", filepath.Join(t.TempDir(), "none.json"), "--data-dir", data); code != 2 || !strings.Contains(out, "policy init") {
		t.Errorf("no policy: %d %s", code, out)
	}
	if code, _ := cli("check", "--data-dir", data, "--bogus"); code != 2 {
		t.Errorf("unknown flag: %d", code)
	}
	if code, _ := cli("policy"); code != 2 {
		t.Errorf("policy without init: %d", code)
	}

	bom := filepath.Join(t.TempDir(), "aibom.json")
	code, out = cli("aibom", "--policy", pol, "--data-dir", data, "--out", bom)
	b, _ := os.ReadFile(bom)
	var doc struct {
		BOMFormat  string `json:"bomFormat"`
		Components []struct {
			Type string `json:"type"`
		} `json:"components"`
	}
	if code != 0 || json.Unmarshal(b, &doc) != nil || doc.BOMFormat != "CycloneDX" || len(doc.Components) != 3 {
		t.Errorf("aibom: %d %s %s", code, out, b)
	}

	// A log that fails verification is never checked or exported.
	files, _ := filepath.Glob(filepath.Join(data, localreport.Dir, "*.batch.json"))
	_ = os.Remove(files[len(files)-1])
	if code, out := cli("check", "--policy", pol, "--data-dir", data); code != 1 || !strings.Contains(out, "Nothing was evaluated") {
		t.Errorf("check on a broken log: %d %s", code, out)
	}
	if code, _ := cli("aibom", "--policy", pol, "--data-dir", data); code != 1 {
		t.Errorf("aibom on a broken log: %d", code)
	}
}
