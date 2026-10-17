// Package aggregate turns normalised spans into hourly windows in the batch
// metrics shape (docs/architecture.md §3): UTC hours, closed a grace period
// after the hour; a span that arrives after its window closed goes into the
// current window and is counted in late_spans. Its results match the
// concierge's ingest (concierge/ingest.py) on the same input.
package aggregate

import (
	"cmp"
	"slices"
	"sync"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/otlp"
)

// Config holds the window and anomaly settings (D-020 defaults).
type Config struct {
	Grace          time.Duration // how long after the hour a window stays open for late spans
	MinCalls       int           // an hour needs at least this many LLM calls to be judged
	ErrorRate      float64       // error_rate anomaly when LLM errors / calls reach this
	GuardrailBlock float64       // guardrail_rate anomaly when blocked / calls reach this
}

// DefaultConfig: 5-minute grace (§3); 20 calls, 5% errors, 3% blocks (D-020).
func DefaultConfig() Config {
	return Config{Grace: 5 * time.Minute, MinCalls: 20, ErrorRate: 0.05, GuardrailBlock: 0.03}
}

// Window is one closed hour: its metrics, what was live, and the trace IDs
// first seen in it (in order) for the salted commitment. Trace IDs never
// leave the agent; only the commitment does.
type Window struct {
	Window   batch.Window
	Start    time.Time
	Metrics  batch.Metrics
	Observed batch.Observed
	TraceIDs []string
}

type open struct {
	spans []otlp.Span
	evals []batch.Eval
	late  int64
}

// Aggregator is safe for concurrent use.
type Aggregator struct {
	cfg Config

	mu           sync.Mutex
	windows      map[time.Time]*open
	closedBefore time.Time            // every hour before this is closed
	traceFirst   map[string]time.Time // trace ID -> hour of its first span, for leaves
	lastModel    map[string]string    // slot -> model, for transitions
	lastPrompt   map[string]string    // prompt name -> version
}

// New returns an empty aggregator.
func New(cfg Config) *Aggregator {
	return &Aggregator{cfg: cfg, windows: map[time.Time]*open{}, traceFirst: map[string]time.Time{},
		lastModel: map[string]string{}, lastPrompt: map[string]string{}}
}

func hour(t time.Time) time.Time { return t.UTC().Truncate(time.Hour) }

// Add files spans into their hours. now decides lateness: a span whose hour is
// already closed goes into the hour that contains now.
func (a *Aggregator) Add(spans []otlp.Span, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range spans {
		h, late := hour(s.Start), false
		if h.Before(a.closedBefore) {
			h, late = hour(now), true
			if h.Before(a.closedBefore) { // a clock behind the closed windows: use the first open hour
				h = a.closedBefore
			}
		}
		w := a.windows[h]
		if w == nil {
			w = &open{}
			a.windows[h] = w
		}
		w.spans = append(w.spans, s)
		if late {
			w.late++
		}
	}
}

// AddEvals files eval runs (from the Langfuse puller or an SDK) into the hour
// of their ran_at time; a run for a closed hour goes into the hour containing
// now. A run with an unreadable time is dropped.
func (a *Aggregator) AddEvals(evals []batch.Eval, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range evals {
		t, err := time.Parse(time.RFC3339, e.RanAt)
		if err != nil {
			continue
		}
		h := hour(t)
		if h.Before(a.closedBefore) {
			h = hour(now)
			if h.Before(a.closedBefore) {
				h = a.closedBefore
			}
		}
		w := a.windows[h]
		if w == nil {
			w = &open{}
			a.windows[h] = w
		}
		w.evals = append(w.evals, e)
	}
}

// Flush closes, in order, every window whose hour ended at least Grace before now.
func (a *Aggregator) Flush(now time.Time) []Window {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.close(func(h time.Time) bool { return !h.Add(time.Hour + a.cfg.Grace).After(now) })
}

