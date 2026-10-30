package aggregate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/otlp"
)

// call is one model call in slot "claims" at minute m of hour h.
func call(h, m int, model, version string) otlp.Span {
	x := llm(fmt.Sprintf("t%02d%02d%s%s", h, m, model, version), at(h, m))
	x.Model, x.Slot, x.PromptName, x.PromptVersion = model, "claims", "triage", version
	return x
}

func changes(ws []Window) []string {
	var out []string
	for _, w := range ws {
		for _, t := range w.Metrics.Transitions {
			out = append(out, fmt.Sprintf("%s %s->%s %s", t.Kind, t.From, t.To, t.At[11:16]))
		}
	}
	return out
}

func hourly(a *Aggregator, spans ...otlp.Span) []Window {
	a.Add(spans, spans[len(spans)-1].Start)
	return a.FlushAll()
}

// Acceptance (D-074): two models used side by side, however their calls
// interleave and for however long, are no change. Before D-074 this app
// reported a change at every alternation (19,999 for 20,000 calls).
func TestSideBySideIsNoChange(t *testing.T) {
	a := New(DefaultConfig())
	var got []string
	for h := 0; h < 30; h++ {
		var spans []otlp.Span
		for m := 0; m < 60; m++ {
			model := "gpt-4.1-mini"
			if m%2 == 1 {
				model = "claude-sonnet-4-5"
			}
			spans = append(spans, call(h%24, m, model, ""))
		}
		got = append(got, changes(hourly(a, spans...))...)
	}
	if len(got) != 0 {
		t.Fatalf("changes = %v, want none", got)
	}
}

