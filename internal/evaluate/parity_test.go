package evaluate

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
)

var root = filepath.Join("..", "..")

// Concierge input files, read as the concierge reads them.

func readCSV(t *testing.T, path string) []map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for _, r := range recs[1:] {
		row := map[string]string{}
		for i, h := range recs[0] {
			row[h] = strings.TrimSpace(r[i])
		}
		out = append(out, row)
	}
	return out
}

func atoi(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

type conciergeFiles struct {
	system, aggregates, usage, changes, incidents, evals string // "" when absent
}

func load(t *testing.T, fs conciergeFiles) Input {
	t.Helper()
	var in Input
	b, err := os.ReadFile(fs.system)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &in.System); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(fs.aggregates)
	var agg struct {
		Windows []Window `json:"windows"`
	}
	if err := json.Unmarshal(b, &agg); err != nil {
		t.Fatal(err)
	}
	in.Windows = agg.Windows
	for _, u := range readCSV(t, fs.usage) {
		start, err := time.Parse(time.RFC3339, u["start"])
		if err != nil {
			t.Fatal(err)
		}
		row := UsageRow{Provider: u["provider"], Model: u["model"], Region: u["region"], Start: start,
			InputTokens: atoi(t, u["input_tokens"]), OutputTokens: atoi(t, u["output_tokens"])}
		if u["requests"] != "" {
			r := atoi(t, u["requests"])
			row.Requests = &r
		}
		in.Usage = append(in.Usage, row)
	}
	if fs.changes != "" {
		for _, c := range readCSV(t, fs.changes) {
			in.Changes = append(in.Changes, Change{Date: c["date"], Kind: c["kind"], What: c["what"]})
		}
	}
	if fs.incidents != "" {
		for _, i := range readCSV(t, fs.incidents) {
			in.Incidents = append(in.Incidents, Incident{ID: i["id"], Opened: i["opened"], Acknowledged: i["acknowledged"], Resolved: i["resolved"], Summary: i["summary"]})
		}
	}
	if fs.evals != "" {
		for _, e := range readCSV(t, fs.evals) {
			rate, _ := strconv.ParseFloat(e["pass_rate"], 64)
			in.Evals = append(in.Evals, batch.Eval{Suite: e["suite"], Model: e["model"], PromptVersion: e["prompt_version"], RanAt: e["ran_at"], PassRate: rate})
		}
	}
	return in
}

func python(t *testing.T) string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	return py
}

// concierge runs concierge/evaluate.py on the same files.
func concierge(t *testing.T, fs conciergeFiles) any {
	t.Helper()
	abs := func(p string) any {
		if p == "" {
			return nil
		}
		a, _ := filepath.Abs(p)
		return a
	}
	args, _ := json.Marshal(map[string]any{"system_path": abs(fs.system), "aggregate_paths": []any{abs(fs.aggregates)},
		"usage_paths": []any{abs(fs.usage)}, "changes_path": abs(fs.changes), "incidents_path": abs(fs.incidents), "evals_path": abs(fs.evals)})
	script := "import sys, json; sys.path.insert(0, 'concierge'); import evaluate; " +
		"ev = evaluate.evaluate(**json.loads(sys.argv[1])); ev.pop('inputs'); ev.pop('schema'); print(json.dumps(ev))"
	cmd := exec.Command(python(t), "-c", script, string(args))
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("concierge evaluate: %v\n%s", err, ee.Stderr)
		}
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func asJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func compare(t *testing.T, name string, fs conciergeFiles) *Evaluation {
	t.Helper()
	want := concierge(t, fs)
	ev, err := Evaluate(load(t, fs))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	got := asJSON(t, ev)
	if !reflect.DeepEqual(got, want) {
		if d := os.Getenv("PARITY_DUMP"); d != "" {
			gb, _ := json.MarshalIndent(got, "", " ")
			wb, _ := json.MarshalIndent(want, "", " ")
			_ = os.WriteFile(filepath.Join(d, name+".go.json"), gb, 0o644)
			_ = os.WriteFile(filepath.Join(d, name+".py.json"), wb, 0o644)
		}
		g, w := got.(map[string]any), want.(map[string]any)
		for k := range w {
			if !reflect.DeepEqual(g[k], w[k]) {
				gb, _ := json.MarshalIndent(g[k], "", " ")
				wb, _ := json.MarshalIndent(w[k], "", " ")
				t.Errorf("%s: %q differs\n go: %s\n py: %s", name, k, gb, wb)
			}
		}
		for k := range g {
			if _, ok := w[k]; !ok {
				t.Errorf("%s: Go has %q, the concierge does not", name, k)
			}
		}
	}
	return ev
}

