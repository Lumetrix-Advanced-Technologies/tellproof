// Package demolog writes a synthetic local evidence log, signed and chained
// like a real one: a week of an imaginary claims assistant, with a model
// switch, a prompt change, an evaluation run each Monday and a quiet-hours
// deploy without the guardrail (when the injection and jailbreak canaries get
// through). It feeds `tellproof-agent demo`, the dashboard screenshots and
// tools/demolog. Nothing in it comes from a real system.
package demolog

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/aggregate"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/batchsign"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/commitment"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/enrol"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/evaluate"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/localreport"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/policy"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/queue"
)

// Result describes the log Write made.
type Result struct {
	AgentID  string
	Batches  int64
	From, To time.Time // the first window's start and the last window's end
	// ModelSwitch and PromptChange are when the log shows the Bedrock model
	// and the triage prompt changing.
	ModelSwitch, PromptChange time.Time
}

// Timezone is the imaginary system's: its business days are Sydney's.
const Timezone = "Australia/Sydney"

// Policy is a policy for the demo log, as its team might have written it:
// every model and region declared, the model switch recorded and the prompt
// change not, the evaluation run before the log began, and no incident for
// the night deploy. So most objectives pass, and the change register, the
// guardrail coverage, the incident register and the canaries show what the
// checks catch.
func Policy(r *Result) *policy.Policy {
	p := &policy.Policy{Schema: policy.SchemaURL, Organisation: "Example Insurance (synthetic)", AISystem: "Claims assistant (demo)",
		Environment: "production", Timezone: Timezone,
		Description: "Synthetic data from tellproof-agent demo; nothing here comes from a real system.",
		Changes:     []evaluate.Change{}, Incidents: []evaluate.Incident{},
		Evals: []batch.Eval{{Suite: "claims-triage-regression", Model: "gpt-4.1-mini", PromptVersion: "13",
			RanAt: r.From.Add(-26 * time.Hour).Format(time.RFC3339), PassRate: 0.95}}}
	seen := map[string]bool{}
	for _, md := range models {
		for _, name := range []string{md.name, strings.Replace(md.name, "sonnet-4-5", "sonnet-4-6", 1)} {
			if !seen[md.provider+"/"+name] {
				seen[md.provider+"/"+name] = true
				p.DeclaredModels = append(p.DeclaredModels, evaluate.DeclaredModel{Provider: md.provider, Model: name})
			}
		}
		if !seen[md.region] {
			seen[md.region] = true
			p.DeclaredRegions = append(p.DeclaredRegions, md.region)
		}
	}
	loc, _ := time.LoadLocation(Timezone)
	if loc == nil {
		loc = time.UTC
	}
	if !r.ModelSwitch.IsZero() {
		p.Changes = append(p.Changes, evaluate.Change{Date: r.ModelSwitch.In(loc).Format("2006-01-02"), Kind: "model",
			What: "Bedrock claims model from anthropic.claude-sonnet-4-5 to anthropic.claude-sonnet-4-6, approved in CAB-118"})
	}
	return p
}

type model struct {
	provider, name, region string
	share                  float64
	in, out                int64
}

var models = []model{
	{"openai", "gpt-4.1-mini", "us-east-1", 0.42, 900, 180},
	{"aws.bedrock", "anthropic.claude-sonnet-4-5", "ap-southeast-2", 0.30, 1400, 320},
	{"anthropic", "claude-haiku-4-5", "us", 0.14, 600, 90},
	{"acme-ai", "acme-7b", "eu-west-1", 0.09, 700, 140},
	{"openai", "text-embedding-3-small", "us-east-1", 0.05, 300, 0},
}

