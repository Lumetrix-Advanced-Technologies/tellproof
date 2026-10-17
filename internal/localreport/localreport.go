// Package localreport reads an agent's local evidence log (D-052): it checks
// every batch's signature against the agent's public key and the chain from
// the first batch to the last, then sums the batches into one summary a
// person can read. It needs only the data directory (or a copy of it), never
// a server.
package localreport

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/batchsign"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/enrol"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/queue"
)

// Dir is the local evidence log inside the data directory: signed, chained
// batches in the agent queue's format, never sent.
const Dir = "local"

// Model is one provider, model and region's traffic.
type Model struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Region       string `json:"region"`
	Calls        int64  `json:"calls"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

// Summary is the local evidence log, verified and summed.
type Summary struct {
	AgentID string `json:"agent_id"`
	// Verified is true when every batch's signature holds and the chain is
	// unbroken from seq 1; otherwise Problem says where it failed and the
	// sums cover only the batches before it.
	Verified bool   `json:"verified"`
	Problem  string `json:"problem,omitempty"`
	Batches  int    `json:"batches"`
	LastSeq  int64  `json:"last_seq"`
	LastHash string `json:"last_digest,omitempty"`
	From     string `json:"from,omitempty"` // first window start in range
	To       string `json:"to,omitempty"`   // last window end in range

	Models         []Model                 `json:"models"`
	LLMCalls       int64                   `json:"llm_calls"`
	GuardedCalls   int64                   `json:"guarded_calls"`
	Blocked        int64                   `json:"blocked_calls"`
	PIISpans       int64                   `json:"pii_spans"`
	PIIRedacted    int64                   `json:"pii_spans_redacted"`
	ContentDropped int64                   `json:"content_present_spans"`
	Agent          batch.AgentCounts       `json:"agent"`
	Evals          []batch.Eval            `json:"evals"`
	Transitions    []batch.Transition      `json:"transitions"`
	Anomalies      []batch.Anomaly         `json:"anomalies"`
	Versions       []batch.ObservedVersion `json:"prompt_versions"`
	LateSpans      int64                   `json:"late_spans"`
	Traces         int64                   `json:"traces_committed"`
	Usage          []batch.Usage           `json:"provider_usage"`
	Canaries       []batch.Canary          `json:"canaries"` // summed by kind (D-057)

	// Series is one point per window in range, oldest first, with the
	// additive batches of a window summed (for the dashboard's charts).
	Series []Point `json:"series"`
	// Recent are the newest batches of the whole log, newest first.
	Recent []BatchRow `json:"recent_batches"`
}

// Point is one window's figures.
type Point struct {
	Start          string  `json:"start"`
	Models         []Model `json:"models"`
	LLMCalls       int64   `json:"llm_calls"`
	GuardedCalls   int64   `json:"guarded_calls"`
	Blocked        int64   `json:"blocked_calls"`
	PIISpans       int64   `json:"pii_spans"`
	PIIRedacted    int64   `json:"pii_spans_redacted"`
	ContentDropped int64   `json:"content_present_spans"`
	ToolCalls      int64   `json:"tool_calls"`
	ToolErrors     int64   `json:"tool_errors"`
	Traces         int64   `json:"traces_committed"`
	LateSpans      int64   `json:"late_spans"`
	CanariesRun    int64   `json:"canaries_run"`
	CanariesCaught int64   `json:"canaries_caught"`
}

// BatchRow is one batch of the chain.
type BatchRow struct {
	Seq         int64  `json:"seq"`
	WindowStart string `json:"window_start"`
	Digest      string `json:"digest"`
	Traces      int64  `json:"traces"`
	LLMCalls    int64  `json:"llm_calls"`
}

// recentMax is how many batches Recent keeps.
const recentMax = 48

// Range limits the windows summed; zero values mean no limit.
type Range struct{ From, To time.Time }

func (r Range) has(start string) bool {
	t, err := time.Parse(time.RFC3339, start)
	if err != nil {
		return false
	}
	return (r.From.IsZero() || !t.Before(r.From)) && (r.To.IsZero() || t.Before(r.To))
}

// Read verifies and sums the local evidence log in dataDir. The whole chain
// is always checked; r only limits which windows are summed.
func Read(dataDir string, r Range) (*Summary, error) {
	s, _, err := ReadBatches(dataDir, r)
	return s, err
}

// ReadBatches is Read, also returning the batches that verified, in order
// (all of them when the summary is Verified; those before the first problem
// otherwise), for the evaluator and the AI-BOM.
func ReadBatches(dataDir string, r Range) (*Summary, []batch.Batch, error) {
	agentID, pub, err := enrol.LoadPublic(dataDir)
	if err != nil {
		return nil, nil, fmt.Errorf("localreport: %w (is %s a local-mode data directory?)", err, dataDir)
	}
	q, err := queue.Open(filepath.Join(dataDir, Dir))
	if err != nil {
		return nil, nil, err
	}
	seqs, err := q.Pending()
	if err != nil {
		return nil, nil, err
	}
	var verified []batch.Batch
	s := &Summary{AgentID: agentID, Verified: true, Models: []Model{}, Evals: []batch.Eval{}, Transitions: []batch.Transition{},
		Anomalies: []batch.Anomaly{}, Versions: []batch.ObservedVersion{}, Usage: []batch.Usage{}, Series: []Point{}, Recent: []BatchRow{}}
	acc := newAccumulator()
	var prev *string
	for i, seq := range seqs {
		bt, digest, err := verifyOne(q, seq, pub, agentID)
		switch {
		case err != nil:
			s.fail(fmt.Sprintf("batch %d: %v", seq, err))
		case seq != int64(i)+1:
			s.fail(fmt.Sprintf("batch %d is missing", int64(i)+1))
		case !samePrev(bt.Prev, prev):
			s.fail(fmt.Sprintf("batch %d does not follow batch %d (its prev is not that batch's digest)", seq, seq-1))
		}
		if !s.Verified {
			break
		}
		prev = &digest
		verified = append(verified, *bt)
		s.Batches++
		s.LastSeq, s.LastHash = seq, digest
		s.Recent = append(s.Recent, BatchRow{Seq: seq, WindowStart: bt.Window.Start, Digest: digest, Traces: bt.Commit.Leaves, LLMCalls: bt.Metrics.Guardrails.LLMCalls})
		if len(s.Recent) > recentMax {
			s.Recent = s.Recent[1:]
		}
		if r.has(bt.Window.Start) {
			acc.add(s, bt)
		}
	}
	// The log records how far it has written; batches missing from the end
	// (or all of them) are a failure too, not an empty log.
	if st := q.State(); s.Verified && st.Seq > s.LastSeq {
		s.fail(fmt.Sprintf("batch %d is missing (the log has written up to batch %d)", s.LastSeq+1, st.Seq))
	}
	acc.finish(s)
	return s, verified, nil
}

func (s *Summary) fail(problem string) {
	s.Verified, s.Problem = false, problem
}

func samePrev(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func verifyOne(q *queue.Queue, seq int64, pub ed25519.PublicKey, agentID string) (*batch.Batch, string, error) {
	env, err := q.Envelope(seq)
	if err != nil {
		return nil, "", err
	}
	bt, err := batchsign.Verify(env, pub, agentID)
	if err != nil {
		return nil, "", fmt.Errorf("signature or format: %w", err)
	}
	if bt.Seq != seq {
		return nil, "", fmt.Errorf("it says it is batch %d", bt.Seq)
	}
	return bt, batchsign.Digest(env), nil
}

type accumulator struct {
	points   map[string]*Point
	models   map[[3]string]*Model
	usage    map[[3]string]*batch.Usage
	evals    map[batch.Eval]bool
	versions map[batch.ObservedVersion]bool
	canaries map[string]*batch.Canary
}

func newAccumulator() *accumulator {
	return &accumulator{points: map[string]*Point{}, models: map[[3]string]*Model{}, usage: map[[3]string]*batch.Usage{}, evals: map[batch.Eval]bool{}, versions: map[batch.ObservedVersion]bool{}, canaries: map[string]*batch.Canary{}}
}

// add sums one batch. Windows are additive (D-032): an hour sent early and
// then completed is two batches whose counts add up to the hour.
func (a *accumulator) add(s *Summary, b *batch.Batch) {
	if s.From == "" || b.Window.Start < s.From {
		s.From = b.Window.Start
	}
	if b.Window.End > s.To {
		s.To = b.Window.End
	}
	m := b.Metrics
	p := a.points[b.Window.Start]
	if p == nil {
		p = &Point{Start: b.Window.Start, Models: []Model{}}
		a.points[b.Window.Start] = p
	}
	p.LLMCalls += m.Guardrails.LLMCalls
	p.GuardedCalls += m.Guardrails.GuardedCalls
	p.Blocked += m.Guardrails.Blocked
	p.PIISpans += m.PII.Spans
	p.PIIRedacted += m.PII.SpansRedacted
	p.ContentDropped += m.ContentPresentSpans
	p.ToolCalls += m.Agent.ToolCalls
	p.ToolErrors += m.Agent.ToolErrors
	p.Traces += b.Commit.Leaves
	for _, c := range m.Canaries {
		p.CanariesRun += c.Run
		p.CanariesCaught += c.Caught
		if a.canaries[c.Kind] == nil {
			a.canaries[c.Kind] = &batch.Canary{Kind: c.Kind}
		}
		a.canaries[c.Kind].Run += c.Run
		a.canaries[c.Kind].Caught += c.Caught
	}
	p.LateSpans += m.LateSpans
	for _, t := range m.Tokens {
		found := false
		for i := range p.Models {
			if pm := &p.Models[i]; pm.Provider == t.Provider && pm.Model == t.Model && pm.Region == t.Region {
				pm.Calls, pm.InputTokens, pm.OutputTokens = pm.Calls+t.Calls, pm.InputTokens+t.InputTokens, pm.OutputTokens+t.OutputTokens
				found = true
			}
		}
		if !found {
			p.Models = append(p.Models, Model{Provider: t.Provider, Model: t.Model, Region: t.Region, Calls: t.Calls, InputTokens: t.InputTokens, OutputTokens: t.OutputTokens})
		}
	}
	for _, t := range m.Tokens {
		k := [3]string{t.Provider, t.Model, t.Region}
		if a.models[k] == nil {
			a.models[k] = &Model{Provider: t.Provider, Model: t.Model, Region: t.Region}
		}
		a.models[k].Calls += t.Calls
		a.models[k].InputTokens += t.InputTokens
		a.models[k].OutputTokens += t.OutputTokens
	}
	s.LLMCalls += m.Guardrails.LLMCalls
	s.GuardedCalls += m.Guardrails.GuardedCalls
	s.Blocked += m.Guardrails.Blocked
	s.PIISpans += m.PII.Spans
	s.PIIRedacted += m.PII.SpansRedacted
	s.ContentDropped += m.ContentPresentSpans
	s.Agent.Runs += m.Agent.Runs
	s.Agent.ToolCalls += m.Agent.ToolCalls
	s.Agent.ToolErrors += m.Agent.ToolErrors
	s.Agent.HighImpactCalls += m.Agent.HighImpactCalls
	s.Agent.HighImpactApproved += m.Agent.HighImpactApproved
	s.Agent.Escalations += m.Agent.Escalations
	s.LateSpans += m.LateSpans
	s.Traces += b.Commit.Leaves
	for _, e := range m.Evals {
		a.evals[e] = true
	}
	s.Transitions = append(s.Transitions, m.Transitions...)
	s.Anomalies = append(s.Anomalies, m.Anomalies...)
	for _, v := range b.Observed.Versions {
		a.versions[v] = true
	}
	for _, u := range b.Usage {
		k := [3]string{u.Provider, u.Model, u.Region}
		if a.usage[k] == nil {
			a.usage[k] = &batch.Usage{Provider: u.Provider, Model: u.Model, Region: u.Region}
		}
		a.usage[k].InputTokens += u.InputTokens
		a.usage[k].OutputTokens += u.OutputTokens
		if u.Requests != nil {
			n := *u.Requests
			if a.usage[k].Requests != nil {
				n += *a.usage[k].Requests
			}
			a.usage[k].Requests = &n
		}
	}
}

func (a *accumulator) finish(s *Summary) {
	for _, p := range a.points {
		s.Series = append(s.Series, *p)
	}
	sort.Slice(s.Series, func(i, j int) bool { return s.Series[i].Start < s.Series[j].Start })
	for i, j := 0, len(s.Recent)-1; i < j; i, j = i+1, j-1 {
		s.Recent[i], s.Recent[j] = s.Recent[j], s.Recent[i]
	}
	for _, m := range a.models {
		s.Models = append(s.Models, *m)
	}
	sort.Slice(s.Models, func(i, j int) bool {
		if s.Models[i].Calls != s.Models[j].Calls {
			return s.Models[i].Calls > s.Models[j].Calls
		}
		return s.Models[i].Provider+s.Models[i].Model+s.Models[i].Region < s.Models[j].Provider+s.Models[j].Model+s.Models[j].Region
	})
	for e := range a.evals {
		s.Evals = append(s.Evals, e)
	}
	s.Canaries = []batch.Canary{}
	for _, c := range a.canaries {
		s.Canaries = append(s.Canaries, *c)
	}
	sort.Slice(s.Canaries, func(i, j int) bool { return s.Canaries[i].Kind < s.Canaries[j].Kind })
	sort.Slice(s.Evals, func(i, j int) bool { return s.Evals[i].RanAt < s.Evals[j].RanAt })
	for v := range a.versions {
		s.Versions = append(s.Versions, v)
	}
	sort.Slice(s.Versions, func(i, j int) bool {
		return s.Versions[i].Model+s.Versions[i].PromptVersion < s.Versions[j].Model+s.Versions[j].PromptVersion
	})
	for _, u := range a.usage {
		s.Usage = append(s.Usage, *u)
	}
	sort.Slice(s.Usage, func(i, j int) bool {
		return s.Usage[i].Provider+s.Usage[i].Model < s.Usage[j].Provider+s.Usage[j].Model
	})
	sort.Slice(s.Transitions, func(i, j int) bool { return s.Transitions[i].At < s.Transitions[j].At })
	sort.Slice(s.Anomalies, func(i, j int) bool { return s.Anomalies[i].Start < s.Anomalies[j].Start })
}

// EarlyAccess is where the report points people for independently signed packs.
const EarlyAccess = "https://github.com/lumetrix-advanced-technologies/tellproof/issues/new?template=early-access.yml"

func pct(n, d int64) string {
	if d == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", float64(n)*100/float64(d))
}

// WriteJSON writes the summary as JSON.
func WriteJSON(w io.Writer, s *Summary) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// WriteText writes the summary for a person.
func WriteText(w io.Writer, s *Summary) error {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	p("Tellproof local evidence log, agent %s", s.AgentID)
	if s.Verified {
		p("  Verified: %d batches, signed by this agent's key and chained from batch 1 to %d.", s.Batches, s.LastSeq)
		if s.LastHash != "" {
			p("  Latest batch digest: %s", s.LastHash)
		}
	} else {
		p("  FAILED: %s. The figures below cover only the %d batches before it.", s.Problem, s.Batches)
	}
	if s.From == "" {
		p("")
		p("No windows in range yet. Batches are written hourly, and once a minute after the first spans of a run.")
	} else {
		p("  Windows: %s to %s (UTC)", s.From, s.To)
		p("")
		p("Model calls")
		tw := tabwriter.NewWriter(&b, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  provider\tmodel\tregion\tcalls\tinput tokens\toutput tokens")
		for _, m := range s.Models {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%d\t%d\n", m.Provider, m.Model, m.Region, m.Calls, m.InputTokens, m.OutputTokens)
		}
		tw.Flush()
		p("")
		p("Guardrails      %d of %d model calls had a guardrail decision (%s); %d blocked", s.GuardedCalls, s.LLMCalls, pct(s.GuardedCalls, s.LLMCalls), s.Blocked)
		p("Personal data   %d of %d spans with personal data were redacted (%s)", s.PIIRedacted, s.PIISpans, pct(s.PIIRedacted, s.PIISpans))
		p("Canaries        %s", canaryLine(s.Canaries))
		p("Content         %d spans arrived with message content; the agent dropped it", s.ContentDropped)
		p("Agents          %d runs, %d tool calls (%d failed), %d high-impact calls (%d approved), %d escalations",
			s.Agent.Runs, s.Agent.ToolCalls, s.Agent.ToolErrors, s.Agent.HighImpactCalls, s.Agent.HighImpactApproved, s.Agent.Escalations)
		p("Traces          %d committed (salted Merkle roots; disclose one later from this data directory)", s.Traces)
		if s.LateSpans > 0 {
			p("Late spans      %d arrived after their hour closed and were counted in the next", s.LateSpans)
		}
		if len(s.Versions) > 0 {
			var vs []string
			for _, v := range s.Versions {
				vs = append(vs, v.Model+" @ prompt "+v.PromptVersion)
			}
			p("Prompt versions %s", strings.Join(vs, ", "))
		}
		for _, t := range s.Transitions {
			p("Change          %s: %s %s -> %s", t.At, t.Kind, t.From, t.To)
		}
		for _, a := range s.Anomalies {
			p("Anomaly         %s to %s: %s", a.Start, a.End, a.Kind)
		}
		for _, e := range s.Evals {
			p("Evaluation      %s on %s (prompt %s) at %s: pass rate %.3g", e.Suite, e.Model, e.PromptVersion, e.RanAt, e.PassRate)
		}
		for _, u := range s.Usage {
			p("Provider usage  %s %s %s: %d input, %d output tokens billed", u.Provider, u.Model, u.Region, u.InputTokens, u.OutputTokens)
		}
	}
	p("")
	p("This log is signed by your own agent's key: it shows the record has not changed since")
	p("it was written, relative to %s. For packs evaluated against Tellproof's evidence", enrol.PublicKeyFile)
	p("objectives and signed by an independent issuer, which a bank, insurer or auditor can")
	p("verify, request early access: %s", EarlyAccess)
	_, err := io.WriteString(w, b.String())
	return err
}

// canaryLine sums the guardrail canaries and names the kinds that were missed.
func canaryLine(cs []batch.Canary) string {
	var run, caught int64
	var missed []string
	for _, c := range cs {
		run += c.Run
		caught += c.Caught
		if c.Run > c.Caught {
			missed = append(missed, fmt.Sprintf("%s (%d)", c.Kind, c.Run-c.Caught))
		}
	}
	if run == 0 {
		return "none run: the SDK's schedule_canaries (scheduleCanaries) sends known-bad probes through your guardrail"
	}
	line := fmt.Sprintf("%d of %d known-bad probes caught by the guardrail (%s)", caught, run, pct(caught, run))
	if len(missed) > 0 {
		line += "; missed: " + strings.Join(missed, ", ")
	}
	return line
}
