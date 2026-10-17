package localcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/aggregate"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/aibom"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/batchsign"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/commitment"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/enrol"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/evaluate"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/localreport"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/otlp"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/policy"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/queue"
)

// writeLog writes the concierge's OTLP sample (two hours on 1 Sep 2026: three
// models, a prompt change, a model change, guardrail anomalies) as a local log.
func writeLog(t *testing.T) string {
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
	id, err := enrol.LocalIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := batchsign.NewBuilder(id, m.Version)
	q, _ := queue.Open(filepath.Join(dir, localreport.Dir))
	for _, w := range a.FlushAll() {
		env, _, next, err := b.Build(context.Background(), q.State(), w, commitment.ForWindow([]byte("check-secret"), w.TraceIDs).Commit, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Enqueue(env, next); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func status(r *Report) map[string]string {
	out := map[string]string{}
	for _, o := range r.Objectives {
		out[o.ID] = o.Status
	}
	return out
}

var sampleDay = func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) }

func basePolicy() *policy.Policy {
	p, _ := policy.Parse([]byte(`{"ai_system": "Claims Triage Agent", "timezone": "UTC",
	  "declared_models": [{"provider": "aws.bedrock", "model": "anthropic.claude-sonnet-4-5"}, {"provider": "aws.bedrock", "model": "anthropic.claude-sonnet-4-6"},
	                      {"provider": "openai", "model": "gpt-4.1-mini"}],
	  "declared_regions": ["ap-southeast-2", "us-east-1"]}`))
	return p
}

// Acceptance (D-055): the local log, checked against a policy, gives the
// same objective results as the pack evaluator on the same evidence.
func TestCheckIsThePackEvaluation(t *testing.T) {
	dir := writeLog(t)
	pol := basePolicy()
	r, err := Run(Options{DataDir: dir, Policy: pol, Now: sampleDay})
	if err != nil {
		t.Fatal(err)
	}
	if r.Period.Start != "2026-09-01" || r.Period.End != "2026-09-01" || !r.Log.Verified || r.Log.Batches != 2 || len(r.Objectives) != 11 {
		t.Fatalf("report %+v", r)
	}
	// The same evaluation, computed the issuer's way from the same batches.
	_, bs, _ := localreport.ReadBatches(dir, localreport.Range{})
	w, u, e, _ := evaluate.FromBatches(bs)
	want, err := evaluate.Evaluate(evaluate.Input{System: pol.System("2026-09-01", "2026-09-01"), Windows: w, Usage: u, Evals: e,
		Changes: []evaluate.Change{}, Incidents: []evaluate.Incident{}})
	if err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(r.Objectives)
	wb, _ := json.Marshal(want.Objectives)
	if !bytes.Equal(gb, wb) {
		t.Errorf("check differs from the pack evaluator:\n%s\n%s", gb, wb)
	}
	st := status(r)
	if st["EO-LIN-01"] != "pass" || st["EO-RES-01"] != "pass" || st["EO-CHG-01"] != "fail" || st["EO-LOG-01"] != "attention" {
		t.Errorf("statuses %v", st)
	}
	if !r.Failed(false) || r.Summary["pass"]+r.Summary["attention"]+r.Summary["fail"] != 11 {
		t.Errorf("summary %v", r.Summary)
	}
	var text bytes.Buffer
	_ = WriteText(&text, r)
	for _, want := range []string{"✕ FAIL       EO-CHG-01", "missing change:", "missing incident:", "target:", "supports:", "self-assessment"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, text.String())
		}
	}
}

// Declarations and registers change the verdicts the way a reviewer expects.
func TestPolicyDrivesTheVerdicts(t *testing.T) {
	dir := writeLog(t)
	// An undeclared model and region fail lineage and residency.
	pol := basePolicy()
	pol.DeclaredModels = pol.DeclaredModels[:2]
	pol.DeclaredRegions = []string{"ap-southeast-2"}
	r, _ := Run(Options{DataDir: dir, Policy: pol, Now: sampleDay})
	if st := status(r); st["EO-LIN-01"] != "fail" || st["EO-RES-01"] != "fail" {
		t.Errorf("undeclared: %v", st)
	}
	// Recording the two changes the telemetry shows passes the change log.
	pol = basePolicy()
	pol.Changes = []evaluate.Change{{Date: "2026-09-01", Kind: "prompt", What: "triage prompt 13 to 14"}, {Date: "2026-09-01", Kind: "model", What: "Sonnet 4.5 to 4.6"}}
	r, _ = Run(Options{DataDir: dir, Policy: pol, Now: sampleDay})
	if st := status(r); st["EO-CHG-01"] != "pass" {
		t.Errorf("changes recorded: %v (%s)", st, r.Objectives[2].Measured)
	}
}

