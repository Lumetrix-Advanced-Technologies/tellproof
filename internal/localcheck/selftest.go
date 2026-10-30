package localcheck

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/dsse"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/enrol"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/evaluate"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/localreport"
)

// The self-test (D-057) asks whether this evidence and this policy could
// fail. For every objective it adds the least synthetic evidence that should
// fail it (an undeclared model, an unredacted personal-data span, a missed
// canary, ...) to a copy of the real input and evaluates again: a check that
// still passes cannot fail, and a pass from it means nothing. A register that
// covers every day or hour (a change logged daily, an incident always open)
// makes an objective vacuous, and the self-test says so. It then tampers with
// a copy of the log (an edited batch, a batch signed by another key, a
// missing first batch) and requires verification to refuse each one. The real
// log and policy are never changed.

// Self-test outcomes for one objective.
const (
	Falsifiable = "falsifiable" // the injected violation failed the objective
	Vacuous     = "vacuous"     // the registers leave no room for a violation
	Unfailable  = "unfailable"  // the violation was injected and the objective did not fail
	NoMutation  = "untested"    // no mutation is defined for this objective
)

// ObjectiveTest is one objective's self-test.
type ObjectiveTest struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Baseline string `json:"baseline"` // its status on the real evidence
	Injected string `json:"injected"` // what was added
	Status   string `json:"status"`   // its status with the injection
	Outcome  string `json:"outcome"`
	Note     string `json:"note,omitempty"`
}

// TamperTest is one tampering of a copy of the log.
type TamperTest struct {
	Name     string `json:"name"`
	Detected bool   `json:"detected"`
	Problem  string `json:"problem,omitempty"` // what verification said
}

// SelfTestReport is the self-test's outcome.
type SelfTestReport struct {
	AISystem   string          `json:"ai_system"`
	Period     evaluate.Period `json:"period"`
	Log        LogStatus       `json:"log"`
	Objectives []ObjectiveTest `json:"objectives"`
	Tamper     []TamperTest    `json:"tamper"`
	Limits     []string        `json:"limits"`
}

// Failed reports whether the self-test should fail a build: an objective that
// could not be made to fail, a vacuous one, or tampering that went unnoticed.
func (r *SelfTestReport) Failed() bool {
	for _, o := range r.Objectives {
		if o.Outcome == Unfailable || o.Outcome == Vacuous {
			return true
		}
	}
	for _, t := range r.Tamper {
		if !t.Detected {
			return true
		}
	}
	return false
}

// mutation adds a violation of one objective to in, or says why it cannot.
type mutation func(in *evaluate.Input, f *selfFacts) (what string, vacuous string)

// selfFacts is what the mutations size themselves by.
type selfFacts struct {
	start, end                                time.Time
	loc                                       *time.Location
	traced, billed, pii, llm, tools, canaries int64
	declaredModel, declaredRegion             map[string]bool
}

const selfTag = "tellproof-selftest" // names nothing real

func syntheticWindow(in *evaluate.Input, at time.Time) *batch.Metrics {
	w := evaluate.Window{Window: batch.Window{Start: at.UTC().Format(time.RFC3339), End: at.Add(time.Hour).UTC().Format(time.RFC3339)},
		Metrics:  batch.Metrics{Tokens: []batch.TokenCount{}, Evals: []batch.Eval{}, Transitions: []batch.Transition{}, Anomalies: []batch.Anomaly{}},
		Observed: batch.Observed{Models: []batch.ObservedModel{}, Versions: []batch.ObservedVersion{}}}
	in.Windows = append(in.Windows, w)
	return &in.Windows[len(in.Windows)-1].Metrics
}

// hundredfold is a count large enough to swamp the real ones.
func hundredfold(n int64) int64 { return 100 * (n + 1) }

