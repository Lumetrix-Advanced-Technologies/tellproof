package aggregate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/otlp"
)

var sampleFile = filepath.Join("..", "..", "concierge", "testdata", "otlp-sample.jsonl")

func parseFile(t *testing.T, file string) []otlp.Span {
	t.Helper()
	m, err := otlp.DefaultMapping()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var spans []otlp.Span
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		res, err := otlp.Parse(line, m)
		if err != nil {
			t.Fatal(err)
		}
		spans = append(spans, res.Spans...)
	}
	return spans
}

func toMap(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Acceptance: aggregates for the fixture equal the concierge's C-1 output.
func TestParityWithConciergeIngest(t *testing.T) {
	assertParity(t, sampleFile)
}

// Acceptance (D-066): every content attribute spelling of the leak corpus, in
// a span's attributes or its events, counts the same in the agent and the
// concierge, and a span counts once however many it carries.
func TestContentSpellingsParityWithConcierge(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "spec", "leak-corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		ContentAttributes []string `json:"content_attributes"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	str := func(v string) map[string]any { return map[string]any{"stringValue": v} }
	kv := func(k string, v map[string]any) map[string]any { return map[string]any{"key": k, "value": v} }
	n := 0
	span := func(attrs, events []any) map[string]any {
		n++
		start := at(9, 0).Add(time.Duration(n) * time.Second).UnixNano()
		return map[string]any{"traceId": fmt.Sprintf("%032x", n), "spanId": fmt.Sprintf("%016x", n), "name": "chat",
			"startTimeUnixNano": fmt.Sprint(start), "endTimeUnixNano": fmt.Sprint(start + 1e6),
			"attributes": append([]any{kv("gen_ai.operation.name", str("chat")), kv("gen_ai.request.model", str("gpt-4o"))}, attrs...), "events": events}
	}
	var spans, all []any
	for _, a := range corpus.ContentAttributes {
		spans = append(spans, span([]any{kv(a, str("x"))}, nil),
			span(nil, []any{map[string]any{"name": "content", "timeUnixNano": "1", "attributes": []any{kv(a, str("x"))}}}))
		all = append(all, kv(a, str("x")))
	}
	spans = append(spans, span(all, nil), span(nil, nil))
	doc, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource":   map[string]any{"attributes": []any{kv("service.name", str("parity")), kv("input.value", str("x"))}},
		"scopeSpans": []any{map[string]any{"spans": spans}}}}})
	file := filepath.Join(t.TempDir(), "content.jsonl")
	if err := os.WriteFile(file, append(doc, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	assertParity(t, file)
}

// assertParity checks that the agent's aggregates for an OTLP JSONL file
// equal the concierge's.
func assertParity(t *testing.T, file string) {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		t.Fatal(err)
	}
	script := "import sys, json; sys.path.insert(0, 'concierge'); import ingest; " +
		"out, _ = ingest.ingest('otlp', [sys.argv[1]], b'parity'); print(json.dumps(out['windows']))"
	cmd := exec.Command(py, "-c", script, abs)
	cmd.Dir = filepath.Join("..", "..")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("concierge ingest: %v", err)
	}
	var want []struct {
		Window   map[string]any `json:"window"`
		Metrics  map[string]any `json:"metrics"`
		Observed map[string]any `json:"observed"`
		Commit   struct {
			Leaves int `json:"leaves"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}

	a := New(DefaultConfig())
	spans := parseFile(t, file)
	a.Add(spans, spans[len(spans)-1].Start)
	got := a.FlushAll()
	if len(got) != len(want) {
		t.Fatalf("got %d windows, concierge has %d", len(got), len(want))
	}
	for i := range want {
		w := got[i]
		if g := toMap(t, map[string]string{"start": w.Window.Start, "end": w.Window.End}); !reflect.DeepEqual(g, toMap(t, want[i].Window)) {
			t.Errorf("window %d = %v, want %v", i, g, want[i].Window)
		}
		if g := toMap(t, w.Metrics); !reflect.DeepEqual(g, toMap(t, want[i].Metrics)) {
			gb, _ := json.MarshalIndent(g, "", " ")
			wb, _ := json.MarshalIndent(want[i].Metrics, "", " ")
			t.Errorf("window %d metrics differ\n go: %s\n py: %s", i, gb, wb)
		}
		if g := toMap(t, w.Observed); !reflect.DeepEqual(g, toMap(t, want[i].Observed)) {
			t.Errorf("window %d observed = %v, want %v", i, g, want[i].Observed)
		}
		if len(w.TraceIDs) != want[i].Commit.Leaves {
			t.Errorf("window %d has %d traces, concierge committed %d", i, len(w.TraceIDs), want[i].Commit.Leaves)
		}
	}
}

func at(h, m int) time.Time { return time.Date(2026, 10, 1, h, m, 0, 0, time.UTC) }

func llm(trace string, t time.Time) otlp.Span {
	return otlp.Span{TraceID: trace, Start: t, Kind: otlp.KindLLM, Provider: "openai", Model: "gpt-4o", Region: "us-east-1", InputTokens: 10, OutputTokens: 2}
}

func TestWindowsCloseAfterTheGrace(t *testing.T) {
	a := New(DefaultConfig())
	a.Add([]otlp.Span{llm("t1", at(10, 15))}, at(10, 16))
	if w := a.Flush(at(11, 4)); len(w) != 0 {
		t.Fatalf("closed %d windows before the 5-minute grace", len(w))
	}
	w := a.Flush(at(11, 5))
	if len(w) != 1 || w[0].Window.Start != "2026-10-01T10:00:00Z" || w[0].Window.End != "2026-10-01T11:00:00Z" {
		t.Fatalf("Flush = %+v", w)
	}
	if w[0].Metrics.Guardrails.LLMCalls != 1 || len(w[0].TraceIDs) != 1 {
		t.Errorf("window = %+v", w[0])
	}
	if again := a.Flush(at(12, 0)); len(again) != 0 {
		t.Errorf("a closed window was emitted twice")
	}
}

// §3: a span that arrives after its window closed goes into the next open
// window and is counted in late_spans.
func TestLateSpans(t *testing.T) {
	a := New(DefaultConfig())
	a.Add([]otlp.Span{llm("t1", at(10, 15))}, at(10, 16))
	a.Flush(at(11, 6))
	a.Add([]otlp.Span{llm("t2", at(10, 50)), llm("t1", at(10, 51))}, at(11, 10))
	w := a.Flush(at(12, 6))
	if len(w) != 1 || w[0].Window.Start != "2026-10-01T11:00:00Z" {
		t.Fatalf("Flush = %+v", w)
	}
	m := w[0].Metrics
	if m.LateSpans != 2 || m.Guardrails.LLMCalls != 2 {
		t.Errorf("late_spans %d, llm calls %d; want 2, 2", m.LateSpans, m.Guardrails.LLMCalls)
	}
	// t1 was committed in the 10:00 window; only the new trace t2 is a leaf here.
	if !reflect.DeepEqual(w[0].TraceIDs, []string{"t2"}) {
		t.Errorf("TraceIDs = %v, want [t2]", w[0].TraceIDs)
	}
}

func TestTracesCommitOnceInTheirFirstWindow(t *testing.T) {
	a := New(DefaultConfig())
	a.Add([]otlp.Span{llm("b", at(10, 59)), llm("a", at(10, 59)), llm("c", at(10, 1)), llm("a", at(11, 2))}, at(11, 3))
	w := a.FlushAll()
	if len(w) != 2 {
		t.Fatalf("got %d windows", len(w))
	}
	if !reflect.DeepEqual(w[0].TraceIDs, []string{"c", "a", "b"}) || len(w[1].TraceIDs) != 0 {
		t.Errorf("TraceIDs = %v, %v; want [c a b] then none (first span time, then ID)", w[0].TraceIDs, w[1].TraceIDs)
	}
	if w[1].Metrics.Guardrails.LLMCalls != 1 {
		t.Errorf("second window counts %d calls", w[1].Metrics.Guardrails.LLMCalls)
	}
}

func TestTransitionsCarryAcrossWindows(t *testing.T) {
	s := func(tr string, ts time.Time, model, version string) otlp.Span {
		x := llm(tr, ts)
		x.Model, x.PromptName, x.PromptVersion, x.Slot = model, "reply", version, "triage"
		return x
	}
	other := llm("o", at(10, 30))
	other.Model, other.Slot = "embed-1", "retrieval" // another slot: never a transition
	a := New(DefaultConfig())
	a.Add([]otlp.Span{s("1", at(10, 0), "m1", "13"), other, s("2", at(11, 20), "m1", "14"), s("3", at(11, 40), "m2", "14")}, at(11, 41))
	w := a.FlushAll()
	if len(w[0].Metrics.Transitions) != 0 {
		t.Errorf("first window transitions = %v", w[0].Metrics.Transitions)
	}
	want := []string{"prompt 13->14 2026-10-01T11:20:00Z", "model m1->m2 2026-10-01T11:40:00Z"}
	var got []string
	for _, tr := range w[1].Metrics.Transitions {
		got = append(got, tr.Kind+" "+tr.From+"->"+tr.To+" "+tr.At)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("transitions = %v, want %v", got, want)
	}
}

// A chat model and an embeddings model used side by side in one service are
// not model changes; a new chat model is (D-033).
func TestModelsSideBySideByOperation(t *testing.T) {
	var spans []otlp.Span
	for i, model := range []string{"chat-1", "embed-1", "chat-1", "embed-1", "chat-2"} {
		x := llm(string(rune('a'+i)), at(10, i))
		x.Model, x.Slot, x.Operation = model, "claims-api", "chat"
		if model == "embed-1" {
			x.Operation = "embeddings"
		}
		spans = append(spans, x)
	}
	a := New(DefaultConfig())
	a.Add(spans, at(10, 30))
	got := a.FlushAll()[0].Metrics.Transitions
	if len(got) != 1 || got[0].From != "chat-1" || got[0].To != "chat-2" {
		t.Errorf("transitions = %+v; want only chat-1 -> chat-2", got)
	}
}

func TestAnomalyThresholds(t *testing.T) {
	cfg := DefaultConfig()
	mk := func(n, errs, blocked int) []otlp.Span {
		var out []otlp.Span
		for i := 0; i < n; i++ {
			s := llm("t", at(10, i%60))
			s.TraceID = string(rune('a'+i%26)) + string(rune('a'+i/26))
			s.Error = i < errs
			s.Guardrail = "allow"
			if i < blocked {
				s.Guardrail = "block"
			}
			out = append(out, s)
		}
		return out
	}
	tests := []struct {
		name         string
		n, errs, blk int
		want         []string
	}{
		{"too few calls to judge", 19, 19, 19, nil},
		{"error rate at 5%", 20, 1, 0, []string{"error_rate"}},
		{"error rate under 5%", 21, 1, 0, nil},
		{"block rate at 3%", 100, 0, 3, []string{"guardrail_rate"}},
		{"both", 40, 2, 2, []string{"error_rate", "guardrail_rate"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(cfg)
			a.Add(mk(tt.n, tt.errs, tt.blk), at(10, 59))
			var got []string
			for _, an := range a.FlushAll()[0].Metrics.Anomalies {
				got = append(got, an.Kind)
				if an.Start != "2026-10-01T10:00:00Z" || an.End != "2026-10-01T11:00:00Z" {
					t.Errorf("anomaly window %s..%s", an.Start, an.End)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("anomalies = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEmptySlicesEncodeAsArrays(t *testing.T) {
	a := New(DefaultConfig())
	a.Add([]otlp.Span{{TraceID: "t", Start: at(10, 0), Kind: otlp.KindOther, Provider: "unknown", Model: "unknown", Region: "unknown"}}, at(10, 1))
	b, err := json.Marshal(a.FlushAll()[0].Metrics)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"tokens":[]`, `"evals":[]`, `"transitions":[]`, `"anomalies":[]`} {
		if !bytes.Contains(b, []byte(field)) {
			t.Errorf("metrics JSON lacks %s: %s", field, b)
		}
	}
}