func TestReportsAndExitRules(t *testing.T) {
	dir := writeLog(t)
	r, _ := Run(Options{DataDir: dir, Policy: basePolicy(), Now: sampleDay})
	var x bytes.Buffer
	if err := WriteJUnit(&x, r, false); err != nil {
		t.Fatal(err)
	}
	var suite struct {
		Tests    int `xml:"tests,attr"`
		Failures int `xml:"failures,attr"`
		Skipped  int `xml:"skipped,attr"`
		Cases    []struct {
			Name string `xml:"name,attr"`
		} `xml:"testcase"`
	}
	if err := xml.Unmarshal(x.Bytes(), &suite); err != nil || suite.Tests != 11 || len(suite.Cases) != 11 ||
		suite.Failures != r.Summary["fail"] || suite.Skipped != r.Summary["attention"] {
		t.Fatalf("junit %v %+v", err, suite)
	}
	x.Reset()
	_ = WriteJUnit(&x, r, true)
	_ = xml.Unmarshal(x.Bytes(), &suite)
	if suite.Failures != r.Summary["fail"]+r.Summary["attention"] || suite.Skipped != 0 {
		t.Errorf("strict junit %+v", suite)
	}
	var j bytes.Buffer
	_ = WriteJSON(&j, r)
	var back Report
	if err := json.Unmarshal(j.Bytes(), &back); err != nil || len(back.Objectives) != 11 || back.Log.AgentID == "" {
		t.Errorf("json %v %+v", err, back)
	}
	// Explicit and invalid periods.
	if r, err := Run(Options{DataDir: dir, Policy: basePolicy(), Since: "2026-08-01", Until: "2026-09-30"}); err != nil || r.Period.Start != "2026-08-01" {
		t.Errorf("explicit period: %v", err)
	}
	if _, err := Run(Options{DataDir: dir, Policy: basePolicy(), Since: "2026-09-30", Until: "2026-09-01"}); err == nil {
		t.Error("a backwards period was accepted")
	}
}

// Nothing is evaluated from a log that does not verify.
func TestUnverifiedLogIsNotEvaluated(t *testing.T) {
	dir := writeLog(t)
	files, _ := filepath.Glob(filepath.Join(dir, localreport.Dir, "*.batch.json"))
	_ = os.Remove(files[0])
	r, err := Run(Options{DataDir: dir, Policy: basePolicy(), Now: sampleDay})
	if !errors.Is(err, ErrUnverified) || r == nil || r.Log.Verified || r.Objectives != nil {
		t.Fatalf("missing batch: %v %+v", err, r)
	}
}

