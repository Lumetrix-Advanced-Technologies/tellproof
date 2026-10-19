package langfuse

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/aggregate"
)

// Acceptance (P8-3): Langfuse observations and scores stuffed with the leak
// corpus, in every field Langfuse has, give spans and evaluations with none
// of it, and a malformed page that quotes it gives an error without it.
func TestNothingOfTheLeakCorpusComesThroughLangfuse(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "spec", "leak-corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Markers           []struct{ Kind, Needle, Text string } `json:"markers"`
		ContentAttributes []string                              `json:"content_attributes"`
		FactAttributes    []string                              `json:"fact_attributes"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	var obs, scores []any
	for i, m := range c.Markers {
		attrs := map[string]any{}
		for _, a := range append(append([]string{}, c.ContentAttributes...), c.FactAttributes...) {
			if a != "gen_ai.operation.name" && a != "llm.request.type" { // keep it a generation, so its score counts
				attrs[a] = m.Text
			}
		}
		traceID := strings.Repeat("0", 30) + string("0123456789abcdef"[i%16]) + "1"
		obs = append(obs, map[string]any{
			"id": "obs-" + m.Kind, "traceId": traceID, "startTime": "2026-09-01T00:10:00.000Z", "type": "GENERATION",
			"name": m.Text, "level": "ERROR", "statusMessage": m.Text, "providedModelName": m.Text, "promptName": m.Text, "promptVersion": 3,
			"inputUsage": 10, "outputUsage": 5, "input": map[string]any{"messages": []any{m.Text}}, "output": m.Text,
			"metadata": map[string]any{"attributes": attrs, "resourceAttributes": map[string]any{"service.name": m.Text, "gen_ai.prompt": m.Text},
				"provider": m.Text, "region": m.Text, "user_note": m.Text},
			"userId": m.Text, "sessionId": m.Text, "tags": []any{m.Text},
		})
		scores = append(scores, map[string]any{"name": "triage-eval", "value": 1, "dataType": "NUMERIC", "timestamp": "2026-09-01T00:20:00.000Z",
			"comment": m.Text, "subject": map[string]any{"kind": "observation", "id": "obs-" + m.Kind, "traceId": traceID}})
	}
	page := func(data []any) []byte {
		b, _ := json.Marshal(map[string]any{"data": data, "meta": map[string]any{"cursor": nil}})
		return b
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "scores") {
			_, _ = w.Write(page(scores))
		} else {
			_, _ = w.Write(page(obs))
		}
	}))
	defer srv.Close()
	p, _ := newPuller(t, srv)
	res, err := p.Pull(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Spans) != len(c.Markers) || len(res.Evals) == 0 {
		t.Fatalf("%d spans, %d evals", len(res.Spans), len(res.Evals))
	}
	for i, s := range res.Spans {
		if !s.ContentPresent || s.Model != "invalid" || s.Provider != "invalid" || s.Region != "invalid" {
			t.Errorf("span %d: %+v", i, s)
		}
	}
	// What a batch would carry: the windows' metrics and observed models and
	// versions, and the evaluations. (The span's guardrail decision, prompt
	// name and slot are compared and used as keys inside the agent, never
	// written; its trace ID becomes a salted leaf.)
	agg := aggregate.New(aggregate.DefaultConfig())
	agg.Add(res.Spans, to)
	var windows []any
	for _, w := range agg.FlushAll() {
		windows = append(windows, w.Metrics, w.Observed)
	}
	out, _ := json.Marshal(map[string]any{"windows": windows, "evals": res.Evals})
	for _, m := range c.Markers {
		if strings.Contains(strings.ToLower(string(out)), strings.ToLower(m.Needle)) {
			t.Errorf("the %s marker %q came through Langfuse: %s", m.Kind, m.Needle, out)
		}
	}

	// A page Langfuse garbles, with the content in it.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [{"id": "` + c.Markers[0].Text + `", "startTime": "` + c.Markers[1].Text + `"}`))
	}))
	defer bad.Close()
	p2, _ := newPuller(t, bad)
	_, err = p2.Pull(context.Background(), from, to)
	if err == nil {
		t.Fatal("a garbled page was accepted")
	}
	for _, m := range c.Markers[:2] {
		if strings.Contains(err.Error(), m.Needle) {
			t.Errorf("the error quotes the page: %v", err)
		}
	}
}