var mutations = map[string]mutation{
	"EO-LOG-01": func(in *evaluate.Input, f *selfFacts) (string, string) {
		n := hundredfold(f.traced + f.billed)
		in.Usage = append(in.Usage, evaluate.UsageRow{Provider: selfTag, Model: selfTag, Start: f.start, InputTokens: n})
		return fmt.Sprintf("%d provider-billed tokens with no trace", n), ""
	},
	"EO-LIN-01": func(in *evaluate.Input, f *selfFacts) (string, string) {
		m := syntheticWindow(in, f.start)
		m.Tokens = append(m.Tokens, batch.TokenCount{Provider: selfTag, Model: selfTag, Region: anyDeclared(f.declaredRegion), Calls: 1})
		return "one call to an undeclared model", ""
	},
	"EO-CHG-01": func(in *evaluate.Input, f *selfFacts) (string, string) {
		add := func(at time.Time, kind string) {
			m := syntheticWindow(in, at)
			m.Transitions = append(m.Transitions, batch.Transition{At: at.UTC().Format(time.RFC3339), Kind: kind, From: selfTag + "-a", To: selfTag + "-b"})
		}
		for _, kind := range []string{"model", "prompt"} {
			if at, ok := unregisteredDay(in.Changes, kind, f); ok {
				add(at, kind)
				return "one unregistered " + kind + " change", ""
			}
		}
		// Every day is covered. If each entry doing the covering records a
		// change the telemetry saw, the register is honest and the period
		// short (an adopter's first days): show it can fail just after the
		// period, as EO-EVAL-01 does. Entries that match nothing seen are
		// what makes a register a rubber stamp.
		for _, kind := range []string{"model", "prompt"} {
			if onlyObservedChanges(in, kind, f) {
				at := f.end.Add(72 * time.Hour)
				in.System.Period.End = at.In(f.loc).Format("2006-01-02")
				add(at, kind)
				return "one unregistered " + kind + " change three days after the period (period stretched: every day of it is covered by registered changes the telemetry shows)", ""
			}
		}
		return "", "the change register has a model and a prompt change within a day of every day in the period, and entries that match no change in the telemetry, so no change can ever be unlogged"
	},
	"EO-PII-01": func(in *evaluate.Input, f *selfFacts) (string, string) {
		n := hundredfold(f.pii)
		syntheticWindow(in, f.start).PII = batch.PII{Spans: n}
		return fmt.Sprintf("%d personal-data spans without redaction", n), ""
	},
	"EO-CNT-01": func(in *evaluate.Input, f *selfFacts) (string, string) {
		syntheticWindow(in, f.start).ContentPresentSpans = 1
		return "one span carrying message content", ""
	},
	"EO-GRD-01": func(in *evaluate.Input, f *selfFacts) (string, string) {
		n := hundredfold(f.llm)
		syntheticWindow(in, f.start).Guardrails = batch.Guardrails{LLMCalls: n}
		return fmt.Sprintf("%d LLM calls without a guardrail decision", n), ""
	},
	"EO-EVAL-01": func(in *evaluate.Input, f *selfFacts) (string, string) {
		// A version released after the first window and live past the
		// freshness limit with no eval: the period is stretched to 40 days.
		end := f.start.In(f.loc).AddDate(0, 0, 40)
		in.System.Period.End = end.Format("2006-01-02")
		// The first window that observed anything decides what was already
		// live, so a baseline window comes first and the new version an hour later.
		syntheticWindow(in, f.start)
		in.Windows[len(in.Windows)-1].Observed.Models = []batch.ObservedModel{{Provider: selfTag, Model: selfTag + "-baseline"}}
		syntheticWindow(in, f.start.Add(time.Hour))
		w := &in.Windows[len(in.Windows)-1]
		w.Observed.Models = []batch.ObservedModel{{Provider: selfTag, Model: selfTag}}
		w.Observed.Versions = []batch.ObservedVersion{{Model: selfTag, PromptVersion: "1"}}
		return "a model and prompt version live for 40 days with no eval run (period stretched)", ""
	},
	"EO-INC-01": func(in *evaluate.Input, f *selfFacts) (string, string) {
		at, ok := hourWithoutIncident(in.Incidents, f)
		if !ok {
			return "", "the incident register has an incident opened within an hour of every hour in the period, so no anomaly can ever go unacknowledged"
		}
		m := syntheticWindow(in, at)
		m.Anomalies = append(m.Anomalies, batch.Anomaly{Start: at.UTC().Format(time.RFC3339), End: at.Add(time.Hour).UTC().Format(time.RFC3339), Kind: "error_rate"})
		return "one error-rate anomaly with no incident", ""
	},
	"EO-AGT-01": func(in *evaluate.Input, f *selfFacts) (string, string) {
		n := hundredfold(f.tools)
		syntheticWindow(in, f.start).Agent = batch.AgentCounts{ToolCalls: n, ToolErrors: n}
		return fmt.Sprintf("%d failed tool calls", n), ""
	},
	"EO-RES-01": func(in *evaluate.Input, f *selfFacts) (string, string) {
		m := syntheticWindow(in, f.start)
		m.Tokens = append(m.Tokens, batch.TokenCount{Provider: selfTag, Model: selfTag, Region: selfTag, Calls: 1})
		return "one call in an undeclared region", ""
	},
	"EO-CAN-01": func(in *evaluate.Input, f *selfFacts) (string, string) {
		n := hundredfold(f.canaries)
		syntheticWindow(in, f.start).Canaries = []batch.Canary{{Kind: "other", Run: n}}
		return fmt.Sprintf("%d guardrail canaries missed", n), ""
	},
}

