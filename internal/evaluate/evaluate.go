// Package evaluate applies the evidence objectives (spec/objectives.json)
// to a period of aggregates, provider usage and registers (docs/architecture.md
// §4.3). It is the Go twin of concierge/evaluate.py and must give the same
// results on the same inputs (D-021; parity test); the semantics the rules
// leave open are fixed in D-021, D-028 and D-040.
package evaluate

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // periods are whole days in the system's timezone, wherever the server runs

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/jcs"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/spec"
	specdata "github.com/lumetrix-advanced-technologies/tellproof/spec"
)

// incidentWindow: EO-INC-01 asks for acknowledgement within 60 minutes.
const incidentWindow = 60 * time.Minute

// System is the evaluated AI system's declarations (concierge system.json).
type System struct {
	Organisation    string            `json:"organisation"`
	AISystem        string            `json:"ai_system"`
	Environment     string            `json:"environment"`
	Description     string            `json:"description,omitempty"`
	Profiles        []string          `json:"profiles,omitempty"`
	Timezone        string            `json:"timezone"`
	Period          Period            `json:"period"`
	DeclaredModels  []DeclaredModel   `json:"declared_models"`
	DeclaredRegions []string          `json:"declared_regions"`
	Providers       map[string]string `json:"providers,omitempty"`
	Limitations     []string          `json:"limitations,omitempty"`
	Sources         []json.RawMessage `json:"sources,omitempty"`
	AssuranceLevel  json.RawMessage   `json:"assurance_level,omitempty"`
}

// Period is whole local days, inclusive (YYYY-MM-DD).
type Period struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type DeclaredModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Role     string `json:"role,omitempty"`
}

// Window is one hour of aggregates (a batch payload's window, metrics, observed and commit).
type Window struct {
	Window   batch.Window   `json:"window"`
	Metrics  batch.Metrics  `json:"metrics"`
	Observed batch.Observed `json:"observed"`
	Commit   batch.Commit   `json:"commit"`
}

// UsageRow is provider-billed usage for one hour. Region "" means the
// provider reports none (OpenAI, Anthropic: D-028).
type UsageRow struct {
	Provider     string
	Model        string
	Region       string
	Start        time.Time
	InputTokens  int64
	OutputTokens int64
	Requests     *int64
}

// Change is a change-register entry: a local date, model or prompt, and what changed.
type Change struct {
	Date string `json:"date"`
	Kind string `json:"kind"`
	What string `json:"what"`
}

// Incident is an incident-register entry; times are RFC 3339, empty when unknown.
type Incident struct {
	ID           string `json:"id"`
	Opened       string `json:"opened"`
	Acknowledged string `json:"acknowledged"`
	Resolved     string `json:"resolved"`
	Summary      string `json:"summary"`
}

// Input is everything one evaluation reads. Windows and usage outside the
// period are ignored.
type Input struct {
	System    System
	Windows   []Window
	Usage     []UsageRow
	Changes   []Change
	Incidents []Incident
	Evals     []batch.Eval
}

// Evaluation is the result (the concierge's evaluation.json without its file fingerprints).
type Evaluation struct {
	ObjectiveSetHash string              `json:"objective_set_hash"`
	Period           map[string]string   `json:"period"`
	Subject          map[string]string   `json:"subject"`
	Profiles         []string            `json:"profiles"`
	AssuranceLevel   json.RawMessage     `json:"assurance_level"`
	Sources          []json.RawMessage   `json:"sources"`
	Objectives       []Result            `json:"objectives"`
	Coverage         Coverage            `json:"coverage"`
	Lineage          []Lineage           `json:"lineage"`
	Changes          []Change            `json:"changes"`
	Incidents        []map[string]string `json:"incidents"`
	Limitations      []string            `json:"limitations"`
	Windows          int                 `json:"windows"`
	LeafCount        int64               `json:"leaf_count"`
	PeriodStart      time.Time           `json:"-"`
	PeriodEnd        time.Time           `json:"-"`
}

// Result is one objective's outcome.
type Result struct {
	ID        string         `json:"id"`
	Version   int            `json:"-"`
	Title     string         `json:"title"`
	Status    string         `json:"status"` // pass | attention | fail
	Threshold string         `json:"threshold"`
	Measured  string         `json:"measured"`
	Basis     string         `json:"basis"`
	Maps      []string       `json:"maps"`
	Rule      string         `json:"rule"`
	Values    map[string]any `json:"values"`
}