// Write fills dir (new, or empty) with days of hourly windows, the last one
// ending at end's hour, signed and chained by a fresh local identity. The figures
// come from seed, so the same seed and end give the same log.
func Write(dir string, days int, seed uint64, end time.Time) (*Result, error) {
	rng := rand.New(rand.NewPCG(seed, 1))
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("demolog: %s is not empty; choose a new directory", dir)
	}
	id, err := enrol.LocalIdentity(dir)
	if err != nil {
		return nil, err
	}
	b, err := batchsign.NewBuilder(id, "v2")
	if err != nil {
		return nil, err
	}
	q, err := queue.Open(filepath.Join(dir, localreport.Dir))
	if err != nil {
		return nil, err
	}
	end = end.UTC().Truncate(time.Hour)
	start := end.Add(-time.Duration(days) * 24 * time.Hour)
	switchAt := start.Add(time.Duration(days*24*2/3) * time.Hour)
	deployed := false // the one night deploy without the guardrail, half way through
	for h := start; h.Before(end); h = h.Add(time.Hour) {
		local := h.Add(10 * time.Hour) // a business day in Sydney
		day := 0.25 + 0.75*math.Max(0, math.Sin(math.Pi*float64(local.Hour()-7)/12))
		if wd := local.Weekday(); wd == time.Saturday || wd == time.Sunday {
			day *= 0.35
		}
		calls := int64(40 + 260*day*(0.85+0.3*rng.Float64()))
		var m batch.Metrics
		var obs batch.Observed
		for _, md := range models {
			name := md.name
			if md.name == "anthropic.claude-sonnet-4-5" && !h.Before(switchAt) {
				name = "anthropic.claude-sonnet-4-6"
			}
			n := int64(float64(calls) * md.share * (0.8 + 0.4*rng.Float64()))
			if n == 0 {
				continue
			}
			m.Tokens = append(m.Tokens, batch.TokenCount{Provider: md.provider, Model: name, Region: md.region, Calls: n,
				InputTokens: n * md.in * int64(80+rng.IntN(40)) / 100, OutputTokens: n * md.out * int64(80+rng.IntN(40)) / 100})
			m.Guardrails.LLMCalls += n
			obs.Models = append(obs.Models, batch.ObservedModel{Provider: md.provider, Model: name})
		}
		cover := 0.996 + 0.004*rng.Float64()
		if h.Hour() == 3 && !deployed && !h.Before(start.Add(time.Duration(days*12)*time.Hour)) {
			cover, deployed = 0.6+0.2*rng.Float64(), true
		}
		m.Guardrails.GuardedCalls = int64(float64(m.Guardrails.LLMCalls) * cover)
		m.Guardrails.Blocked = m.Guardrails.GuardedCalls * int64(1+rng.IntN(3)) / 100
		m.PII.Spans = m.Guardrails.LLMCalls / 5
		m.PII.SpansRedacted = m.PII.Spans
		m.Agent = batch.AgentCounts{Runs: m.Guardrails.LLMCalls / 4, ToolCalls: m.Guardrails.LLMCalls / 3, ToolErrors: m.Guardrails.LLMCalls / 3 * int64(rng.IntN(2)) / 150,
			HighImpactCalls: m.Guardrails.LLMCalls / 40, Escalations: int64(rng.IntN(2))}
		m.Agent.HighImpactApproved = m.Agent.HighImpactCalls
		// Hourly guardrail canaries (D-057); during the deploy without the
		// guardrail the injection and jailbreak probes get through.
		for _, kind := range []string{"jailbreak", "pii", "prompt_injection", "secret"} {
			c := batch.Canary{Kind: kind, Run: 1, Caught: 1}
			if cover < 0.85 && (kind == "jailbreak" || kind == "prompt_injection") {
				c.Caught = 0
			}
			m.Canaries = append(m.Canaries, c)
		}
		if cover < 0.85 {
			m.Anomalies = append(m.Anomalies, batch.Anomaly{Start: h.Format(time.RFC3339), End: h.Add(time.Hour).Format(time.RFC3339), Kind: "guardrail_rate"})
		}
		if h.Equal(switchAt) {
			m.Transitions = append(m.Transitions, batch.Transition{At: h.Add(17 * time.Minute).Format(time.RFC3339), Kind: "model", From: "anthropic.claude-sonnet-4-5", To: "anthropic.claude-sonnet-4-6"})
		}
		if h.Equal(start.Add(30 * time.Hour)) {
			m.Transitions = append(m.Transitions, batch.Transition{At: h.Add(41 * time.Minute).Format(time.RFC3339), Kind: "prompt", From: "13", To: "14"})
		}
		// The triage suite runs before each prompt goes live (prompt 13's run,
		// before the log starts, is in the policy) and every Monday.
		if h.Equal(start.Add(30 * time.Hour)) {
			m.Evals = append(m.Evals, batch.Eval{Suite: "claims-triage-regression", Model: "gpt-4.1-mini", PromptVersion: "14", RanAt: h.Add(20 * time.Minute).Format(time.RFC3339), PassRate: 0.96})
		}
		if local.Hour() == 9 && local.Weekday() == time.Monday {
			m.Evals = append(m.Evals, batch.Eval{Suite: "claims-triage-regression", Model: "gpt-4.1-mini", PromptVersion: "14", RanAt: h.Add(5 * time.Minute).Format(time.RFC3339), PassRate: 0.94 + 0.04*rng.Float64()})
		}
		prompt := "13"
		if h.After(start.Add(30 * time.Hour)) {
			prompt = "14"
		}
		obs.Versions = []batch.ObservedVersion{{Model: "gpt-4.1-mini", PromptVersion: prompt}}
		traces := make([]string, int(m.Guardrails.LLMCalls/3))
		for i := range traces {
			traces[i] = fmt.Sprintf("%032x", rng.Uint64())
		}
		w := aggregate.Window{Window: batch.Window{Start: h.Format(time.RFC3339), End: h.Add(time.Hour).Format(time.RFC3339)}, Start: h, Metrics: m, Observed: obs, TraceIDs: traces}
		if w.Metrics.Evals == nil {
			w.Metrics.Evals = []batch.Eval{}
		}
		if w.Metrics.Transitions == nil {
			w.Metrics.Transitions = []batch.Transition{}
		}
		if w.Metrics.Anomalies == nil {
			w.Metrics.Anomalies = []batch.Anomaly{}
		}
		env, _, next, err := b.Build(context.Background(), q.State(), w, commitment.ForWindow([]byte("demo-secret"), traces).Commit, nil)
		if err != nil {
			return nil, err
		}
		if err := q.Enqueue(env, next); err != nil {
			return nil, err
		}
	}
	return &Result{AgentID: id.AgentID, Batches: q.State().Seq, From: start, To: end,
		ModelSwitch: switchAt.Add(17 * time.Minute), PromptChange: start.Add(30*time.Hour + 41*time.Minute)}, nil
}
