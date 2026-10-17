package langfuse

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/aggregate"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/otlp"
)

var (
	from = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to   = from.Add(2 * time.Hour)
)

type recorded struct {
	t        *testing.T
	pages    map[string]string // path + "|" + cursor -> fixture file
	requests []*http.Request
	fail     []int // status codes to return first, in order
}

func (r *recorded) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.requests = append(r.requests, req.Clone(context.Background()))
	if len(r.fail) > 0 {
		code := r.fail[0]
		r.fail = r.fail[1:]
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "2")
		}
		w.WriteHeader(code)
		return
	}
	name, ok := r.pages[req.URL.Path+"|"+req.URL.Query().Get("cursor")]
	if !ok {
		r.t.Errorf("unexpected request %s", req.URL)
		http.NotFound(w, req)
		return
	}
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		r.t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func newRecorded(t *testing.T) *recorded {
	return &recorded{t: t, pages: map[string]string{
		"/api/public/v2/observations|":         "observations-1.json",
		"/api/public/v2/observations|cursor-2": "observations-2.json",
		"/api/public/v2/observations|cursor-3": "observations-3.json",
		"/api/public/v3/scores|":               "scores-1.json",
		"/api/public/v3/scores|s2":             "scores-2.json",
	}}
}

func newPuller(t *testing.T, srv *httptest.Server) (*Puller, *[]time.Duration) {
	t.Helper()
	m, err := otlp.DefaultMapping()
	if err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	return &Puller{BaseURL: srv.URL, PublicKey: "pk-lf-test", SecretKey: "sk-lf-SECRET", HTTP: srv.Client(), Mapping: m,
		EvalScores: []EvalScore{{Name: "triage-eval", Suite: "triage-eval", PassAtLeast: 0.9}},
		Sleep:      func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }}, &slept
}

func otlpSpans(t *testing.T) []otlp.Span {
	t.Helper()
	m, _ := otlp.DefaultMapping()
	b, err := os.ReadFile(filepath.Join("..", "..", "concierge", "testdata", "otlp-sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []otlp.Span
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			res, err := otlp.Parse(line, m)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, res.Spans...)
		}
	}
	return out
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Acceptance: recorded responses produce the same aggregates as the OTLP path
// for the same traces.
func TestSameAggregatesAsTheOTLPPath(t *testing.T) {
	rec := newRecorded(t)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	p, _ := newPuller(t, srv)
	res, err := p.Pull(context.Background(), from, to)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Rejected != 0 {
		t.Errorf("rejected %d observations", res.Rejected)
	}

	viaOTLP := aggregate.New(aggregate.DefaultConfig())
	viaOTLP.Add(otlpSpans(t), to)
	viaLangfuse := aggregate.New(aggregate.DefaultConfig())
	viaLangfuse.Add(res.Spans, to)
	want, got := viaOTLP.FlushAll(), viaLangfuse.FlushAll()
	if len(got) != len(want) {
		t.Fatalf("%d windows via Langfuse, %d via OTLP", len(got), len(want))
	}
	for i := range want {
		for _, part := range []struct {
			name string
			g, w any
		}{{"metrics", got[i].Metrics, want[i].Metrics}, {"observed", got[i].Observed, want[i].Observed}, {"traces", got[i].TraceIDs, want[i].TraceIDs}} {
			if g, w := asJSON(t, part.g), asJSON(t, part.w); g != w {
				t.Errorf("window %d %s differ\nlangfuse: %s\n    otlp: %s", i, part.name, g, w)
			}
		}
	}
	// Content was counted (240 spans) but no observation text survived.
	for _, s := range res.Spans {
		for _, f := range []string{s.Provider, s.Model, s.Region, s.Operation, s.Guardrail, s.PromptName, s.PromptVersion, s.Slot} {
			if strings.Contains(f, "ZEBRA") {
				t.Fatalf("content in span: %+v", s)
			}
		}
	}

	// The requests: Basic auth, the hour range, cursor paging, every field group the
	// mapping needs, newest-first pages followed to the end.
	first := rec.requests[0]
	u, pw, ok := first.BasicAuth()
	q := first.URL.Query()
	if !ok || u != "pk-lf-test" || pw != "sk-lf-SECRET" || q.Get("fromStartTime") != "2026-09-01T00:00:00Z" ||
		q.Get("toStartTime") != "2026-09-01T02:00:00Z" || q.Get("limit") != "1000" {
		t.Errorf("first request %s", first.URL)
	}
	for _, g := range []string{"core", "basic", "model", "usage", "prompt", "metadata", "io"} {
		if !strings.Contains(","+q.Get("fields")+",", ","+g+",") {
			t.Errorf("fields %q lacks %s", q.Get("fields"), g)
		}
	}
}