// Acceptance (D-074): a model swapped while the agent was down (a deploy
// restarts both) is a change, from the history saved before the restart.
func TestChangeAcrossRestart(t *testing.T) {
	a := New(DefaultConfig())
	hourly(a, call(9, 0, "gpt-4.1-mini", "14"), call(9, 5, "gpt-4.1-mini", "14"))
	saved, err := json.Marshal(a.History())
	if err != nil {
		t.Fatal(err)
	}
	var h History
	if err := json.Unmarshal(saved, &h); err != nil {
		t.Fatal(err)
	}
	b := New(DefaultConfig()) // the restarted agent
	b.SetHistory(h)
	got := changes(hourly(b, call(10, 3, "gpt-4.1", "15"), call(10, 9, "gpt-4.1", "15")))
	want := []string{"model gpt-4.1-mini->gpt-4.1 10:03", "prompt 14->15 10:03"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	// Without the history, the restarted agent could not know.
	if got := changes(hourly(New(DefaultConfig()), call(10, 3, "gpt-4.1", "15"))); len(got) != 0 {
		t.Fatalf("changes without history = %v", got)
	}
}

// Acceptance (D-074): a rolling deploy (old and new versions interleaved for a
// while, then only the new one) is one change, at the new version's first call.
func TestRollingDeployIsOneChange(t *testing.T) {
	a := New(DefaultConfig())
	hourly(a, call(8, 0, "m", "14"))
	var spans []otlp.Span
	for m := 10; m < 50; m++ {
		v := "14"
		if m%3 == 0 {
			v = "15"
		}
		spans = append(spans, call(9, m, "m", v))
	}
	got := changes(hourly(a, spans...))
	got = append(got, changes(hourly(a, call(10, 0, "m", "15")))...)
	if want := []string{"prompt 14->15 09:12"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
}

// Acceptance (D-074): in a slot's first window a clean switch is a change and
// overlapping use is not.
func TestFirstWindow(t *testing.T) {
	clean := changes(hourly(New(DefaultConfig()), call(9, 0, "a", ""), call(9, 10, "a", ""), call(9, 20, "b", ""), call(9, 30, "b", "")))
	if want := []string{"model a->b 09:20"}; !reflect.DeepEqual(clean, want) {
		t.Errorf("clean switch: %v, want %v", clean, want)
	}
	overlap := changes(hourly(New(DefaultConfig()), call(9, 0, "a", ""), call(9, 20, "b", ""), call(9, 30, "a", "")))
	if len(overlap) != 0 {
		t.Errorf("overlapping use: %v, want none", overlap)
	}
}

// Acceptance (D-074): a model unused for RetireAfter of its slot's windows
// with calls is retired; when it returns that is a change. Hours without
// calls in the slot do not age it.
func TestRetiredModelReturning(t *testing.T) {
	a := New(DefaultConfig())
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	callAt := func(hours int, model string) otlp.Span {
		x := llm(fmt.Sprintf("r%d%s", hours, model), base.Add(time.Duration(hours)*time.Hour))
		x.Model, x.Slot = model, "claims"
		return x
	}
	hourly(a, callAt(0, "a"))
	got := changes(hourly(a, callAt(1, "b"))) // a -> b
	for i := 2; i < 2+RetireAfter-2; i++ {    // with hour 1: RetireAfter-1 windows without a
		got = append(got, changes(hourly(a, callAt(i, "b")))...)
	}
	got = append(got, changes(hourly(a, callAt(100, "a")))...) // a unused for RetireAfter-1 windows with calls (and a quiet gap): still in use
	for i := 101; i < 101+RetireAfter; i++ {
		got = append(got, changes(hourly(a, callAt(i, "b")))...)
	}
	got = append(got, changes(hourly(a, callAt(200, "a")))...) // now retired: a change
	want := []string{"model a->b 01:00", "model b->a 08:00"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
}

// What History holds stays bounded however many values arrive.
func TestHistoryBounded(t *testing.T) {
	a := New(DefaultConfig())
	for i := 0; i < 3*maxSlotEntries; i++ {
		hourly(a, call(i%24, 0, fmt.Sprintf("m%03d", i), ""))
	}
	for i := 0; i < 2*maxSlots; i++ {
		x := llm(fmt.Sprintf("s%d", i), at(5, i%60))
		x.Model, x.Slot = "m", fmt.Sprintf("slot-%04d", i)
		hourly(a, x)
	}
	h := a.History()
	if n := len(h["model"]); n > maxSlots {
		t.Errorf("%d slots kept", n)
	}
	for slot, sh := range h["model"] {
		if len(sh.Unused) > maxSlotEntries {
			t.Errorf("slot %q keeps %d values", slot, len(sh.Unused))
		}
	}
}

// Acceptance (D-074): the agent and the concierge find the same changes in
// 30 hours of traffic with every case: a swap, side-by-side models, a rolling
// prompt deploy, a first-window clean switch, a retired model returning, and
// another operation's model alongside.
func TestChangesParityWithConcierge(t *testing.T) {
	str := func(v string) map[string]any { return map[string]any{"stringValue": v} }
	kv := func(k, v string) map[string]any { return map[string]any{"key": k, "value": str(v)} }
	var spans []any
	n := 0
	add := func(h, m int, slot, op, model, prompt, version string) {
		n++
		start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute).UnixNano()
		attrs := []any{kv("gen_ai.operation.name", op), kv("gen_ai.request.model", model), kv("gen_ai.agent.name", slot)}
		if version != "" {
			attrs = append(attrs, kv("tellproof.prompt.name", prompt), kv("tellproof.prompt.version", version))
		}
		spans = append(spans, map[string]any{"traceId": fmt.Sprintf("%032x", n), "spanId": fmt.Sprintf("%016x", n), "name": op,
			"startTimeUnixNano": fmt.Sprint(start), "endTimeUnixNano": fmt.Sprint(start + 1e6), "attributes": attrs})
	}
	for h := 0; h < 30; h++ {
		swap := "gpt-4.1-mini"
		if h >= 5 {
			swap = "gpt-4.1"
		}
		add(h, 1, "triage", "chat", swap, "triage", "")
		add(h, 2, "summary", "chat", "claude-a", "", "") // side by side, interleaved
		add(h, 3, "summary", "chat", "claude-b", "", "")
		add(h, 7, "summary", "chat", "claude-a", "", "")
		add(h, 4, "summary", "embeddings", "embed-1", "", "") // another operation
		old := "14"                                           // a rolling deploy: 14 and 15 in hours 6 and 7, then 15
		if h >= 8 {
			old = "15"
		}
		add(h, 5, "triage", "chat", swap, "reply", old)
		if h >= 6 {
			add(h, 6, "triage", "chat", swap, "reply", "15")
		}
		if h == 0 {
			add(h, 10, "first", "chat", "x-1", "", "")
			add(h, 20, "first", "chat", "x-2", "", "")
		}
		if h == 1 || h == 29 { // r-old: back after 27 windows unused, so retired by then
			add(h, 30, "rare", "chat", "r-old", "", "")
		} else {
			add(h, 30, "rare", "chat", "r-new", "", "")
		}
	}
	doc, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource":   map[string]any{"attributes": []any{kv("service.name", "parity")}},
		"scopeSpans": []any{map[string]any{"spans": spans}}}}})
	file := filepath.Join(t.TempDir(), "changes.jsonl")
	if err := os.WriteFile(file, append(doc, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	assertParity(t, file)

	a := New(DefaultConfig())
	sp := parseFile(t, file)
	a.Add(sp, sp[len(sp)-1].Start)
	var all []batch.Transition
	for _, w := range a.FlushAll() {
		all = append(all, w.Metrics.Transitions...)
	}
	var got []string
	for _, t := range all {
		got = append(got, fmt.Sprintf("%s %s->%s %s", t.Kind, t.From, t.To, t.At[8:16]))
	}
	want := []string{
		"model x-1->x-2 01T00:20",              // a clean switch in the slot's first window
		"model r-new->r-old 01T01:30",          // new to its slot
		"model gpt-4.1-mini->gpt-4.1 01T05:01", // the swap
		"prompt 14->15 01T06:06",               // the rolling deploy, once
		"model r-new->r-old 02T05:30",          // retired, then back
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %v\nwant %v", got, want)
	}
}