func anyDeclared(m map[string]bool) string {
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}) {
		return k
	}
	return "unknown"
}

// unregisteredDay is a time in the period whose local day has no change of
// kind within a day either side (the evaluator's matching rule).
func unregisteredDay(changes []evaluate.Change, kind string, f *selfFacts) (time.Time, bool) {
	for d := f.start; d.Before(f.end); d = d.Add(24 * time.Hour) {
		day := d.In(f.loc)
		day0 := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
		covered := false
		for _, c := range changes {
			cd, err := time.Parse("2006-01-02", c.Date)
			if err == nil && c.Kind == kind && cd.Sub(day0) <= 24*time.Hour && day0.Sub(cd) <= 24*time.Hour {
				covered = true
				break
			}
		}
		if !covered {
			return d.Add(time.Hour), true
		}
	}
	return time.Time{}, false
}

// onlyObservedChanges says whether every register entry of kind that covers
// a day of the period matches a change of that kind in the telemetry, within
// a day (the evaluator's matching rule, by local date).
func onlyObservedChanges(in *evaluate.Input, kind string, f *selfFacts) bool {
	day := func(t time.Time) time.Time {
		l := t.In(f.loc)
		return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
	}
	near := func(a, b time.Time) bool { return a.Sub(b) <= 24*time.Hour && b.Sub(a) <= 24*time.Hour }
	var seen []time.Time
	for _, w := range in.Windows {
		for _, t := range w.Metrics.Transitions {
			if at, err := time.Parse(time.RFC3339, t.At); err == nil && t.Kind == kind {
				seen = append(seen, day(at))
			}
		}
	}
	slices.SortFunc(seen, func(a, b time.Time) int { return a.Compare(b) })
	var entries []time.Time
	first, last := day(f.start), day(f.end.Add(-time.Nanosecond))
	for _, c := range in.Changes {
		cd, err := time.Parse("2006-01-02", c.Date)
		if err == nil && c.Kind == kind && !cd.Before(first.Add(-24*time.Hour)) && !cd.After(last.Add(24*time.Hour)) {
			entries = append(entries, cd) // covers a day of the period
		}
	}
	slices.SortFunc(entries, func(a, b time.Time) int { return a.Compare(b) })
	// Each entry must record a distinct change seen: several entries around
	// one change are the blanket pattern.
	used := make([]bool, len(seen))
	for _, cd := range entries {
		i := slices.IndexFunc(seen, func(s time.Time) bool { return near(s, cd) })
		for i >= 0 && i < len(seen) && used[i] {
			i++
			if i < len(seen) && !near(seen[i], cd) {
				i = -1
			}
		}
		if i < 0 || i >= len(seen) {
			return false
		}
		used[i] = true
	}
	return len(entries) > 0
}

// hourWithoutIncident is an hour in the period with no incident opened
// within 60 minutes of it (the evaluator's matching rule).
func hourWithoutIncident(incidents []evaluate.Incident, f *selfFacts) (time.Time, bool) {
	var opened []time.Time
	for _, i := range incidents {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(i.Opened)); err == nil {
			opened = append(opened, t)
		}
	}
	for h := f.start; h.Before(f.end); h = h.Add(time.Hour) {
		near := false
		for _, t := range opened {
			if !t.Before(h.Add(-time.Hour)) && !t.After(h.Add(2*time.Hour)) {
				near = true
				break
			}
		}
		if !near {
			return h, true
		}
	}
	return time.Time{}, false
}