func TestConcurrentAdd(t *testing.T) {
	a := New(DefaultConfig())
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				a.Add([]otlp.Span{llm(string(rune('A'+g))+string(rune('a'+i%26))+string(rune('a'+i/26)), at(10, i%60))}, at(10, 59))
			}
		}(g)
	}
	wg.Wait()
	w := a.FlushAll()
	if len(w) != 1 || w[0].Metrics.Guardrails.LLMCalls != 800 || len(w[0].TraceIDs) != 800 {
		t.Errorf("windows %d, calls %d, traces %d", len(w), w[0].Metrics.Guardrails.LLMCalls, len(w[0].TraceIDs))
	}
}

func TestEvalsGoIntoTheirHour(t *testing.T) {
	a := New(DefaultConfig())
	a.Add([]otlp.Span{llm("t1", at(10, 5))}, at(10, 6))
	a.AddEvals([]batch.Eval{{Suite: "s", Model: "m", PromptVersion: "1", RanAt: "2026-10-01T10:30:00Z", PassRate: 0.9}, {RanAt: "bad"}}, at(10, 31))
	a.Flush(at(11, 6))
	a.AddEvals([]batch.Eval{{Suite: "late", RanAt: "2026-10-01T10:45:00Z"}}, at(11, 20)) // its hour is closed
	w := a.FlushAll()
	if len(w) != 1 || len(w[0].Metrics.Evals) != 1 || w[0].Metrics.Evals[0].Suite != "late" || w[0].Window.Start != "2026-10-01T11:00:00Z" {
		t.Fatalf("late eval windows = %+v", w)
	}
}

