// Package aggregate turns normalised spans into hourly windows in the batch
// metrics shape (docs/architecture.md §3): UTC hours, closed a grace period
// after the hour; a span that arrives after its window closed goes into the
// current window and is counted in late_spans. Its results match the
// concierge's ingest (concierge/ingest.py) on the same input.
package aggregate

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"math"
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

// open is an hour still receiving spans. Spans are added up as they arrive
// (memory per span: a trace key the first time a trace is seen, and a small
// record per model call for the change log), never kept: the agent's memory
// does not grow with the traffic it summarises.
type open struct {
	m                   batch.Metrics // counters; the lists are assembled in build
	llmCalls, llmErrors int64
	evals               []batch.Eval
	tokens              map[tokenKey]*batch.TokenCount
	models              map[batch.ObservedModel]struct{}
	versions            map[batch.ObservedVersion]struct{}
	canaries            map[string]*canaryAcc
	traceIdx            map[traceKey]uint32
	traces              []traceEntry
	uses                map[useKey]*use // per slot, each model (or prompt version) used: first and last call
	strIdx              map[string]uint32
	strs                []string // interned slots, models, prompt names and versions; strs[0] = ""
	seq                 uint32   // arrival order: the tie-break of a stable sort
}

type tokenKey struct{ provider, model, region string }

// traceKey holds a 32-hex-digit trace ID (OTLP) as its 16 bytes, any other ID
// (Langfuse) as itself. Byte order of the 16 bytes is the order of their
// lower-case hex, so traces sort exactly as their IDs do.
type traceKey struct {
	b [16]byte
	s string
}

