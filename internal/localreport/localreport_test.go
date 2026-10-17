package localreport

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/aggregate"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/batchsign"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/commitment"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/enrol"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/otlp"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/queue"
)

// sample is the concierge's OTLP sample month, aggregated into windows.
func sample(t *testing.T) []aggregate.Window {
	t.Helper()
	m, _ := otlp.DefaultMapping()
	b, err := os.ReadFile(filepath.Join("..", "..", "concierge", "testdata", "otlp-sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	a := aggregate.New(aggregate.DefaultConfig())
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			res, _ := otlp.Parse(line, m)
			a.Add(res.Spans, res.Spans[0].Start)
		}
	}
	return a.FlushAll()
}

// writeLog writes windows as a local evidence log; corrupt, if set, changes
// the state a batch is built on.
func writeLog(t *testing.T, windows []aggregate.Window, corrupt func(seq int64, st batchsign.State) batchsign.State) string {
	t.Helper()
	dir := t.TempDir()
	id, err := enrol.LocalIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := batchsign.NewBuilder(id, "v2")
	q, err := queue.Open(filepath.Join(dir, Dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range windows {
		st := q.State()
		if corrupt != nil {
			st = corrupt(st.Seq+1, st)
		}
		env, _, next, err := b.Build(context.Background(), st, w, commitment.ForWindow([]byte("local-report-secret"), w.TraceIDs).Commit, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Enqueue(env, next); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestReadVerifiesAndSumsTheLog(t *testing.T) {
	windows := sample(t)
	if len(windows) < 2 {
		t.Fatalf("the sample has %d windows", len(windows))
	}
	dir := writeLog(t, windows, nil)
	s, err := Read(dir, Range{})
	if err != nil {
		t.Fatal(err)
	}
	var calls, guarded, traces int64
	for _, w := range windows {
		calls += w.Metrics.Guardrails.LLMCalls
		guarded += w.Metrics.Guardrails.GuardedCalls
		traces += int64(len(w.TraceIDs))
	}
	if !s.Verified || s.Batches != len(windows) || s.LLMCalls != calls || s.GuardedCalls != guarded || s.Traces != traces || len(s.Models) == 0 {
		t.Fatalf("summary = %+v (want %d batches, %d calls, %d guarded, %d traces)", s, len(windows), calls, guarded, traces)
	}
	// One point per window, summing to the totals; the newest batch first in Recent.
	var seriesCalls int64
	for _, p := range s.Series {
		seriesCalls += p.LLMCalls
	}
	if len(s.Series) != len(windows) || seriesCalls != calls || len(s.Recent) != len(windows) || s.Recent[0].Seq != int64(len(windows)) || s.Recent[0].Digest != s.LastHash {
		t.Errorf("series %d points, %d calls; recent %+v", len(s.Series), seriesCalls, s.Recent)
	}
	var modelCalls int64
	for _, m := range s.Models {
		modelCalls += m.Calls
	}
	if modelCalls == 0 {
		t.Error("no calls per model")
	}
	// A range sums only its windows but still checks the whole chain.
	first, _ := time.Parse(time.RFC3339, windows[0].Window.Start)
	one, _ := Read(dir, Range{From: first, To: first.Add(time.Hour)})
	if !one.Verified || one.Batches != len(windows) || one.LLMCalls != windows[0].Metrics.Guardrails.LLMCalls || one.From != windows[0].Window.Start {
		t.Errorf("one window: %+v", one)
	}
	var text bytes.Buffer
	err = WriteText(&text, s)
	t.Log("\n" + text.String())
	if err != nil || !strings.Contains(text.String(), "Verified:") || !strings.Contains(text.String(), EarlyAccess) {
		t.Errorf("text: %v\n%s", err, text.String())
	}
}

func TestReadCatchesABrokenChain(t *testing.T) {
	windows := sample(t)
	bogus := "sha256:" + strings.Repeat("0", 64)
	dir := writeLog(t, windows, func(seq int64, st batchsign.State) batchsign.State {
		if seq == 2 {
			st.Prev = &bogus
		}
		return st
	})
	s, err := Read(dir, Range{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Verified || s.Batches != 1 || !strings.Contains(s.Problem, "batch 2 does not follow batch 1") {
		t.Fatalf("broken chain: %+v", s)
	}
	var text bytes.Buffer
	_ = WriteText(&text, s)
	if !strings.Contains(text.String(), "FAILED: batch 2") {
		t.Errorf("text: %s", text.String())
	}
}

// A log copied with another key's agent.pub does not verify.
func TestReadRefusesAnotherKey(t *testing.T) {
	dir := writeLog(t, sample(t)[:1], nil)
	other := t.TempDir()
	if _, err := enrol.LocalIdentity(other); err != nil {
		t.Fatal(err)
	}
	pub, _ := os.ReadFile(filepath.Join(other, enrol.PublicKeyFile))
	_ = os.WriteFile(filepath.Join(dir, enrol.PublicKeyFile), pub, 0o644)
	if _, err := Read(dir, Range{}); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("another key: %v", err)
	}
}

// D-057: the SDK scenario's canaries reach the report, summed by kind, with
// the missed kind named; a log without canaries says how to start them.
func TestReportShowsCanaries(t *testing.T) {
	m, _ := otlp.DefaultMapping()
	b, err := os.ReadFile(filepath.Join("..", "..", "sdk", "testdata", "python-export.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	a := aggregate.New(aggregate.DefaultConfig())
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			res, _ := otlp.Parse(line, m)
			a.Add(res.Spans, res.Spans[0].Start)
		}
	}
	s, err := Read(writeLog(t, a.FlushAll(), nil), Range{})
	if err != nil || !s.Verified {
		t.Fatalf("%v %+v", err, s)
	}
	if len(s.Canaries) != 2 || s.Canaries[0].Kind != "pii" || s.Canaries[0].Run != 2 || s.Canaries[0].Caught != 1 {
		t.Errorf("canaries %+v", s.Canaries)
	}
	var out bytes.Buffer
	_ = WriteText(&out, s)
	if want := "Canaries        2 of 3 known-bad probes caught by the guardrail (66.7%); missed: pii (1)"; !strings.Contains(out.String(), want) {
		t.Errorf("report lacks %q:\n%s", want, out.String())
	}
	none, _ := Read(writeLog(t, sample(t), nil), Range{})
	out.Reset()
	_ = WriteText(&out, none)
	if !strings.Contains(out.String(), "Canaries        none run") || none.Canaries == nil {
		t.Errorf("no canaries:\n%s", out.String())
	}
}