var month = conciergeFiles{
	system:     filepath.Join(root, "concierge/testdata/partner-month/system.json"),
	aggregates: filepath.Join(root, "concierge/testdata/partner-month/aggregates.json"),
	usage:      filepath.Join(root, "concierge/testdata/partner-month/usage.csv"),
	changes:    filepath.Join(root, "concierge/testdata/partner-month/changes.csv"),
	incidents:  filepath.Join(root, "concierge/testdata/partner-month/incidents.csv"),
	evals:      filepath.Join(root, "concierge/testdata/partner-month/evals.csv"),
}

// Acceptance (P4-4): the Go evaluator matches the concierge on the fixtures.
func TestParityPartnerMonth(t *testing.T) {
	ev := compare(t, "partner month", month)
	// Spot-check against the concierge test's hand calculation.
	got := map[string]string{}
	for _, o := range ev.Objectives {
		got[o.ID] = o.Status
	}
	want := map[string]string{"EO-LOG-01": "attention", "EO-LIN-01": "pass", "EO-CHG-01": "fail", "EO-PII-01": "pass", "EO-CNT-01": "fail",
		"EO-GRD-01": "pass", "EO-EVAL-01": "attention", "EO-INC-01": "fail", "EO-AGT-01": "attention", "EO-RES-01": "fail", "EO-CAN-01": "attention"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("statuses %v", got)
	}
}

func rewrite(t *testing.T, dir, name string, src string, edit func([]byte) []byte) string {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, edit(b), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func editAggregates(t *testing.T, edit func(doc map[string]any)) func([]byte) []byte {
	return func(b []byte) []byte {
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		edit(doc)
		out, _ := json.Marshal(doc)
		return out
	}
}

func TestParityVariants(t *testing.T) {
	dir := t.TempDir()
	windows := func(doc map[string]any) []any { return doc["windows"].([]any) }

	// Prompt v15 live from day 2: eval freshness fails.
	late := month
	late.aggregates = rewrite(t, dir, "late.json", month.aggregates, editAggregates(t, func(doc map[string]any) {
		windows(doc)[2].(map[string]any)["window"] = map[string]any{"start": "2026-09-02T05:00:00Z", "end": "2026-09-02T06:00:00Z"}
	}))
	compare(t, "late release", late)

	// Agent-shaped data (D-028, D-040): OpenAI spans without a region and usage
	// without one, a blocked call without a model, a window with models but no versions.
	agentShaped := month
	agentShaped.aggregates = rewrite(t, dir, "agent.json", month.aggregates, editAggregates(t, func(doc map[string]any) {
		for _, w := range windows(doc) {
			for _, tok := range w.(map[string]any)["metrics"].(map[string]any)["tokens"].([]any) {
				if tm := tok.(map[string]any); tm["provider"] == "openai" {
					tm["region"] = "unknown"
				}
			}
		}
		w0 := windows(doc)[0].(map[string]any)["metrics"].(map[string]any)
		w0["tokens"] = append(w0["tokens"].([]any), map[string]any{"provider": "openai", "model": "unknown", "region": "unknown",
			"input_tokens": 0, "output_tokens": 0, "calls": 3})
		windows(doc)[1].(map[string]any)["observed"].(map[string]any)["versions"] = []any{}
	}))
	agentShaped.usage = rewrite(t, dir, "usage.csv", month.usage, func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), "openai,text-embedding-3-small,us-east-1,", "openai,text-embedding-3-small,,"))
	})
	ev := compare(t, "agent-shaped", agentShaped)
	for _, o := range ev.Objectives {
		if (o.ID == "EO-RES-01" || o.ID == "EO-LIN-01") && o.Status != "pass" {
			t.Errorf("%s = %s with only unknown and declared values", o.ID, o.Status)
		}
	}

	// A first window that observed nothing (usage or eval runs only) does not
	// decide which versions were live before the period (D-040).
	quiet := month
	quiet.aggregates = rewrite(t, dir, "quiet.json", month.aggregates, editAggregates(t, func(doc map[string]any) {
		doc["windows"] = append([]any{map[string]any{
			"window": map[string]any{"start": "2026-09-01T00:00:00Z", "end": "2026-09-01T01:00:00Z"},
			"metrics": map[string]any{"tokens": []any{}, "pii": map[string]any{"pii_spans": 0, "pii_spans_redacted": 0}, "content_present_spans": 0,
				"guardrails": map[string]any{"llm_calls": 0, "guarded_calls": 0, "blocked": 0}, "evals": []any{},
				"agent":       map[string]any{"runs": 0, "tool_calls": 0, "tool_errors": 0, "high_impact_calls": 0, "high_impact_approved": 0, "escalations": 0},
				"transitions": []any{}, "anomalies": []any{}, "late_spans": 0},
			"observed": map[string]any{"models": []any{}, "versions": []any{}},
			"commit":   map[string]any{"root": strings.Repeat("0", 64), "leaves": 0}}}, windows(doc)...)
	}))
	ev = compare(t, "quiet first window", quiet)
	for _, o := range ev.Objectives {
		if o.ID == "EO-EVAL-01" && o.Status != "attention" {
			t.Errorf("EO-EVAL-01 = %s after an empty first window (want attention, as without it)", o.Status)
		}
	}

	// Guardrail canaries (D-057): all caught passes EO-CAN-01; one miss fails it
	// and names the kind; windows without canaries add nothing.
	withCanaries := func(name string, missed int) *Evaluation {
		c := month
		c.aggregates = rewrite(t, dir, name+".json", month.aggregates, editAggregates(t, func(doc map[string]any) {
			windows(doc)[0].(map[string]any)["metrics"].(map[string]any)["canaries"] = []any{
				map[string]any{"kind": "pii", "run": 4, "caught": 4},
				map[string]any{"kind": "prompt_injection", "run": 4, "caught": 4 - missed}}
			windows(doc)[1].(map[string]any)["metrics"].(map[string]any)["canaries"] = []any{
				map[string]any{"kind": "prompt_injection", "run": 2, "caught": 2}}
		}))
		return compare(t, name, c)
	}
	for _, tc := range []struct {
		name     string
		missed   int
		status   string
		measured string
	}{
		{"canaries caught", 0, "pass", "10 of 10 canaries caught"},
		{"canary missed", 1, "fail", "9 of 10 canaries caught; missed: prompt_injection (1)"},
	} {
		for _, o := range withCanaries(tc.name, tc.missed).Objectives {
			if o.ID == "EO-CAN-01" && (o.Status != tc.status || o.Measured != tc.measured) {
				t.Errorf("%s: EO-CAN-01 = %s, %q", tc.name, o.Status, o.Measured)
			}
		}
	}

	// No registers, no evals, nothing in the period but one window.
	bare := conciergeFiles{system: month.system, usage: month.usage}
	bare.aggregates = rewrite(t, dir, "bare.json", month.aggregates, editAggregates(t, func(doc map[string]any) {
		doc["windows"] = windows(doc)[:1]
	}))
	compare(t, "bare", bare)
}