func keyOf(id string) traceKey {
	var k traceKey
	if len(id) == 32 && isLowerHex(id) {
		hex.Decode(k.b[:], []byte(id)) //nolint:errcheck // checked by isLowerHex
		return k
	}
	k.s = id
	return k
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (k traceKey) String() string {
	if k.s != "" {
		return k.s
	}
	return hex.EncodeToString(k.b[:])
}

func compareKeys(x, y traceKey) int {
	if x.s == "" && y.s == "" {
		return bytes.Compare(x.b[:], y.b[:])
	}
	return cmp.Compare(x.String(), y.String())
}

// pos is where a span sorts in its window: by start, then trace ID, then
// arrival (the order the stable sort of every span used to give).
type pos struct {
	start int64 // Unix nanoseconds
	trace uint32
	seq   uint32
}

const noTrace = math.MaxUint32 // a span without a trace ID: sorts as ""

type traceEntry struct {
	key   traceKey
	first int64 // start of its earliest span in this window
}

type canaryAcc struct {
	first       pos
	run, caught int64
}

// useKey is a model in a slot (kind "model": the agent or service, and the
// operation) or a version of a prompt (kind "prompt": the prompt name).
type useKey struct {
	kind        string
	slot, value uint32 // interned
}

type use struct{ first, last pos }

func newOpen() *open {
	return &open{tokens: map[tokenKey]*batch.TokenCount{}, models: map[batch.ObservedModel]struct{}{},
		versions: map[batch.ObservedVersion]struct{}{}, canaries: map[string]*canaryAcc{},
		traceIdx: map[traceKey]uint32{}, uses: map[useKey]*use{}, strIdx: map[string]uint32{"": 0}, strs: []string{""}}
}

func (w *open) intern(s string) uint32 {
	if i, ok := w.strIdx[s]; ok {
		return i
	}
	i := uint32(len(w.strs))
	w.strs = append(w.strs, s)
	w.strIdx[s] = i
	return i
}

func (w *open) compare(x, y pos) int {
	if c := cmp.Compare(x.start, y.start); c != 0 {
		return c
	}
	if x.trace != y.trace {
		switch {
		case x.trace == noTrace:
			return -1
		case y.trace == noTrace:
			return 1
		}
		if c := compareKeys(w.traces[x.trace].key, w.traces[y.trace].key); c != 0 {
			return c
		}
	}
	return cmp.Compare(x.seq, y.seq)
}

// add counts one span.
func (w *open) add(s otlp.Span) {
	p := pos{start: s.Start.UnixNano(), trace: noTrace, seq: w.seq}
	w.seq++
	if s.TraceID != "" {
		k := keyOf(s.TraceID)
		i, ok := w.traceIdx[k]
		if !ok {
			i = uint32(len(w.traces))
			w.traceIdx[k] = i
			w.traces = append(w.traces, traceEntry{key: k, first: p.start})
		} else if p.start < w.traces[i].first {
			w.traces[i].first = p.start
		}
		p.trace = i
	}
	m := &w.m
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
		c := w.canaries[s.CanaryKind]
		if c == nil {
			c = &canaryAcc{first: p}
			w.canaries[s.CanaryKind] = c
		} else if w.compare(p, c.first) < 0 { // arrived later, sorts earlier
			c.first = p
		}
		c.run++
		if s.CanaryCaught {
			c.caught++
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
		w.llmCalls++
		if s.Error {
			w.llmErrors++
		}
		k := tokenKey{s.Provider, s.Model, s.Region}
		tc := w.tokens[k]
		if tc == nil {
			tc = &batch.TokenCount{Provider: s.Provider, Model: s.Model, Region: s.Region}
			w.tokens[k] = tc
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
		if s.Model != "unknown" {
			slot := s.Slot
			if slot == "" {
				slot = "default"
			}
			w.used(useKey{"model", w.intern(slot + "\x00" + s.Operation), w.intern(s.Model)}, p)
		}
		if s.PromptVersion != "" {
			name := s.PromptName
			if name == "" {
				name = "default"
			}
			w.used(useKey{"prompt", w.intern(name), w.intern(s.PromptVersion)}, p)
		}
		if s.Model != "unknown" {
			w.models[batch.ObservedModel{Provider: s.Provider, Model: s.Model}] = struct{}{}
			if s.PromptVersion != "" {
				w.versions[batch.ObservedVersion{Model: s.Model, PromptVersion: s.PromptVersion}] = struct{}{}
			}
		}
	}
}

func (w *open) used(k useKey, p pos) {
	u := w.uses[k]
	switch {
	case u == nil:
		w.uses[k] = &use{first: p, last: p}
	case w.compare(p, u.first) < 0:
		u.first = p
	case w.compare(p, u.last) > 0:
		u.last = p
	}
}

// Aggregator is safe for concurrent use.
type Aggregator struct {
	cfg Config

	mu           sync.Mutex
	windows      map[time.Time]*open
	closedBefore time.Time          // every hour before this is closed
	traceFirst   map[traceKey]int64 // trace ID -> hour (Unix seconds) of its first span, for leaves
	history      History            // what each slot has in use, for changes (D-074)
}

// New returns an empty aggregator.
func New(cfg Config) *Aggregator {
	return &Aggregator{cfg: cfg, windows: map[time.Time]*open{}, traceFirst: map[traceKey]int64{}, history: NewHistory()}
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
			w = newOpen()
			a.windows[h] = w
		}
		w.add(s)
		if late {
			w.m.LateSpans++
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
			w = newOpen()
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
		if time.Unix(h, 0).Add(2 * time.Hour).Before(a.closedBefore) {
			delete(a.traceFirst, id)
		}
	}
	return out
}

func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func (a *Aggregator) build(h time.Time, w *open) Window {
	end := h.Add(time.Hour)
	m := w.m
	m.Tokens, m.Transitions, m.Anomalies = []batch.TokenCount{}, []batch.Transition{}, []batch.Anomaly{}
	m.Evals = slices.Clone(w.evals)
	if m.Evals == nil {
		m.Evals = []batch.Eval{}
	}
	slices.SortFunc(m.Evals, func(x, y batch.Eval) int {
		return cmp.Or(cmp.Compare(x.RanAt, y.RanAt), cmp.Compare(x.Suite, y.Suite), cmp.Compare(x.Model, y.Model), cmp.Compare(x.PromptVersion, y.PromptVersion))
	})

	// Leaves: each trace in the window where its first span is, ordered by that span.
	order := make([]uint32, len(w.traces))
	for i := range order {
		order[i] = uint32(i)
	}
	slices.SortFunc(order, func(x, y uint32) int {
		tx, ty := &w.traces[x], &w.traces[y]
		return cmp.Or(cmp.Compare(tx.first, ty.first), compareKeys(tx.key, ty.key))
	})
	var traces []string
	for _, i := range order {
		k := w.traces[i].key
		if _, seen := a.traceFirst[k]; !seen {
			a.traceFirst[k] = h.Unix()
			traces = append(traces, k.String())
		}
	}

	// Canary kinds in the order they first appear; past the cap, "other".
	kinds := make([]string, 0, len(w.canaries))
	for k := range w.canaries {
		kinds = append(kinds, k)
	}
	slices.SortFunc(kinds, func(x, y string) int { return w.compare(w.canaries[x].first, w.canaries[y].first) })
	canaries := map[string]*batch.Canary{}
	for _, k := range kinds {
		acc, kind := w.canaries[k], k
		if canaries[kind] == nil && len(canaries) >= batch.MaxCanaryKinds {
			kind = "other"
		}
		c := canaries[kind]
		if c == nil {
			c = &batch.Canary{Kind: kind}
			canaries[kind] = c
		}
		c.Run += acc.run
		c.Caught += acc.caught
	}
	for _, c := range canaries {
		m.Canaries = append(m.Canaries, *c)
	}
	slices.SortFunc(m.Canaries, func(x, y batch.Canary) int { return cmp.Compare(x.Kind, y.Kind) })

	m.Transitions = a.changes(w)

	m.Guardrails.LLMCalls = w.llmCalls
	for _, tc := range w.tokens {
		m.Tokens = append(m.Tokens, *tc)
	}
	slices.SortFunc(m.Tokens, func(x, y batch.TokenCount) int {
		return cmp.Or(cmp.Compare(x.Provider, y.Provider), cmp.Compare(x.Model, y.Model), cmp.Compare(x.Region, y.Region))
	})
	if w.llmCalls > 0 && w.llmCalls >= int64(a.cfg.MinCalls) {
		if float64(w.llmErrors)/float64(w.llmCalls) >= a.cfg.ErrorRate {
			m.Anomalies = append(m.Anomalies, batch.Anomaly{Start: iso(h), End: iso(end), Kind: "error_rate"})
		}
		if float64(m.Guardrails.Blocked)/float64(w.llmCalls) >= a.cfg.GuardrailBlock {
			m.Anomalies = append(m.Anomalies, batch.Anomaly{Start: iso(h), End: iso(end), Kind: "guardrail_rate"})
		}
	}
	obs := batch.Observed{Models: []batch.ObservedModel{}, Versions: []batch.ObservedVersion{}}
	for om := range w.models {
		obs.Models = append(obs.Models, om)
	}
	slices.SortFunc(obs.Models, func(x, y batch.ObservedModel) int {
		return cmp.Or(cmp.Compare(x.Provider, y.Provider), cmp.Compare(x.Model, y.Model))
	})
	for ov := range w.versions {
		obs.Versions = append(obs.Versions, ov)
	}
	slices.SortFunc(obs.Versions, func(x, y batch.ObservedVersion) int {
		return cmp.Or(cmp.Compare(x.Model, y.Model), cmp.Compare(x.PromptVersion, y.PromptVersion))
	})
	return Window{Window: batch.Window{Start: iso(h), End: iso(end)}, Start: h, Metrics: m, Observed: obs, TraceIDs: traces}
}