type Coverage struct {
	Basis     string        `json:"basis"`
	TotalPct  float64       `json:"total_pct"`
	Providers []CoverageRow `json:"providers"`
}

type CoverageRow struct {
	Provider       string `json:"provider"`
	Region         string `json:"region"`
	BilledTokens   int64  `json:"billed_tokens"`
	TracedTokens   int64  `json:"traced_tokens"`
	BilledRequests *int64 `json:"billed_requests"`
	TracedRequests int64  `json:"traced_requests"`
	hasUsage       bool
}

type Lineage struct {
	Provider        string   `json:"provider"`
	Model           string   `json:"model"`
	Role            string   `json:"role"`
	Region          string   `json:"region"`
	ShareOfCallsPct *float64 `json:"share_of_calls_pct"`
}

var defaultAssurance = json.RawMessage(`{"level":1,"meaning":"Telemetry and provider-usage exports supplied by the vendor; reconciliation computed by the issuer."}`)

// ObjectiveSetHash is "sha256:" + SHA-256 of the JCS form of spec/objectives.json.
func ObjectiveSetHash() (string, error) {
	c, err := jcs.Canonicalize([]byte(specdata.Objectives))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ErrPeriod: the period or timezone cannot be used.
var ErrPeriod = errors.New("evaluate: invalid period")

// PeriodBounds is the period as a half-open UTC interval: whole days in the system's timezone.
func PeriodBounds(s System) (time.Time, time.Time, error) {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: timezone %q: %v", ErrPeriod, s.Timezone, err)
	}
	a, err1 := time.ParseInLocation("2006-01-02", s.Period.Start, loc)
	b, err2 := time.ParseInLocation("2006-01-02", s.Period.End, loc)
	if err1 != nil || err2 != nil || b.Before(a) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: the period must be two dates, start before end", ErrPeriod)
	}
	return a.UTC(), b.AddDate(0, 0, 1).UTC(), nil
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, fmt.Errorf("evaluate: time %q: %w", s, err)
	}
	return t.UTC(), nil
}