func TestAIBOMFromTheLog(t *testing.T) {
	dir := writeLog(t)
	sum, bs, _ := localreport.ReadBatches(dir, localreport.Range{})
	pol := basePolicy()
	pol.DeclaredModels = append(pol.DeclaredModels, evaluate.DeclaredModel{Provider: "anthropic", Model: "claude-opus-4-1", Role: "fallback"})
	b := aibom.Build(bs, aibom.Options{Policy: pol, AgentID: sum.AgentID, LastDigest: sum.LastHash, Version: "test"})
	if b.BOMFormat != "CycloneDX" || b.SpecVersion != "1.6" || !strings.HasPrefix(b.SerialNumber, "urn:uuid:") || len(b.SerialNumber) != 45 {
		t.Fatalf("header %+v", b)
	}
	if b.Metadata.Component.Name != "Claims Triage Agent" || len(b.Components) != 4 {
		t.Fatalf("components %d: %+v", len(b.Components), b.Components)
	}
	props := func(c aibom.Component) map[string]string {
		m := map[string]string{}
		for _, p := range c.Properties {
			m[p.Name] = p.Value
		}
		return m
	}
	for _, c := range b.Components {
		p := props(c)
		if c.Type != "machine-learning-model" || c.Publisher == "" || p["tellproof:declared"] != "true" {
			t.Errorf("component %+v", c)
		}
		switch c.Name {
		case "claude-opus-4-1":
			if p["tellproof:observed"] != "false" || c.ModelCard == nil || c.ModelCard.Considerations.UseCases[0] != "fallback" {
				t.Errorf("declared, never seen: %+v", c)
			}
		case "anthropic.claude-sonnet-4-5":
			if p["tellproof:observed"] != "true" || p["tellproof:regions"] != "ap-southeast-2" || p["tellproof:calls"] == "0" || p["tellproof:first_seen"] == "" {
				t.Errorf("observed: %v", p)
			}
		}
	}
	if len(b.Dependencies) != 5 || b.Dependencies[0].Ref != "ai-system" || len(b.Dependencies[0].DependsOn) != 4 {
		t.Errorf("dependencies %+v", b.Dependencies)
	}
	var out bytes.Buffer
	if err := aibom.Write(&out, b); err != nil || !json.Valid(out.Bytes()) {
		t.Errorf("write: %v", err)
	}
}

// D-057: on real evidence every objective can be made to fail, and every
// tampering of a copy of the log is refused; the real log is untouched.
func TestSelfTest(t *testing.T) {
	dir := writeLog(t)
	before, _ := os.ReadDir(filepath.Join(dir, localreport.Dir))
	r, err := SelfTest(Options{DataDir: dir, Policy: basePolicy(), Now: sampleDay})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Objectives) != 11 || len(r.Tamper) != 3 || r.Failed() {
		t.Fatalf("self-test %+v", r)
	}
	for _, o := range r.Objectives {
		if o.Outcome != Falsifiable || o.Status != "fail" || o.Injected == "" {
			t.Errorf("%s: %+v", o.ID, o)
		}
	}
	for _, tt := range r.Tamper {
		if !tt.Detected || tt.Problem == "" {
			t.Errorf("tamper %+v", tt)
		}
	}
	after, _ := os.ReadDir(filepath.Join(dir, localreport.Dir))
	if len(after) != len(before) {
		t.Error("the self-test changed the real log")
	}
	if again, _ := Run(Options{DataDir: dir, Policy: basePolicy(), Now: sampleDay}); !again.Log.Verified {
		t.Error("the real log no longer verifies")
	}
	var text bytes.Buffer
	_ = WriteSelfTestText(&text, r)
	for _, want := range []string{"✓ CAN FAIL     EO-CAN-01", "✓ DETECTED     the first batch removed", "11 of 11 objectives can fail"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, text.String())
		}
	}
}

// A register that leaves no room for a violation makes its objective vacuous,
// and the self-test fails.
func TestSelfTestFindsVacuousRegisters(t *testing.T) {
	dir := writeLog(t)
	pol := basePolicy()
	for _, d := range []string{"2026-08-31", "2026-09-01", "2026-09-02"} {
		pol.Changes = append(pol.Changes, evaluate.Change{Date: d, Kind: "model", What: "every day"}, evaluate.Change{Date: d, Kind: "prompt", What: "every day"})
	}
	for h := -1; h <= 24; h++ {
		at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(h) * time.Hour).Format(time.RFC3339)
		pol.Incidents = append(pol.Incidents, evaluate.Incident{ID: at, Opened: at, Acknowledged: at})
	}
	r, err := SelfTest(Options{DataDir: dir, Policy: pol, Now: sampleDay})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, o := range r.Objectives {
		got[o.ID] = o.Outcome
	}
	if got["EO-CHG-01"] != Vacuous || got["EO-INC-01"] != Vacuous || got["EO-LIN-01"] != Falsifiable || !r.Failed() {
		t.Errorf("outcomes %v", got)
	}
}

// Every objective in the spec has a mutation.
func TestEveryObjectiveHasAMutation(t *testing.T) {
	r, _ := Run(Options{DataDir: writeLog(t), Policy: basePolicy(), Now: sampleDay})
	for _, o := range r.Objectives {
		if mutations[o.ID] == nil {
			t.Errorf("%s has no self-test mutation", o.ID)
		}
	}
}
