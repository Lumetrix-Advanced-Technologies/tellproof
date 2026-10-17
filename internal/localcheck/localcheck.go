// Package localcheck evaluates the local evidence log against the evidence
// objectives (D-055): the log is verified first (signatures, chain,
// nothing missing), then the verified batches and the policy's registers go
// through the same evaluator that issues hosted packs. The result is a
// self-assessment, signed by nobody; a pack is the same evaluation signed by
// an independent issuer.
package localcheck

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/evaluate"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/localreport"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/policy"
)

// Options for a check.
type Options struct {
	DataDir      string
	Policy       *policy.Policy
	Since, Until string // local dates (the policy's timezone), inclusive; empty: the last 30 days that have data, up to today
	FromFirst    bool   // with no Since: start on the log's first day instead
	Now          func() time.Time
}

// Report is a check's outcome.
type Report struct {
	AISystem    string               `json:"ai_system"`
	Environment string               `json:"environment,omitempty"`
	Period      evaluate.Period      `json:"period"`
	Timezone    string               `json:"timezone"`
	Log         LogStatus            `json:"log"`
	Objectives  []evaluate.Result    `json:"objectives"`
	Summary     map[string]int       `json:"summary"` // pass, attention, fail
	Evaluation  *evaluate.Evaluation `json:"-"`
}

// LogStatus is the local log's verification.
type LogStatus struct {
	AgentID    string `json:"agent_id"`
	Verified   bool   `json:"verified"`
	Batches    int    `json:"batches"`
	LastDigest string `json:"last_digest,omitempty"`
	Problem    string `json:"problem,omitempty"`
}

// ErrUnverified is a local log that failed verification: nothing in it is evaluated.
var ErrUnverified = errors.New("localcheck: the local evidence log failed verification")

// ErrNoData is a log with no batches yet.
var ErrNoData = errors.New("localcheck: the local evidence log has no batches yet")

// Run verifies the log and evaluates it.
func Run(o Options) (*Report, error) {
	r, in, err := prepare(o)
	if err != nil {
		return r, err
	}
	ev, err := evaluate.Evaluate(in)
	if err != nil {
		return r, err
	}
	r.Evaluation, r.Objectives = ev, ev.Objectives
	r.Summary = map[string]int{"pass": 0, "attention": 0, "fail": 0}
	for _, o := range ev.Objectives {
		r.Summary[o.Status]++
	}
	return r, nil
}

// prepare verifies the log and builds the evaluator's input from the
// verified batches and the policy.
func prepare(o Options) (*Report, evaluate.Input, error) {
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	var in evaluate.Input
	sum, batches, err := localreport.ReadBatches(o.DataDir, localreport.Range{})
	if err != nil {
		return nil, in, err
	}
	r := &Report{AISystem: o.Policy.AISystem, Environment: o.Policy.Environment, Timezone: o.Policy.Timezone,
		Log: LogStatus{AgentID: sum.AgentID, Verified: sum.Verified, Batches: sum.Batches, LastDigest: sum.LastHash, Problem: sum.Problem}}
	if !sum.Verified {
		return r, in, fmt.Errorf("%w: %s", ErrUnverified, sum.Problem)
	}
	if len(batches) == 0 {
		return r, in, ErrNoData
	}
	loc, _ := time.LoadLocation(o.Policy.Timezone)
	if r.Period, err = period(o, batches, loc, now()); err != nil {
		return r, in, err
	}
	windows, usage, evals, err := evaluate.FromBatches(batches)
	if err != nil {
		return r, in, err
	}
	in = evaluate.Input{System: o.Policy.System(r.Period.Start, r.Period.End), Windows: windows, Usage: usage,
		Changes: o.Policy.Changes, Incidents: o.Policy.Incidents, Evals: append(evals, o.Policy.Evals...)}
	if in.Changes == nil {
		in.Changes = []evaluate.Change{}
	}
	if in.Incidents == nil {
		in.Incidents = []evaluate.Incident{}
	}
	return r, in, nil
}

// period is the requested one, or the last 30 local days that have data, up to today.
func period(o Options, bs []batch.Batch, loc *time.Location, now time.Time) (evaluate.Period, error) {
	today := now.In(loc).Format("2006-01-02")
	first, err := time.Parse(time.RFC3339, bs[0].Window.Start)
	if err != nil {
		return evaluate.Period{}, err
	}
	p := evaluate.Period{Start: o.Since, End: o.Until}
	if p.End == "" {
		p.End = today
	}
	if p.Start == "" {
		end, err := time.ParseInLocation("2006-01-02", p.End, loc)
		if err != nil {
			return p, fmt.Errorf("localcheck: until %q is not a date (YYYY-MM-DD)", p.End)
		}
		p.Start = end.AddDate(0, 0, -29).Format("2006-01-02")
		if f := first.In(loc).Format("2006-01-02"); f > p.Start || o.FromFirst {
			p.Start = f
		}
	}
	for _, d := range []string{p.Start, p.End} {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			return p, fmt.Errorf("localcheck: %q is not a date (YYYY-MM-DD)", d)
		}
	}
	if p.End < p.Start {
		return p, errors.New("localcheck: the period ends before it starts")
	}
	return p, nil
}