func clone(in evaluate.Input) evaluate.Input {
	out := in
	out.Windows = slices.Clone(in.Windows)
	out.Usage = slices.Clone(in.Usage)
	out.Changes = slices.Clone(in.Changes)
	out.Incidents = slices.Clone(in.Incidents)
	out.Evals = slices.Clone(in.Evals)
	return out
}

// SelfTest checks that the evidence and policy can fail, and that tampering
// with the log is detected.
func SelfTest(o Options) (*SelfTestReport, error) {
	r, in, err := prepare(o)
	if r == nil {
		return nil, err
	}
	st := &SelfTestReport{AISystem: r.AISystem, Period: r.Period, Log: r.Log, Objectives: []ObjectiveTest{}, Tamper: []TamperTest{},
		Limits: []string{
			"Deleting the newest batches cannot be detected from the log alone; the hosted service's chain and daily public checkpoints detect it.",
			"The self-test shows the objectives respond to violations in the evidence. It cannot show the telemetry itself is complete: that is what trace coverage (EO-LOG-01) and canaries (EO-CAN-01) measure.",
		}}
	if err != nil {
		return st, err
	}
	base, err := evaluate.Evaluate(in)
	if err != nil {
		return st, err
	}
	f, err := factsFor(in)
	if err != nil {
		return st, err
	}
	for _, b := range base.Objectives {
		t := ObjectiveTest{ID: b.ID, Title: b.Title, Baseline: b.Status}
		mut, ok := mutations[b.ID]
		if !ok {
			t.Outcome = NoMutation
			st.Objectives = append(st.Objectives, t)
			continue
		}
		mi := clone(in)
		what, vacuous := mut(&mi, f)
		if vacuous != "" {
			t.Outcome, t.Note = Vacuous, vacuous
			st.Objectives = append(st.Objectives, t)
			continue
		}
		ev, err := evaluate.Evaluate(mi)
		if err != nil {
			return st, fmt.Errorf("localcheck: self-test %s: %w", b.ID, err)
		}
		t.Injected, t.Outcome = what, Unfailable
		for _, res := range ev.Objectives {
			if res.ID == b.ID {
				t.Status = res.Status
				if res.Status == "fail" {
					t.Outcome = Falsifiable
				}
			}
		}
		st.Objectives = append(st.Objectives, t)
	}
	tamper, err := tamperTests(o.DataDir)
	st.Tamper = tamper
	return st, err
}

func factsFor(in evaluate.Input) (*selfFacts, error) {
	start, end, err := evaluate.PeriodBounds(in.System)
	if err != nil {
		return nil, err
	}
	loc, _ := time.LoadLocation(in.System.Timezone)
	f := &selfFacts{start: start, end: end, loc: loc, declaredModel: map[string]bool{}, declaredRegion: map[string]bool{}}
	for _, w := range in.Windows {
		m := w.Metrics
		for _, t := range m.Tokens {
			f.traced += t.InputTokens + t.OutputTokens
		}
		f.pii += m.PII.Spans
		f.llm += m.Guardrails.LLMCalls
		f.tools += m.Agent.ToolCalls
		for _, c := range m.Canaries {
			f.canaries += c.Run
		}
	}
	for _, u := range in.Usage {
		f.billed += u.InputTokens + u.OutputTokens
	}
	for _, d := range in.System.DeclaredModels {
		f.declaredModel[d.Provider+" "+d.Model] = true
	}
	for _, r := range in.System.DeclaredRegions {
		f.declaredRegion[r] = true
	}
	return f, nil
}