// FlushOpen emits a window early without closing its hour: later spans for
// that hour are not late, and each trace is committed once (S-5).
func TestFlushOpenKeepsTheHourOpen(t *testing.T) {
	a := New(DefaultConfig())
	a.Add([]otlp.Span{llm("t1", at(10, 5)), llm("t2", at(10, 6))}, at(10, 7))
	early := a.FlushOpen()
	if len(early) != 1 || early[0].Metrics.Guardrails.LLMCalls != 2 || len(early[0].TraceIDs) != 2 {
		t.Fatalf("early window %+v", early)
	}
	a.Add([]otlp.Span{llm("t2", at(10, 20)), llm("t3", at(10, 30))}, at(10, 31)) // t2 continues; t3 is new
	if w := a.Flush(at(11, 4)); len(w) != 0 {
		t.Fatal("the hour closed before its grace")
	}
	rest := a.Flush(at(11, 6))
	if len(rest) != 1 || rest[0].Window.Start != early[0].Window.Start || rest[0].Metrics.LateSpans != 0 ||
		rest[0].Metrics.Guardrails.LLMCalls != 2 || len(rest[0].TraceIDs) != 1 || rest[0].TraceIDs[0] != "t3" {
		t.Errorf("the rest of the hour: %+v", rest)
	}
}