// Failed reports whether the check should fail a build: any objective that
// failed, or, when strict, any that needs attention.
func (r *Report) Failed(strict bool) bool {
	return r.Summary["fail"] > 0 || (strict && r.Summary["attention"] > 0)
}

var marks = map[string]string{"pass": "✓ PASS", "attention": "! ATTENTION", "fail": "✕ FAIL"}

// WriteText writes the report for a person.
func WriteText(w io.Writer, r *Report) error {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	name := r.AISystem
	if r.Environment != "" {
		name += " (" + r.Environment + ")"
	}
	p("Tellproof check: %s, %s to %s (%s)", name, r.Period.Start, r.Period.End, r.Timezone)
	p("  Evidence: %d batches from agent %s, verified (signatures and chain).", r.Log.Batches, r.Log.AgentID)
	p("")
	unverified := false
	for _, o := range r.Objectives {
		p("%-12s %-10s %s", marks[o.Status], o.ID, o.Title)
		p("             %s", o.Measured)
		p("             target: %s", o.Threshold)
		// What to record so the objective passes: the register entries it is missing.
		if ts, ok := o.Values["unlogged"].([]batch.Transition); ok {
			for _, t := range ts {
				p("             missing change: %s %s %s -> %s (add to \"changes\")", t.At, t.Kind, t.From, t.To)
			}
		}
		if as, ok := o.Values["unacknowledged"].([]batch.Anomaly); ok {
			for _, a := range as {
				p("             missing incident: %s anomaly %s to %s (add to \"incidents\", acknowledged within 60 min)", a.Kind, a.Start, a.End)
			}
		}
		if len(o.Maps) > 0 {
			p("             supports: %s", strings.Join(o.Maps, "; "))
		}
		for _, m := range o.Maps {
			unverified = unverified || strings.HasSuffix(m, "*")
		}
	}
	p("")
	p("%d pass, %d attention, %d fail.", r.Summary["pass"], r.Summary["attention"], r.Summary["fail"])
	if unverified {
		p("* Framework mapping not yet verified against the framework's text.")
	}
	p("This is a self-assessment of your own signed log. Packs signed by an independent issuer: %s", localreport.EarlyAccess)
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteJSON writes the report as JSON.
func WriteJSON(w io.Writer, r *Report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// WriteJUnit writes one test case per objective, for CI systems: fail is a
// failure; attention is a failure when strict, otherwise skipped with its reason.
func WriteJUnit(w io.Writer, r *Report, strict bool) error {
	type failure struct {
		Message string `xml:"message,attr"`
		Text    string `xml:",chardata"`
	}
	type skipped struct {
		Message string `xml:"message,attr"`
	}
	type testcase struct {
		Name      string   `xml:"name,attr"`
		Classname string   `xml:"classname,attr"`
		Failure   *failure `xml:"failure,omitempty"`
		Skipped   *skipped `xml:"skipped,omitempty"`
		Out       string   `xml:"system-out,omitempty"`
	}
	type suite struct {
		XMLName  xml.Name   `xml:"testsuite"`
		Name     string     `xml:"name,attr"`
		Tests    int        `xml:"tests,attr"`
		Failures int        `xml:"failures,attr"`
		Skipped  int        `xml:"skipped,attr"`
		Cases    []testcase `xml:"testcase"`
	}
	s := suite{Name: "tellproof: " + r.AISystem + " " + r.Period.Start + " to " + r.Period.End, Tests: len(r.Objectives)}
	for _, o := range r.Objectives {
		msg := o.Measured + "; target: " + o.Threshold
		c := testcase{Name: o.ID + " " + o.Title, Classname: "tellproof." + o.ID, Out: msg}
		switch {
		case o.Status == "fail" || (o.Status == "attention" && strict):
			c.Failure = &failure{Message: strings.ToUpper(o.Status[:1]) + o.Status[1:] + ": " + msg, Text: strings.Join(o.Maps, "\n")}
			s.Failures++
		case o.Status == "attention":
			c.Skipped = &skipped{Message: "Attention: " + msg}
			s.Skipped++
		}
		s.Cases = append(s.Cases, c)
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(s); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}
