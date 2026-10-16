package aggregate

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/otlp"
)

var update = flag.Bool("update", false, "rewrite sdk/testdata/expected-windows.json")

const sdkData = "../../sdk/testdata"

// sdkWindow is what every SDK's scenario export must aggregate to.
type sdkWindow struct {
	Window   map[string]any `json:"window"`
	Metrics  any            `json:"metrics"`
	Observed any            `json:"observed"`
	Traces   int            `json:"traces"`
}

func aggregateFile(t *testing.T, path string) []sdkWindow {
	t.Helper()
	m, err := otlp.DefaultMapping()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var spans []otlp.Span
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		res, err := otlp.Parse(line, m)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if res.Rejected != 0 {
			t.Errorf("%s: %d spans rejected", path, res.Rejected)
		}
		spans = append(spans, res.Spans...)
	}
	a := New(DefaultConfig())
	a.Add(spans, spans[len(spans)-1].Start)
	var out []sdkWindow
	for _, w := range a.FlushAll() {
		out = append(out, sdkWindow{Window: map[string]any{"start": w.Window.Start, "end": w.Window.End},
			Metrics: toMap(t, w.Metrics), Observed: toMap(t, w.Observed), Traces: len(w.TraceIDs)})
	}
	return out
}

// Acceptance (S-1, S-3): the shared scenario, exported by each SDK, parses
// without rejections and aggregates to the expected windows; the concierge
// ingest agrees.
func TestSDKScenarioExports(t *testing.T) {
	expectedFile := filepath.Join(sdkData, "expected-windows.json")
	exports, _ := filepath.Glob(filepath.Join(sdkData, "*-export.jsonl"))
	if len(exports) == 0 {
		t.Fatal("no SDK exports in sdk/testdata")
	}
	if *update {
		b, _ := json.MarshalIndent(aggregateFile(t, filepath.Join(sdkData, "python-export.jsonl")), "", "  ")
		if err := os.WriteFile(expectedFile, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(expectedFile)
	if err != nil {
		t.Fatal(err)
	}
	var want []sdkWindow
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	for _, path := range exports {
		got := aggregateFile(t, path)
		if g, w := toMap(t, got), toMap(t, want); !reflect.DeepEqual(g, w) {
			gb, _ := json.MarshalIndent(got, "", " ")
			t.Errorf("%s aggregates to\n%s\nwant sdk/testdata/expected-windows.json", filepath.Base(path), gb)
		}
	}

	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	for _, path := range exports {
		rel := "sdk/testdata/" + filepath.Base(path)
		script := "import sys, json; sys.path.insert(0, 'concierge'); import ingest; " +
			"out, _ = ingest.ingest('otlp', ['" + rel + "'], b'parity'); " +
			"print(json.dumps([{'window': w['window'], 'metrics': w['metrics'], 'observed': w['observed'], 'traces': w['commit']['leaves']} for w in out['windows']]))"
		cmd := exec.Command(py, "-c", script)
		cmd.Dir = filepath.Join("..", "..")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("concierge ingest of %s: %v", rel, err)
		}
		var concierge []sdkWindow
		if err := json.Unmarshal(out, &concierge); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(toMap(t, concierge), toMap(t, want)) {
			t.Errorf("concierge ingest of %s differs:\n%s", rel, out)
		}
	}
}
