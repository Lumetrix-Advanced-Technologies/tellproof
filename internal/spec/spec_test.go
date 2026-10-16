package spec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readSpec(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "spec", name))
	if err != nil {
		t.Fatalf("read spec file: %v", err)
	}
	return string(b)
}

func TestSpecFilesAreValid(t *testing.T) {
	err := Check(
		[]byte(readSpec(t, "objectives.json")),
		[]byte(readSpec(t, "framework-refs.json")),
		[]byte(readSpec(t, "semconv-map.json")),
	)
	if err != nil {
		t.Fatalf("spec files do not validate:\n%v", err)
	}
}

// Each case breaks one real spec file with a single string replacement and
// expects Check to fail with a message that names the problem.
func TestBrokenSpecFails(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		old     string
		new     string
		suffix  string // appended to the file after the replacement
		wantErr string
	}{
		{
			name:    "maps_to entry missing from framework refs",
			file:    "objectives.json",
			old:     `"iso-42001/A.6.2.8"`,
			new:     `"iso-42001/A.9.9.9"`,
			wantErr: `objectives.json: EO-LOG-01: maps_to "iso-42001/A.9.9.9" is not in framework-refs.json`,
		},
		{
			name:    "framework ref renamed under an objective",
			file:    "framework-refs.json",
			old:     `"id": "aiuc-1/E",`,
			new:     `"id": "aiuc-1/EE",`,
			wantErr: `objectives.json: EO-LOG-01: maps_to "aiuc-1/E" is not in framework-refs.json`,
		},
		{
			name:    "unknown rule kind",
			file:    "objectives.json",
			old:     `"kind": "zero_count"`,
			new:     `"kind": "zero_counts"`,
			wantErr: `objectives.json: EO-CHG-01: rule kind "zero_counts" is not one of ratio_at_least, max_value, zero_count, freshness_within, all_declared`,
		},
		{
			name:    "rule_kinds list differs from the five",
			file:    "objectives.json",
			old:     `"max_value",`,
			new:     `"max_values",`,
			wantErr: `objectives.json: rule_kinds must be exactly ratio_at_least, max_value, zero_count, freshness_within, all_declared`,
		},
		{
			name:    "rule missing a field its kind requires",
			file:    "objectives.json",
			old:     `"denominator": "billed_tokens",`,
			new:     ``,
			wantErr: `objectives.json: EO-LOG-01: rule kind "ratio_at_least" requires "denominator"`,
		},
		{
			name:    "rule has a field its kind does not take",
			file:    "objectives.json",
			old:     `"metric": "unlogged_transitions"`,
			new:     `"metric": "unlogged_transitions", "threshold": 0`,
			wantErr: `objectives.json: EO-CHG-01: rule kind "zero_count" does not take "threshold"`,
		},
		{
			name:    "duplicate objective id",
			file:    "objectives.json",
			old:     `"id": "EO-LIN-01"`,
			new:     `"id": "EO-LOG-01"`,
			wantErr: `objectives.json: duplicate objective id "EO-LOG-01"`,
		},
		{
			name:    "unknown field on an objective",
			file:    "objectives.json",
			old:     `"scope": "production",`,
			new:     `"scope": "production", "owner": "x",`,
			wantErr: `objectives.json: json: unknown field "owner"`,
		},
		{
			name:    "unknown field on a rule",
			file:    "objectives.json",
			old:     `"kind": "ratio_at_least",`,
			new:     `"kind": "ratio_at_least", "floor": 0.5,`,
			wantErr: `objectives.json: json: unknown field "floor"`,
		},
		{
			name:    "trailing data after the document",
			file:    "objectives.json",
			old:     `"version": 1,`,
			new:     `"version": 1,`,
			suffix:  "\n{}\n",
			wantErr: `objectives.json: unexpected data after the JSON document`,
		},
		{
			name:    "duplicate framework ref id",
			file:    "framework-refs.json",
			old:     `"id": "eu-ai-act/art-26-6"`,
			new:     `"id": "eu-ai-act/art-12"`,
			wantErr: `framework-refs.json: duplicate ref id "eu-ai-act/art-12"`,
		},
		{
			name:    "framework ref title too long to be our own short title",
			file:    "framework-refs.json",
			old:     `"title": "Deployers keep logs"`,
			new:     `"title": "This title is deliberately long, like pasted control text, not our own"`,
			wantErr: `framework-refs.json: eu-ai-act/art-26-6: title is 70 characters; keep our own short title (at most 60)`,
		},
		{
			name:    "unknown field on a framework ref",
			file:    "framework-refs.json",
			old:     `"verified": true`,
			new:     `"verified": true, "text": "x"`,
			wantErr: `framework-refs.json: json: unknown field "text"`,
		},
		{
			name:    "wrong type on a framework ref",
			file:    "framework-refs.json",
			old:     `"verified": true`,
			new:     `"verified": "yes"`,
			wantErr: `framework-refs.json: json: cannot unmarshal string`,
		},
		{
			name:    "semconv attribute mapped to two fields",
			file:    "semconv-map.json",
			old:     `"gen_ai.request.model"`,
			new:     `"gen_ai.response.model"`,
			wantErr: `semconv-map.json: attribute "gen_ai.response.model" is mapped to both "request_model" and "response_model"`,
		},
		{
			name:    "presence-only content attribute also mapped to a field",
			file:    "semconv-map.json",
			old:     `"gen_ai.prompt",`,
			new:     `"gen_ai.usage.input_tokens",`,
			wantErr: `semconv-map.json: presence-only attribute "gen_ai.usage.input_tokens" is also mapped to field "input_tokens"; content values must never be read`,
		},
		{
			name:    "mapped attribute under a content prefix",
			file:    "semconv-map.json",
			old:     `"gen_ai.request.model"`,
			new:     `"gen_ai.prompt.model"`,
			wantErr: `semconv-map.json: attribute "gen_ai.prompt.model" of field "request_model" falls under presence-only prefix "gen_ai.prompt."; content values must never be read`,
		},
		{
			name:    "content prefix without a trailing dot",
			file:    "semconv-map.json",
			old:     `"llm.prompts."`,
			new:     `"llm.prompts"`,
			wantErr: `semconv-map.json: presence-only prefix "llm.prompts" must be a name followed by a dot`,
		},
		{
			name:    "unknown field in semconv map",
			file:    "semconv-map.json",
			old:     `"version": "v2",`,
			new:     `"version": "v2", "extra": 1,`,
			wantErr: `semconv-map.json: json: unknown field "extra"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{
				"objectives.json":     readSpec(t, "objectives.json"),
				"framework-refs.json": readSpec(t, "framework-refs.json"),
				"semconv-map.json":    readSpec(t, "semconv-map.json"),
			}
			src, ok := files[tt.file]
			if !ok {
				t.Fatalf("unknown spec file %q", tt.file)
			}
			if !strings.Contains(src, tt.old) {
				t.Fatalf("%s does not contain %q; update the test case", tt.file, tt.old)
			}
			src = strings.Replace(src, tt.old, tt.new, 1) + tt.suffix
			files[tt.file] = src

			err := Check([]byte(files["objectives.json"]), []byte(files["framework-refs.json"]), []byte(files["semconv-map.json"]))
			if err == nil {
				t.Fatalf("Check passed; want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Check error =\n%v\nwant it to contain\n%s", err, tt.wantErr)
			}
		})
	}
}

// P3-0: batch schema v0.2 adds "observed" and changes the version const;
// everything else must be exactly as in v0.1 (kept as batch-v0.1.schema.json).
func TestBatchSchemaV02OnlyAddsObserved(t *testing.T) {
	load := func(name string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(readSpec(t, name)), &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return m
	}
	v01, v02 := load("batch-v0.1.schema.json"), load("batch.schema.json")
	props := func(m map[string]any) map[string]any { return m["properties"].(map[string]any) }
	if got := props(v01)["schema"].(map[string]any)["const"]; got != "plumbline-batch/v0.1" {
		t.Errorf("v0.1 schema const = %v", got)
	}
	if got := props(v02)["schema"].(map[string]any)["const"]; got != "plumbline-batch/v0.2" {
		t.Errorf("v0.2 schema const = %v", got)
	}
	obs, ok := props(v02)["observed"].(map[string]any)
	if !ok {
		t.Fatal("v0.2 has no observed property")
	}
	if req := obs["required"].([]any); len(req) != 2 || req[0] != "models" || req[1] != "versions" {
		t.Errorf("observed.required = %v", req)
	}
	for name, p := range props(v01) {
		if name == "schema" {
			continue
		}
		a, _ := json.Marshal(p)
		b, _ := json.Marshal(props(v02)[name])
		if string(a) != string(b) {
			t.Errorf("property %q changed between v0.1 and v0.2", name)
		}
	}
	if len(props(v02)) != len(props(v01))+1 {
		t.Errorf("v0.2 has %d properties, want v0.1's %d plus observed", len(props(v02)), len(props(v01)))
	}
	reqs := func(m map[string]any) []any { return m["required"].([]any) }
	if len(reqs(v02)) != len(reqs(v01))+1 {
		t.Errorf("v0.2 required = %v", reqs(v02))
	}
}