// The demo month: the sample OTLP export ingested by the concierge, then evaluated by both.
func TestParityDemoMonth(t *testing.T) {
	dir := t.TempDir()
	agg := filepath.Join(dir, "aggregates.json")
	script := "import sys, json; sys.path.insert(0, 'concierge'); import ingest; " +
		"out, _ = ingest.ingest('otlp', ['concierge/testdata/otlp-sample.jsonl'], b'parity'); open(sys.argv[1], 'w').write(json.dumps(out))"
	cmd := exec.Command(python(t), "-c", script, agg)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ingest: %v %s", err, out)
	}
	compare(t, "demo", conciergeFiles{system: filepath.Join(root, "concierge/testdata/demo/system.json"), aggregates: agg,
		usage: filepath.Join(root, "concierge/testdata/demo/usage.csv"), changes: filepath.Join(root, "concierge/testdata/demo/changes.csv")})
}

func TestFormatsLikePython(t *testing.T) {
	hundred := 100.0 // at run time, as Python computes threshold * 100 (constants would fold exactly)
	for _, c := range []struct {
		x    float64
		want string
	}{{98, "98"}, {99.5, "99.5"}, {100, "100"}, {0.9 * hundred, "90"}, {0.995 * hundred, "99.5"}, {0.01 * hundred, "1"}, {0.1, "0.1"}, {1234567, "1.23457e+06"}} {
		if got := g(c.x); got != c.want {
			t.Errorf("g(%v) = %q, want %q", c.x, got, c.want)
		}
	}
	for x, want := range map[float64]float64{0.25: 0.2, 0.35: 0.3, 39.39393939: 39.4, 24.242424: 24.2, 2.675: 2.7} {
		if got := round1(x); got != want {
			t.Errorf("round1(%v) = %v, want %v", x, got, want)
		}
	}
	if pctFloor(1999, 2000, 1) != 99.9 || pctText(3, 1000, 2) != "0.30%" {
		t.Errorf("pctFloor/pctText")
	}
	if days(-time.Hour) != -1 || days(47*time.Hour) != 1 {
		t.Error("days must floor like timedelta.days")
	}
}