// FlushOpen emits every window now without closing its hour: spans that
// arrive later for the same hour start a new window for it, emitted when the
// hour closes. Windows are additive (D-032), so this gives a new agent its
// first batch within minutes instead of after the hour (S-5); a trace is still
// committed only once, in the batch where it first appears.
func (a *Aggregator) FlushOpen() []Window {
	a.mu.Lock()
	defer a.mu.Unlock()
	var hours []time.Time
	for h := range a.windows {
		hours = append(hours, h)
	}
	slices.SortFunc(hours, func(x, y time.Time) int { return x.Compare(y) })
	out := make([]Window, 0, len(hours))
	for _, h := range hours {
		out = append(out, a.build(h, a.windows[h]))
		delete(a.windows, h)
	}
	return out
}

// FlushAll closes every window (shutdown, or offline replay of an export).
func (a *Aggregator) FlushAll() []Window {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.close(func(time.Time) bool { return true })
}

func (a *Aggregator) close(ready func(time.Time) bool) []Window {
	var hours []time.Time
	for h := range a.windows {
		if ready(h) {
			hours = append(hours, h)
		}
	}
	slices.SortFunc(hours, func(x, y time.Time) int { return x.Compare(y) })
	out := make([]Window, 0, len(hours))
	for _, h := range hours {
		out = append(out, a.build(h, a.windows[h]))
		delete(a.windows, h)
		if next := h.Add(time.Hour); next.After(a.closedBefore) {
			a.closedBefore = next
		}
	}
	// Forget traces from hours that can no longer receive spans on time.
	for id, h := range a.traceFirst {
		if h.Add(2 * time.Hour).Before(a.closedBefore) {
			delete(a.traceFirst, id)
		}
	}
	return out
}