// tamperTests tampers with copies of the log in a temporary directory. The
// agent's private key is never copied.
func tamperTests(dataDir string) ([]TamperTest, error) {
	files, err := filepath.Glob(filepath.Join(dataDir, localreport.Dir, "*.batch.json"))
	if err != nil || len(files) == 0 {
		return nil, err
	}
	slices.Sort(files)
	cases := []struct {
		name string
		do   func(dir string, files []string) error
	}{
		{"one number edited in the newest batch", func(dir string, fs []string) error {
			return editEnvelope(fs[len(fs)-1], func(e *dsse.Envelope) error {
				i := bytes.IndexAny(e.Payload[bytes.Index(e.Payload, []byte(`"metrics"`))+1:], "0123456789")
				if i < 0 {
					return errors.New("no number to edit")
				}
				i += bytes.Index(e.Payload, []byte(`"metrics"`)) + 1
				e.Payload[i] = "1234567890"[e.Payload[i]-'0']
				return nil
			})
		}},
		{"the newest batch re-signed with another key", func(dir string, fs []string) error {
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			return editEnvelope(fs[len(fs)-1], func(e *dsse.Envelope) error {
				s, err := dsse.NewEd25519Signer(other, e.Signatures[0].KeyID)
				if err != nil {
					return err
				}
				re, err := dsse.Sign(context.Background(), s, e.PayloadType, e.Payload)
				if err != nil {
					return err
				}
				*e = *re
				return nil
			})
		}},
		{"the first batch removed", func(dir string, fs []string) error { return os.Remove(fs[0]) }},
	}
	var out []TamperTest
	for _, c := range cases {
		dir, err := os.MkdirTemp("", "tellproof-selftest-")
		if err != nil {
			return out, err
		}
		copied, err := copyLog(dataDir, dir, files)
		if err == nil {
			err = c.do(dir, copied)
		}
		var sum *localreport.Summary
		if err == nil {
			sum, err = localreport.Read(dir, localreport.Range{})
		}
		_ = os.RemoveAll(dir)
		if err != nil {
			return out, fmt.Errorf("localcheck: self-test %q: %w", c.name, err)
		}
		out = append(out, TamperTest{Name: c.name, Detected: !sum.Verified, Problem: sum.Problem})
	}
	return out, nil
}

func copyLog(from, to string, files []string) ([]string, error) {
	for _, name := range []string{enrol.IdentityFile, enrol.PublicKeyFile} {
		if err := copyFile(filepath.Join(from, name), filepath.Join(to, name)); err != nil {
			return nil, err
		}
	}
	if err := os.Mkdir(filepath.Join(to, localreport.Dir), 0o700); err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		dst := filepath.Join(to, localreport.Dir, filepath.Base(f))
		if err := copyFile(f, dst); err != nil {
			return nil, err
		}
		out = append(out, dst)
	}
	return out, nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// editEnvelope edits the envelope in a queue file ({"state", "envelope"}),
// keeping the rest as it is.
func editEnvelope(path string, edit func(*dsse.Envelope) error) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(b, &file); err != nil {
		return err
	}
	e, err := dsse.Decode(file["envelope"])
	if err != nil {
		return err
	}
	if err := edit(e); err != nil {
		return err
	}
	if file["envelope"], err = json.Marshal(e); err != nil {
		return err
	}
	out, err := json.Marshal(file)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

var outcomeMarks = map[string]string{Falsifiable: "✓ CAN FAIL", Vacuous: "✕ VACUOUS", Unfailable: "✕ CANNOT FAIL", NoMutation: "- UNTESTED"}

// WriteSelfTestText writes the self-test for a person.
func WriteSelfTestText(w io.Writer, r *SelfTestReport) error {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	p("Tellproof self-test: %s, %s to %s", r.AISystem, r.Period.Start, r.Period.End)
	p("  Can each objective fail on this evidence and policy? A violation is added to a copy of the evidence; the objective must fail.")
	p("")
	n := map[string]int{}
	for _, o := range r.Objectives {
		n[o.Outcome]++
		p("%-14s %-10s %s (now %s)", outcomeMarks[o.Outcome], o.ID, o.Title, o.Baseline)
		switch o.Outcome {
		case Vacuous:
			p("               %s", o.Note)
		case NoMutation:
			p("               no violation is defined for this objective yet")
		default:
			p("               added %s: %s", o.Injected, o.Status)
		}
	}
	p("")
	p("Is tampering with the log detected?")
	for _, t := range r.Tamper {
		mark := "✓ DETECTED"
		if !t.Detected {
			mark = "✕ MISSED"
		}
		p("%-14s %s", mark, t.Name)
		if t.Problem != "" {
			p("               %s", t.Problem)
		}
	}
	p("")
	for _, l := range r.Limits {
		p("Limit: %s", l)
	}
	p("")
	p("%d of %d objectives can fail; %d vacuous, %d cannot fail, %d untested.", n[Falsifiable], len(r.Objectives), n[Vacuous], n[Unfailable], n[NoMutation])
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteSelfTestJSON writes the self-test as JSON.
func WriteSelfTestJSON(w io.Writer, r *SelfTestReport) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}