func TestEvalRunsFromScores(t *testing.T) {
	srv := httptest.NewServer(newRecorded(t))
	defer srv.Close()
	p, _ := newPuller(t, srv)
	res, err := p.Pull(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	// 20 boolean scores (15 true) on generations and one numeric 0.95 on a
	// trace, all prompt v13 on claude-sonnet-4-5; the categorical score is not an
	// eval suite and is ignored.
	if len(res.Evals) != 1 {
		t.Fatalf("evals = %+v", res.Evals)
	}
	e := res.Evals[0]
	if e.Suite != "triage-eval" || e.Model != "anthropic.claude-sonnet-4-5" || e.PromptVersion != "13" || math.Abs(e.PassRate-16.0/21) > 1e-9 {
		t.Errorf("eval = %+v; want 16/21 passing", e)
	}
	if !strings.HasPrefix(e.RanAt, "2026-09-01T00:") {
		t.Errorf("ran_at = %s", e.RanAt)
	}
}

func TestBackoffOn429And5xx(t *testing.T) {
	rec := newRecorded(t)
	rec.fail = []int{http.StatusTooManyRequests, http.StatusBadGateway}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	p, slept := newPuller(t, srv)
	if _, err := p.Pull(context.Background(), from, to); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !reflect.DeepEqual(*slept, []time.Duration{2 * time.Second, 2 * time.Second}) {
		t.Errorf("slept %v; want Retry-After 2s, then the 2s second backoff step", *slept)
	}
}

func TestGivesUpAndNeverLeaksTheKey(t *testing.T) {
	rec := newRecorded(t)
	rec.fail = []int{500, 500, 500, 500, 500, 500}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	p, _ := newPuller(t, srv)
	_, err := p.Pull(context.Background(), from, to)
	if err == nil || !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("err = %v", err)
	}
	rec2 := newRecorded(t)
	rec2.fail = []int{http.StatusUnauthorized}
	srv2 := httptest.NewServer(rec2)
	defer srv2.Close()
	p2, slept := newPuller(t, srv2)
	if _, err := p2.Pull(context.Background(), from, to); err == nil || !strings.Contains(err.Error(), "401") || len(*slept) != 0 {
		t.Errorf("401: err %v, slept %v; want an immediate error", err, *slept)
	}
}

func TestPullRefusesBadInput(t *testing.T) {
	m, _ := otlp.DefaultMapping()
	p := &Puller{BaseURL: "http://unused", PublicKey: "pk", SecretKey: "sk", Mapping: m}
	if _, err := p.Pull(context.Background(), to, from); err == nil {
		t.Error("to before from accepted")
	}
	if _, err := (&Puller{BaseURL: "http://unused", Mapping: m}).Pull(context.Background(), from, to); err == nil {
		t.Error("missing keys accepted")
	}
}

func TestCursorLoopStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[],"meta":{"cursor":"same"}}`))
	}))
	defer srv.Close()
	p, _ := newPuller(t, srv)
	if _, err := p.Pull(context.Background(), from, to); err == nil || !strings.Contains(err.Error(), "pages") {
		t.Errorf("err = %v; want a page-limit error", err)
	}
}