func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func (a *Aggregator) build(h time.Time, w *open) Window {
	spans := slices.Clone(w.spans)
	slices.SortStableFunc(spans, func(x, y otlp.Span) int {
		return cmp.Or(x.Start.Compare(y.Start), cmp.Compare(x.TraceID, y.TraceID))
	})
	end := h.Add(time.Hour)
	m := batch.Metrics{Tokens: []batch.TokenCount{}, Evals: slices.Clone(w.evals), Transitions: []batch.Transition{},
		Anomalies: []batch.Anomaly{}, LateSpans: w.late}
	if m.Evals == nil {
		m.Evals = []batch.Eval{}
	}
	slices.SortFunc(m.Evals, func(x, y batch.Eval) int {
		return cmp.Or(cmp.Compare(x.RanAt, y.RanAt), cmp.Compare(x.Suite, y.Suite), cmp.Compare(x.Model, y.Model), cmp.Compare(x.PromptVersion, y.PromptVersion))
	})

	type key struct{ provider, model, region string }
	tokens := map[key]*batch.TokenCount{}
	models := map[batch.ObservedModel]bool{}
	versions := map[batch.ObservedVersion]bool{}
	canaries := map[string]*batch.Canary{}
	var llmCalls, llmErrors int64
	var traces []string
	for _, s := range spans {
		if s.TraceID != "" {
			if _, seen := a.traceFirst[s.TraceID]; !seen {
				a.traceFirst[s.TraceID] = h
				traces = append(traces, s.TraceID)
			}
		}
		if s.ContentPresent {
			m.ContentPresentSpans++
		}
		if s.PII {
			m.PII.Spans++
			if s.Redacted {
				m.PII.SpansRedacted++
			}
		}
		if s.Escalation {
			m.Agent.Escalations++
		}
		if s.CanaryKind != "" {
			kind := s.CanaryKind
			if canaries[kind] == nil && len(canaries) >= batch.MaxCanaryKinds {
				kind = "other"
			}
			c := canaries[kind]
			if c == nil {
				c = &batch.Canary{Kind: kind}
				canaries[kind] = c
			}
			c.Run++
			if s.CanaryCaught {
				c.Caught++
			}
		}
		switch s.Kind {
		case otlp.KindAgent:
			m.Agent.Runs++
		case otlp.KindTool:
			m.Agent.ToolCalls++
			if s.Error {
				m.Agent.ToolErrors++
			}
			if s.HighImpact {
				m.Agent.HighImpactCalls++
				if s.Approved {
					m.Agent.HighImpactApproved++
				}
			}
		case otlp.KindLLM:
			llmCalls++
			if s.Error {
				llmErrors++
			}
			k := key{s.Provider, s.Model, s.Region}
			tc := tokens[k]
			if tc == nil {
				tc = &batch.TokenCount{Provider: s.Provider, Model: s.Model, Region: s.Region}
				tokens[k] = tc
			}
			tc.InputTokens += s.InputTokens
			tc.OutputTokens += s.OutputTokens
			tc.Calls++
			if s.Guardrail != "" {
				m.Guardrails.GuardedCalls++
				if s.Guardrail == "block" {
					m.Guardrails.Blocked++
				}
			}
			a.transitions(&m, s)
			if s.Model != "unknown" {
				models[batch.ObservedModel{Provider: s.Provider, Model: s.Model}] = true
				if s.PromptVersion != "" {
					versions[batch.ObservedVersion{Model: s.Model, PromptVersion: s.PromptVersion}] = true
				}
			}
		}
	}
	m.Guardrails.LLMCalls = llmCalls
	for _, c := range canaries {
		m.Canaries = append(m.Canaries, *c)
	}
	slices.SortFunc(m.Canaries, func(x, y batch.Canary) int { return cmp.Compare(x.Kind, y.Kind) })
	for _, tc := range tokens {
		m.Tokens = append(m.Tokens, *tc)
	}
	slices.SortFunc(m.Tokens, func(x, y batch.TokenCount) int {
		return cmp.Or(cmp.Compare(x.Provider, y.Provider), cmp.Compare(x.Model, y.Model), cmp.Compare(x.Region, y.Region))
	})
	if llmCalls > 0 && llmCalls >= int64(a.cfg.MinCalls) {
		if float64(llmErrors)/float64(llmCalls) >= a.cfg.ErrorRate {
			m.Anomalies = append(m.Anomalies, batch.Anomaly{Start: iso(h), End: iso(end), Kind: "error_rate"})
		}
		if float64(m.Guardrails.Blocked)/float64(llmCalls) >= a.cfg.GuardrailBlock {
			m.Anomalies = append(m.Anomalies, batch.Anomaly{Start: iso(h), End: iso(end), Kind: "guardrail_rate"})
		}
	}
	obs := batch.Observed{Models: []batch.ObservedModel{}, Versions: []batch.ObservedVersion{}}
	for om := range models {
		obs.Models = append(obs.Models, om)
	}
	slices.SortFunc(obs.Models, func(x, y batch.ObservedModel) int {
		return cmp.Or(cmp.Compare(x.Provider, y.Provider), cmp.Compare(x.Model, y.Model))
	})
	for ov := range versions {
		obs.Versions = append(obs.Versions, ov)
	}
	slices.SortFunc(obs.Versions, func(x, y batch.ObservedVersion) int {
		return cmp.Or(cmp.Compare(x.Model, y.Model), cmp.Compare(x.PromptVersion, y.PromptVersion))
	})
	return Window{Window: batch.Window{Start: iso(h), End: iso(end)}, Start: h, Metrics: m, Observed: obs, TraceIDs: traces}
}

// transitions records a model change within the span's slot and operation
// (so a chat model and an embeddings model used side by side are not read as
// changes, D-033) and a prompt version change within its prompt name.
func (a *Aggregator) transitions(m *batch.Metrics, s otlp.Span) {
	if s.Model != "unknown" {
		slot := s.Slot
		if slot == "" {
			slot = "default"
		}
		slot += "\x00" + s.Operation
		if prev, ok := a.lastModel[slot]; ok && prev != s.Model {
			m.Transitions = append(m.Transitions, batch.Transition{At: iso(s.Start), Kind: "model", From: prev, To: s.Model})
		}
		a.lastModel[slot] = s.Model
	}
	if s.PromptVersion != "" {
		name := s.PromptName
		if name == "" {
			name = "default"
		}
		if prev, ok := a.lastPrompt[name]; ok && prev != s.PromptVersion {
			m.Transitions = append(m.Transitions, batch.Transition{At: iso(s.Start), Kind: "prompt", From: prev, To: s.PromptVersion})
		}
		a.lastPrompt[name] = s.PromptVersion
	}
}
