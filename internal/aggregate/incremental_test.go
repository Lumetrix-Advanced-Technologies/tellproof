package aggregate

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/otlp"
)

// Acceptance (D-072): adding spans up as they arrive gives exactly the windows
// the old sort-everything-at-close aggregator gave, whatever the arrival
// order: same metrics and the same leaves in the same order (transitions
// changed meaning in D-074 and are tested on their own). Random traffic with the awkward cases: spans out of order, equal
// start times, OTLP, Langfuse and missing trace IDs, more canary kinds than a
// window keeps, several slots, models and prompt versions, late spans and
// early flushes.
func TestIncrementalMatchesReference(t *testing.T) {
	base := time.Date(2026, 10, 30, 5, 0, 0, 0, time.UTC)
	for seed := uint64(1); seed <= 60; seed++ {
		r := rand.New(rand.NewPCG(seed, 7))
		cfg := DefaultConfig()
		cfg.MinCalls = 5
		got, want := New(cfg), newRef(cfg)
		traces := make([]string, 1+r.IntN(40))
		for i := range traces {
			switch r.IntN(5) {
			case 0:
				traces[i] = fmt.Sprintf("lf-trace-%d", r.IntN(1000)) // Langfuse: any string
			case 1:
				traces[i] = ""
			default:
				traces[i] = fmt.Sprintf("%016x%016x", r.Uint64(), r.Uint64())
			}
		}
		pick := func(xs ...string) string { return xs[r.IntN(len(xs))] }
		now := base.Add(10 * time.Minute)
		for step := 0; step < 25; step++ {
			spans := make([]otlp.Span, r.IntN(30))
			for i := range spans {
				s := otlp.Span{
					TraceID: traces[r.IntN(len(traces))],
					// coarse starts so ties are common; some in earlier hours (late)
					Start:         base.Add(time.Duration(r.IntN(9)-2) * 20 * time.Minute).Add(time.Duration(r.IntN(3)) * time.Second),
					Kind:          []otlp.Kind{otlp.KindOther, otlp.KindLLM, otlp.KindLLM, otlp.KindAgent, otlp.KindTool}[r.IntN(5)],
					Operation:     pick("chat", "embeddings"),
					Provider:      pick("openai", "anthropic"),
					Model:         pick("gpt-4.1-mini", "gpt-4.1", "claude-sonnet-4-5", "unknown"),
					Region:        pick("us-east-1", "unknown"),
					InputTokens:   int64(r.IntN(500)),
					OutputTokens:  int64(r.IntN(90)),
					Error:         r.IntN(6) == 0,
					PII:           r.IntN(2) == 0,
					Redacted:      r.IntN(2) == 0,
					Guardrail:     pick("", "allow", "block", "flag"),
					PromptName:    pick("", "triage", "summary"),
					PromptVersion: pick("", "", "14", "15"),
					Slot:          pick("", "claims", "refunds"),
					HighImpact:    r.IntN(3) == 0,
					Approved:      r.IntN(2) == 0,
					Escalation:    r.IntN(9) == 0,
				}
				if r.IntN(4) == 0 {
					s.CanaryKind = fmt.Sprintf("kind_%02d", r.IntN(24)) // past the 16-kind cap
					s.CanaryCaught = r.IntN(2) == 0
				}
				spans[i] = s
			}
			got.Add(spans, now)
			want.Add(spans, now)
			var g, w []Window
			switch r.IntN(6) {
			case 0:
				g, w = got.FlushOpen(), want.FlushOpen()
			case 1, 2:
				now = now.Add(time.Duration(r.IntN(50)) * time.Minute)
				g, w = got.Flush(now), want.Flush(now)
			}
			same(t, seed, step, g, w)
		}
		same(t, seed, -1, got.FlushAll(), want.FlushAll())
	}
}

// same compares everything but transitions, whose meaning changed on purpose
// (D-074) and which history_test.go tests.
func same(t *testing.T, seed uint64, step int, got, want []Window) {
	t.Helper()
	for _, ws := range [][]Window{got, want} {
		for i := range ws {
			ws[i].Metrics.Transitions = nil
		}
	}
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		i := 0
		for i < len(g) && i < len(w) && g[i] == w[i] {
			i++
		}
		from := max(0, i-300)
		t.Fatalf("seed %d step %d: windows differ at byte %d\n got: …%s\nwant: …%s", seed, step, i, g[from:min(len(g), i+200)], w[from:min(len(w), i+200)])
	}
}