var durationRE = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?)?$`)

func parseDuration(s string) (time.Duration, error) {
	m := durationRE.FindStringSubmatch(s)
	if m == nil || (m[1] == "" && m[2] == "" && m[3] == "") {
		return 0, fmt.Errorf("evaluate: unsupported duration %q", s)
	}
	n := func(x string) time.Duration { v, _ := strconv.Atoi(x); return time.Duration(v) }
	return n(m[1])*24*time.Hour + n(m[2])*time.Hour + n(m[3])*time.Minute, nil
}

func days(d time.Duration) int { // Python timedelta.days: floor
	q := d / (24 * time.Hour)
	if d < 0 && d%(24*time.Hour) != 0 {
		q--
	}
	return int(q)
}

type provRegion struct{ provider, region string }
type modelKey struct{ provider, model, region string }

// versionKey is a live (model, prompt version), or a model alone when no prompt versions are emitted.
type versionKey struct {
	model      string
	version    string
	hasVersion bool
}

type traced struct{ tokens, calls int64 }
type billed struct {
	tokens, requests int64
	requestsKnown    bool
}

// facts is everything the rules measure, computed once.
type facts struct {
	sys                         System
	loc                         *time.Location
	start, end, firstWindow     time.Time
	m                           map[string]int64
	traced                      map[provRegion]*traced
	callsByModel                map[modelKey]int64
	modelOrder                  []modelKey
	transitions                 []batch.Transition
	anomalies                   []batch.Anomaly
	liveVersions                []versionKey
	firstSeen                   map[versionKey]time.Time
	billed                      map[provRegion]*billed
	changes                     []Change
	incidents                   []Incident
	evals                       []batch.Eval
	modelsObserved              []provRegion // (provider, model)
	regionsObserved             []string
	callsNoModel, callsNoRegion int64
	canaryMissed                map[string]int64 // kind -> canaries not caught (EO-CAN-01)
}

func newFacts(in Input, windows []Window, usage []UsageRow, start, end time.Time, loc *time.Location) (*facts, error) {
	f := &facts{sys: in.System, loc: loc, start: start, end: end, firstWindow: start, m: map[string]int64{},
		traced: map[provRegion]*traced{}, callsByModel: map[modelKey]int64{}, firstSeen: map[versionKey]time.Time{},
		billed: map[provRegion]*billed{}, changes: in.Changes, incidents: in.Incidents, evals: in.Evals}
	seen := func(k versionKey, t time.Time) {
		if _, ok := f.firstSeen[k]; !ok {
			f.firstSeen[k] = t
			f.liveVersions = append(f.liveVersions, k)
		}
	}
	sawObserved := false
	for _, w := range windows {
		wm := w.Metrics
		for _, t := range wm.Tokens {
			k := provRegion{t.Provider, t.Region}
			if f.traced[k] == nil {
				f.traced[k] = &traced{}
			}
			f.traced[k].tokens += t.InputTokens + t.OutputTokens
			f.traced[k].calls += t.Calls
			mk := modelKey{t.Provider, t.Model, t.Region}
			if _, ok := f.callsByModel[mk]; !ok {
				f.modelOrder = append(f.modelOrder, mk)
			}
			f.callsByModel[mk] += t.Calls
		}
		f.m["pii_spans"] += wm.PII.Spans
		f.m["pii_spans_redacted"] += wm.PII.SpansRedacted
		f.m["content_present_spans"] += wm.ContentPresentSpans
		f.m["llm_calls"] += wm.Guardrails.LLMCalls
		f.m["guarded_calls"] += wm.Guardrails.GuardedCalls
		f.m["blocked"] += wm.Guardrails.Blocked
		f.m["runs"] += wm.Agent.Runs
		f.m["tool_calls"] += wm.Agent.ToolCalls
		f.m["tool_errors"] += wm.Agent.ToolErrors
		f.m["high_impact_calls"] += wm.Agent.HighImpactCalls
		f.m["high_impact_approved"] += wm.Agent.HighImpactApproved
		f.m["escalations"] += wm.Agent.Escalations
		for _, c := range wm.Canaries {
			f.m["canaries_run"] += c.Run
			f.m["canaries_caught"] += c.Caught
			if c.Run > c.Caught {
				if f.canaryMissed == nil {
					f.canaryMissed = map[string]int64{}
				}
				f.canaryMissed[c.Kind] += c.Run - c.Caught
			}
		}
		f.transitions = append(f.transitions, wm.Transitions...)
		f.anomalies = append(f.anomalies, wm.Anomalies...)
		ws, err := parseTime(w.Window.Start)
		if err != nil {
			return nil, err
		}
		// The first window that observed anything: versions live there were live
		// before the period. A window with only usage or eval runs observed nothing (D-040).
		if !sawObserved && (len(w.Observed.Models) > 0 || len(w.Observed.Versions) > 0) {
			f.firstWindow, sawObserved = ws, true
		}
		for _, v := range w.Observed.Versions {
			seen(versionKey{v.Model, v.PromptVersion, true}, ws)
		}
		if len(w.Observed.Versions) == 0 {
			for _, om := range w.Observed.Models {
				seen(versionKey{model: om.Model}, ws)
			}
		}
		f.m["leaves"] += w.Commit.Leaves
	}
	for _, u := range usage {
		k := provRegion{u.Provider, u.Region}
		b := f.billed[k]
		if b == nil {
			b = &billed{requestsKnown: true}
			f.billed[k] = b
		}
		b.tokens += u.InputTokens + u.OutputTokens
		if u.Requests == nil {
			b.requestsKnown = false
		} else {
			b.requests += *u.Requests
		}
	}
	// "unknown" is not a model or a region: it cannot be checked against a declaration.
	ms := map[provRegion]bool{}
	for _, k := range f.modelOrder {
		if k.model == "unknown" {
			f.callsNoModel += f.callsByModel[k]
			continue
		}
		ms[provRegion{k.provider, k.model}] = true
	}
	for k := range ms {
		f.modelsObserved = append(f.modelsObserved, k)
	}
	slices.SortFunc(f.modelsObserved, cmpPR)
	rs := map[string]bool{}
	for k, t := range f.traced {
		if k.region == "unknown" {
			f.callsNoRegion += t.calls
			continue
		}
		rs[k.region] = true
	}
	for r := range rs {
		f.regionsObserved = append(f.regionsObserved, r)
	}
	slices.Sort(f.regionsObserved)
	return f, nil
}

func cmpPR(a, b provRegion) int {
	return cmp.Or(cmp.Compare(a.provider, b.provider), cmp.Compare(a.region, b.region))
}

func (f *facts) providerName(p string) string {
	if n, ok := f.sys.Providers[p]; ok {
		return n
	}
	return p
}

// coverage over providers that have usage data; others are listed, not
// counted. A provider whose usage has no region is compared across all its
// traced regions (D-028).
func (f *facts) coverage() ([]CoverageRow, int64, int64) {
	regionless := map[string]bool{}
	for k := range f.billed {
		if k.region == "" {
			regionless[k.provider] = true
		}
	}
	tr := map[provRegion]*traced{}
	for k, t := range f.traced {
		if regionless[k.provider] {
			k.region = ""
		}
		if tr[k] == nil {
			tr[k] = &traced{}
		}
		tr[k].tokens += t.tokens
		tr[k].calls += t.calls
	}
	var keys []provRegion
	for k := range f.billed {
		keys = append(keys, k)
	}
	for k := range tr {
		if f.billed[k] == nil {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, cmpPR)
	var rows []CoverageRow
	var num, den int64
	for _, k := range keys {
		b, t := f.billed[k], tr[k]
		if t == nil {
			t = &traced{}
		}
		row := CoverageRow{Provider: f.providerName(k.provider), Region: k.region, TracedTokens: t.tokens, TracedRequests: t.calls, hasUsage: b != nil}
		if b != nil {
			num += t.tokens
			den += b.tokens
			row.BilledTokens = b.tokens
			if b.requestsKnown {
				r := b.requests
				row.BilledRequests = &r
			}
		}
		rows = append(rows, row)
	}
	return rows, num, den
}

func statusRatio(num, den int64, threshold, band float64) string {
	if den == 0 {
		return "attention"
	}
	r := float64(num) / float64(den)
	switch {
	case r >= threshold:
		return "pass"
	case r >= threshold-band:
		return "attention"
	}
	return "fail"
}

func statusMax(v, threshold, band float64) string {
	switch {
	case v <= threshold:
		return "pass"
	case v <= threshold+band:
		return "attention"
	}
	return "fail"
}

func val(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func (f *facts) localDate(t time.Time) time.Time {
	y, m, d := t.In(f.loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func parseDate(s string) (time.Time, error) { return time.Parse("2006-01-02", strings.TrimSpace(s)) }

func (f *facts) inPeriodDate(s string) bool {
	d, err := parseDate(s)
	if err != nil {
		return false
	}
	return !d.Before(f.localDate(f.start)) && !d.After(f.localDate(f.end.Add(-time.Microsecond)))
}

// logged: the change register has the same kind within a day either side (local dates).
func (f *facts) logged(t batch.Transition) bool {
	at, err := parseTime(t.At)
	if err != nil {
		return false
	}
	day := f.localDate(at)
	for _, c := range f.changes {
		d, err := parseDate(c.Date)
		if err == nil && c.Kind == t.Kind && absDays(d.Sub(day)) <= 1 {
			return true
		}
	}
	return false
}

func absDays(d time.Duration) int {
	n := days(d)
	if n < 0 {
		return -n
	}
	return n
}

// acknowledged: an incident opened within 60 min of the window was acknowledged within 60 min of opening.
func (f *facts) acknowledged(a batch.Anomaly) bool {
	start, err1 := parseTime(a.Start)
	end, err2 := parseTime(a.End)
	if err1 != nil || err2 != nil {
		return false
	}
	for _, i := range f.incidents {
		opened, err := parseTime(i.Opened)
		if err != nil || opened.Before(start.Add(-incidentWindow)) || opened.After(end.Add(incidentWindow)) || i.Acknowledged == "" {
			continue
		}
		if ack, err := parseTime(i.Acknowledged); err == nil && ack.Sub(opened) <= incidentWindow {
			return true
		}
	}
	return false
}

func mapLabel(r spec.FrameworkRef) string {
	star := ""
	if !r.Verified {
		star = "*"
	}
	if r.Ref == "theme" || strings.Contains(r.Framework, r.Ref) {
		return r.Framework + ": " + r.Title + star
	}
	return r.Framework + " " + r.Ref + " " + r.Title + star
}

func (f *facts) objective(o spec.Objective, refs map[string]spec.FrameworkRef) (Result, error) {
	rule := o.Rule
	m := f.m
	res := Result{ID: o.ID, Version: o.Version, Title: o.Title, Basis: o.Basis, Rule: rule.Kind, Values: map[string]any{}}
	th, band := val(rule.Threshold), val(rule.AttentionBand)
	switch {
	case o.ID == "EO-LOG-01" || (rule.Kind == "ratio_at_least" && rule.Numerator == "traced_tokens"):
		_, num, den := f.coverage()
		res.Status = statusRatio(num, den, th, band)
		res.Measured = "No provider usage supplied"
		if den != 0 {
			res.Measured = pctText(num, den, 1) + " of billed tokens traced"
		}
		res.Threshold = ">= " + f1(th*100) + "% of provider-billed tokens traced"
		res.Values = map[string]any{"numerator": num, "denominator": den}
	case rule.Kind == "ratio_at_least":
		num, den := m[rule.Numerator], m[rule.Denominator]
		res.Status = statusRatio(num, den, th, band)
		res.Values = map[string]any{"numerator": num, "denominator": den}
		switch rule.Numerator {
		case "pii_spans_redacted":
			res.Measured = "No spans tagged as personal data"
			if den != 0 {
				res.Measured = pctText(num, den, 1) + " of tagged spans redacted"
			}
			res.Threshold = g(th*100) + "% of spans tagged as personal data pass the redaction processor"
		case "guarded_calls":
			res.Measured = "No LLM calls"
			if den != 0 {
				res.Measured = pctText(num, den, 1) + " covered; " + pctText(m["blocked"], den, 2) + " blocked"
			}
			res.Threshold = ">= " + g(th*100) + "% of LLM calls pass input and output guardrails"
		case "canaries_caught":
			res.Measured = "No guardrail canaries run"
			if den != 0 {
				res.Measured = fmt.Sprintf("%d of %d canaries caught", num, den)
				if missed := slices.Sorted(maps.Keys(f.canaryMissed)); len(missed) > 0 {
					parts := make([]string, len(missed))
					for i, k := range missed {
						parts[i] = fmt.Sprintf("%s (%d)", k, f.canaryMissed[k])
					}
					res.Measured += "; missed: " + strings.Join(parts, ", ")
				}
			}
			res.Threshold = "Every synthetic known-bad probe sent through the guardrail is caught"
		default:
			res.Measured, res.Threshold = pctText(num, den, 1), ">= "+g(th*100)+"%"
		}
	case rule.Kind == "max_value":
		if rule.Metric != "tool_error_rate" {
			return res, fmt.Errorf("evaluate: %s: no measure for metric %s", o.ID, rule.Metric)
		}
		calls := m["tool_calls"]
		v := 0.0
		if calls != 0 {
			v = float64(m["tool_errors"]) / float64(calls)
		}
		res.Status = statusMax(v, th, band)
		res.Measured = "No tool calls"
		if calls != 0 {
			res.Measured = fmt.Sprintf("%s tool errors; %s of runs escalated; %d of %d high-impact calls approved",
				pctText(m["tool_errors"], calls, 2), pctText(m["escalations"], m["runs"], 1), m["high_impact_approved"], m["high_impact_calls"])
		}
		res.Threshold = "Tool-call error rate <= " + g(th*100) + "%"
		res.Values = map[string]any{"value": v, "tool_errors": m["tool_errors"], "tool_calls": calls}
	case rule.Kind == "zero_count":
		var n int
		switch rule.Metric {
		case "content_present_spans":
			n = int(m["content_present_spans"])
			res.Measured = fmt.Sprintf("%d spans with message content attributes", n)
			res.Threshold = "No prompt or completion text exported to telemetry"
		case "unlogged_transitions":
			unlogged := []batch.Transition{}
			for _, t := range f.transitions {
				if !f.logged(t) {
					unlogged = append(unlogged, t)
				}
			}
			n = len(unlogged)
			inPeriod := 0
			for _, c := range f.changes {
				if f.inPeriodDate(c.Date) {
					inPeriod++
				}
			}
			res.Measured = fmt.Sprintf("%d change(s) logged, %d unlogged transition(s)", inPeriod, n)
			res.Threshold = "Every model or prompt change seen in telemetry is in the change register"
			res.Values["unlogged"] = unlogged
		case "unacknowledged_anomalies":
			unack := []batch.Anomaly{}
			for _, a := range f.anomalies {
				if !f.acknowledged(a) {
					unack = append(unack, a)
				}
			}
			n = len(unack)
			inPeriod := 0
			for _, i := range f.incidents {
				if t, err := parseTime(i.Opened); err == nil && !t.Before(f.start) && t.Before(f.end) {
					inPeriod++
				}
			}
			if n == 0 {
				res.Measured = fmt.Sprintf("%d incident(s); all acknowledged in time", inPeriod)
			} else {
				res.Measured = fmt.Sprintf("%d incident(s); %d anomaly window(s) without an incident acknowledged within 60 min", inPeriod, n)
			}
			res.Threshold = "Every guardrail spike or error burst has an incident record acknowledged within 60 min"
			res.Values["unacknowledged"] = unack
		default:
			return res, fmt.Errorf("evaluate: %s: no measure for metric %s", o.ID, rule.Metric)
		}
		res.Status = "fail"
		if n == 0 {
			res.Status = "pass"
		}
		res.Values["count"] = n
	case rule.Kind == "all_declared":
		bad := []string{}
		if rule.Observed == "models_observed" {
			declared := map[provRegion]bool{}
			for _, d := range f.sys.DeclaredModels {
				declared[provRegion{d.Provider, d.Model}] = true
			}
			observed := [][]string{}
			for _, k := range f.modelsObserved {
				observed = append(observed, []string{k.provider, k.region})
				if !declared[k] {
					bad = append(bad, k.provider+" "+k.region)
				}
			}
			res.Measured = fmt.Sprintf("%d of %d models observed are declared", len(observed)-len(bad), len(observed))
			if len(bad) > 0 {
				res.Measured += "; undeclared: " + strings.Join(bad, ", ")
			}
			res.Threshold = "Every model in use is on the declared inventory"
			res.Values = map[string]any{"observed": observed, "undeclared": bad}
		} else {
			declared := map[string]bool{}
			for _, r := range f.sys.DeclaredRegions {
				declared[r] = true
			}
			observed := append([]string{}, f.regionsObserved...)
			for _, r := range observed {
				if !declared[r] {
					bad = append(bad, r)
				}
			}
			if len(bad) == 0 {
				res.Measured = "All calls in declared regions (" + strings.Join(observed, ", ") + ")"
			} else {
				res.Measured = "Calls in undeclared region(s): " + strings.Join(bad, ", ")
			}
			ds := slices.Sorted(func(yield func(string) bool) {
				for r := range declared {
					if !yield(r) {
						return
					}
				}
			})
			res.Threshold = "Every region in use is declared (" + strings.Join(ds, ", ") + ")"
			res.Values = map[string]any{"observed": observed, "undeclared": bad}
		}
		res.Status = "fail"
		if len(bad) == 0 {
			res.Status = "pass"
		}
	case rule.Kind == "freshness_within":
		st, measured, values, err := f.freshness(rule)
		if err != nil {
			return res, err
		}
		res.Status, res.Measured, res.Values = st, measured, values
		res.Threshold = fmt.Sprintf("Eval run on each live model and prompt version before release (at most %s before); pass rate >= %s%%",
			strings.ToLower(strings.TrimPrefix(rule.MaxAge, "P")), g(val(rule.MinPassRate)*100))
	default:
		return res, fmt.Errorf("evaluate: %s: unknown rule kind %s", o.ID, rule.Kind)
	}
	for _, id := range o.MapsTo {
		r, ok := refs[id]
		if !ok {
			return res, fmt.Errorf("evaluate: %s: unknown framework ref %s", o.ID, id)
		}
		res.Maps = append(res.Maps, mapLabel(r))
	}
	return res, nil
}

type problem struct {
	Model         string  `json:"model"`
	PromptVersion *string `json:"prompt_version"`
	LiveDays      int     `json:"live_days"`
	Status        string  `json:"status"`
}

// freshness (D-021): each live (model, prompt version) needs a passing eval
// that ran before it was first seen and at most max_age before; versions
// already live in the first window need only one that ran before. A version
// without one is attention while live less than max_age at period end, fail after.
func (f *facts) freshness(rule spec.Rule) (string, string, map[string]any, error) {
	maxAge, err := parseDuration(rule.MaxAge)
	if err != nil {
		return "", "", nil, err
	}
	minRate := val(rule.MinPassRate)
	type run struct {
		e  batch.Eval
		at time.Time
	}
	var passing []run
	for _, e := range f.evals {
		if e.PassRate >= minRate {
			at, err := parseTime(e.RanAt)
			if err != nil {
				return "", "", nil, err
			}
			passing = append(passing, run{e, at})
		}
	}
	order := slices.Clone(f.liveVersions)
	slices.SortStableFunc(order, func(a, b versionKey) int { return f.firstSeen[a].Compare(f.firstSeen[b]) })
	problems := []problem{}
	var latest *run
	for _, k := range order {
		seen := f.firstSeen[k]
		carried := !seen.After(f.firstWindow)
		var best *run
		for i := range passing {
			r := &passing[i]
			if r.e.Model != k.model || (k.hasVersion && r.e.PromptVersion != k.version) || r.at.After(seen) {
				continue
			}
			if !carried && seen.Sub(r.at) > maxAge {
				continue
			}
			if best == nil || r.at.After(best.at) {
				best = r
			}
		}
		if best != nil {
			if latest == nil || best.at.After(latest.at) {
				latest = best
			}
			continue
		}
		live := f.end.Sub(seen)
		st := "fail"
		if live < maxAge {
			st = "attention"
		}
		p := problem{Model: k.model, LiveDays: days(live), Status: st}
		if k.hasVersion {
			v := k.version
			p.PromptVersion = &v
		}
		problems = append(problems, p)
	}
	if len(f.liveVersions) == 0 {
		return "attention", "No model or prompt versions observed", map[string]any{"problems": problems}, nil
	}
	if len(problems) == 0 {
		age := days(f.end.Sub(latest.at))
		return "pass", fmt.Sprintf("%s%% pass, last run %d days before period end", f1(latest.e.PassRate*100), age),
			map[string]any{"problems": problems}, nil
	}
	worst := "attention"
	for _, p := range problems {
		if p.Status == "fail" {
			worst = "fail"
		}
	}
	p := problems[0]
	what := p.Model
	if p.PromptVersion != nil {
		what = p.Model + " prompt v" + *p.PromptVersion
	}
	extra := ""
	if len(problems) > 1 {
		extra = fmt.Sprintf(" (+%d more)", len(problems)-1)
	}
	return worst, fmt.Sprintf("No passing run on live %s (live %d days)%s", what, p.LiveDays, extra), map[string]any{"problems": problems}, nil
}

// Evaluate applies the embedded objectives to in.
func Evaluate(in Input) (*Evaluation, error) {
	var objs spec.Objectives
	if err := spec.Decode(strings.NewReader(specdata.Objectives), &objs); err != nil {
		return nil, err
	}
	var refsDoc spec.FrameworkRefs
	if err := spec.Decode(strings.NewReader(specdata.FrameworkRefs), &refsDoc); err != nil {
		return nil, err
	}
	refs := map[string]spec.FrameworkRef{}
	for _, r := range refsDoc.Refs {
		refs[r.ID] = r
	}
	start, end, err := PeriodBounds(in.System)
	if err != nil {
		return nil, err
	}
	loc, _ := time.LoadLocation(in.System.Timezone)

	var windows []Window
	for _, w := range in.Windows {
		t, err := parseTime(w.Window.Start)
		if err != nil {
			return nil, err
		}
		if !t.Before(start) && t.Before(end) {
			windows = append(windows, w)
		}
	}
	slices.SortStableFunc(windows, func(a, b Window) int { return cmp.Compare(a.Window.Start, b.Window.Start) })
	var usage []UsageRow
	for _, u := range in.Usage {
		if !u.Start.Before(start) && u.Start.Before(end) {
			usage = append(usage, u)
		}
	}
	f, err := newFacts(in, windows, usage, start, end, loc)
	if err != nil {
		return nil, err
	}

	ev := &Evaluation{PeriodStart: start, PeriodEnd: end, Profiles: in.System.Profiles, Sources: in.System.Sources,
		AssuranceLevel: in.System.AssuranceLevel, Windows: len(windows), LeafCount: f.m["leaves"]}
	if ev.ObjectiveSetHash, err = ObjectiveSetHash(); err != nil {
		return nil, err
	}
	if ev.Profiles == nil {
		ev.Profiles = []string{}
	}
	if ev.Sources == nil {
		ev.Sources = []json.RawMessage{}
	}
	if len(ev.AssuranceLevel) == 0 {
		ev.AssuranceLevel = defaultAssurance
	}
	ev.Period = map[string]string{"start": in.System.Period.Start, "end": in.System.Period.End, "timezone": in.System.Timezone,
		"utc_start": start.Format("2006-01-02T15:04:05Z"), "utc_end": end.Format("2006-01-02T15:04:05Z")}
	ev.Subject = map[string]string{}
	for k, v := range map[string]string{"organisation": in.System.Organisation, "ai_system": in.System.AISystem,
		"environment": in.System.Environment, "description": in.System.Description} {
		if v != "" {
			ev.Subject[k] = v
		}
	}
	for _, o := range objs.Objectives {
		r, err := f.objective(o, refs)
		if err != nil {
			return nil, err
		}
		ev.Objectives = append(ev.Objectives, r)
	}

	rows, num, den := f.coverage()
	ev.Coverage = Coverage{Basis: "tokens", TotalPct: pctFloor(num, den, 1), Providers: rows}
	if ev.Coverage.Providers == nil {
		ev.Coverage.Providers = []CoverageRow{}
	}
	var total int64
	for _, c := range f.callsByModel {
		total += c
	}
	roles := map[provRegion]string{}
	for _, d := range in.System.DeclaredModels {
		roles[provRegion{d.Provider, d.Model}] = d.Role
	}
	order := slices.Clone(f.modelOrder)
	slices.SortStableFunc(order, func(a, b modelKey) int { return cmp.Compare(f.callsByModel[b], f.callsByModel[a]) })
	ev.Lineage = []Lineage{}
	for _, k := range order {
		l := Lineage{Provider: f.providerName(k.provider), Model: k.model, Role: roles[provRegion{k.provider, k.model}], Region: k.region}
		if total != 0 {
			s := round1(float64(100*f.callsByModel[k]) / float64(total))
			l.ShareOfCallsPct = &s
		}
		ev.Lineage = append(ev.Lineage, l)
	}
	ev.Changes = []Change{}
	for _, c := range in.Changes {
		if f.inPeriodDate(c.Date) {
			ev.Changes = append(ev.Changes, c)
		}
	}
	ev.Incidents = []map[string]string{}
	for _, i := range in.Incidents {
		t, err := parseTime(i.Opened)
		if err != nil || t.Before(start) || !t.Before(end) {
			continue
		}
		row := map[string]string{}
		for k, v := range map[string]string{"id": i.ID, "opened": i.Opened, "acknowledged": i.Acknowledged, "resolved": i.Resolved, "summary": i.Summary} {
			if v != "" {
				row[k] = v
			}
		}
		ev.Incidents = append(ev.Incidents, row)
	}
	ev.Limitations = append(slices.Clone(in.System.Limitations),
		"Proves what the exported telemetry shows. It does not prove the model behaves well on inputs that never occurred.",
		"Coverage below 100% means some provider-billed usage has no matching trace; untraced usage is not assessed.")
	for _, r := range rows {
		if !r.hasUsage {
			ev.Limitations = append(ev.Limitations, fmt.Sprintf("No provider usage was supplied for %s in %s; its traced usage is listed but not counted in coverage.", r.Provider, r.Region))
		}
	}
	if f.callsNoModel > 0 {
		ev.Limitations = append(ev.Limitations, fmt.Sprintf("%d call(s) named no model; the model inventory cannot be checked for them.", f.callsNoModel))
	}
	if f.callsNoRegion > 0 {
		ev.Limitations = append(ev.Limitations, fmt.Sprintf("%d call(s) carried no region; region declarations cannot be checked for them.", f.callsNoRegion))
	}
	return ev, nil
}
