// Command demolog writes a synthetic local evidence log (signed and chained
// like a real one) for developing and screenshotting the dashboard:
//
//	go run ./tools/demolog -out /tmp/demo -days 7
//	go run ./cmd/agent dashboard --data-dir /tmp/demo
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/aggregate"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/batch"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/batchsign"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/commitment"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/enrol"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/localreport"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/queue"
)

type model struct {
	provider, name, region string
	share                  float64
	in, out                int64
}

func main() {
	out := flag.String("out", "", "data directory to create")
	days := flag.Int("days", 7, "days of hourly windows, ending now")
	seed := flag.Uint64("seed", 7, "random seed")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "usage: demolog -out <dir> [-days 7]")
		os.Exit(2)
	}
	if err := write(*out, *days, rand.New(rand.NewPCG(*seed, 1))); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func write(dir string, days int, rng *rand.Rand) error {
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s exists; choose a new directory", dir)
	}
	id, err := enrol.LocalIdentity(dir)
	if err != nil {
		return err
	}
	b, err := batchsign.NewBuilder(id, "v2")
	if err != nil {
		return err
	}
	q, err := queue.Open(filepath.Join(dir, localreport.Dir))
	if err != nil {
		return err
	}
	models := []model{
		{"openai", "gpt-4.1-mini", "us-east-1", 0.42, 900, 180},
		{"aws.bedrock", "anthropic.claude-sonnet-4-5", "ap-southeast-2", 0.30, 1400, 320},
		{"anthropic", "claude-haiku-4-5", "us", 0.14, 600, 90},
		{"acme-ai", "acme-7b", "eu-west-1", 0.09, 700, 140},
		{"openai", "text-embedding-3-small", "us-east-1", 0.05, 300, 0},
	}
	end := time.Now().UTC().Truncate(time.Hour)
	start := end.Add(-time.Duration(days) * 24 * time.Hour)
	switchAt := start.Add(time.Duration(days*24*2/3) * time.Hour)
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
		cover := 0.93 + 0.06*rng.Float64()
		if h.Hour() == 3 && rng.IntN(3) == 0 {
			cover = 0.6 + 0.2*rng.Float64() // a quiet-hours deploy without the guardrail
		}
		m.Guardrails.GuardedCalls = int64(float64(m.Guardrails.LLMCalls) * cover)
		m.Guardrails.Blocked = m.Guardrails.GuardedCalls * int64(1+rng.IntN(3)) / 100
		m.PII.Spans = m.Guardrails.LLMCalls / 5
		m.PII.SpansRedacted = int64(float64(m.PII.Spans) * (0.9 + 0.1*rng.Float64()))
		m.ContentPresentSpans = m.Guardrails.LLMCalls / 3
		m.Agent = batch.AgentCounts{Runs: m.Guardrails.LLMCalls / 4, ToolCalls: m.Guardrails.LLMCalls / 3, ToolErrors: int64(rng.IntN(4)),
			HighImpactCalls: m.Guardrails.LLMCalls / 40, Escalations: int64(rng.IntN(2))}
		m.Agent.HighImpactApproved = m.Agent.HighImpactCalls
		if cover < 0.85 {
			m.Anomalies = append(m.Anomalies, batch.Anomaly{Start: h.Format(time.RFC3339), End: h.Add(time.Hour).Format(time.RFC3339), Kind: "guardrail_rate"})
		}
		if h.Equal(switchAt) {
			m.Transitions = append(m.Transitions, batch.Transition{At: h.Add(17 * time.Minute).Format(time.RFC3339), Kind: "model", From: "anthropic.claude-sonnet-4-5", To: "anthropic.claude-sonnet-4-6"})
		}
		if h.Equal(start.Add(30 * time.Hour)) {
			m.Transitions = append(m.Transitions, batch.Transition{At: h.Add(41 * time.Minute).Format(time.RFC3339), Kind: "prompt", From: "13", To: "14"})
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
			return err
		}
		if err := q.Enqueue(env, next); err != nil {
			return err
		}
	}
	fmt.Printf("wrote %d hourly batches to %s as %s\n", q.State().Seq, dir, id.AgentID)
	return nil
}
