package otlp

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func mustMapping(t testing.TB) *Mapping {
	t.Helper()
	m, err := DefaultMapping()
	if err != nil {
		t.Fatalf("DefaultMapping: %v", err)
	}
	return m
}

func readFixture(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Fixture 1: an OpenTelemetry collector file export (one request per line,
// integers as strings, new attribute names, content as attributes and as span
// events). The same file the concierge tests use, so the counts are known.
func TestParseCollectorExport(t *testing.T) {
	m := mustMapping(t)
	var all []Span
	for _, line := range bytes.Split(readFixture(t, filepath.Join("..", "..", "concierge", "testdata", "otlp-sample.jsonl")), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		res, err := Parse(line, m)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if res.Rejected != 0 {
			t.Fatalf("rejected %d spans", res.Rejected)
		}
		all = append(all, res.Spans...)
	}
	count := func(f func(Span) bool) int {
		n := 0
		for _, s := range all {
			if f(s) {
				n++
			}
		}
		return n
	}
	traces := map[string]bool{}
	tokens := map[string]int64{}
	for _, s := range all {
		traces[s.TraceID] = true
		if s.Kind == KindLLM {
			tokens[s.Provider+" "+s.Model+" "+s.Region] += s.InputTokens + s.OutputTokens
		}
	}
	checks := []struct {
		name      string
		got, want int
	}{
		{"traces", len(traces), 240},
		{"llm calls", count(func(s Span) bool { return s.Kind == KindLLM }), 320},
		{"agent runs", count(func(s Span) bool { return s.Kind == KindAgent }), 240},
		{"tool calls", count(func(s Span) bool { return s.Kind == KindTool }), 240},
		{"tool errors", count(func(s Span) bool { return s.Kind == KindTool && s.Error }), 8},
		{"content spans", count(func(s Span) bool { return s.ContentPresent }), 240},
		{"pii spans", count(func(s Span) bool { return s.PII }), 60},
		{"redacted", count(func(s Span) bool { return s.PII && s.Redacted }), 54},
		{"guarded", count(func(s Span) bool { return s.Guardrail != "" }), 240},
		{"blocked", count(func(s Span) bool { return s.Guardrail == "block" }), 10},
		{"high impact", count(func(s Span) bool { return s.HighImpact }), 24},
		{"approved", count(func(s Span) bool { return s.HighImpact && s.Approved }), 12},
		{"escalations", count(func(s Span) bool { return s.Escalation }), 1},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	// Token totals equal the concierge's aggregates for the same file; old
	// attribute names (gen_ai.system, prompt_tokens, aws.region) map too.
	want := map[string]int64{
		"aws.bedrock anthropic.claude-sonnet-4-5 ap-southeast-2": 320494,
		"aws.bedrock anthropic.claude-sonnet-4-6 ap-southeast-2": 68901,
		"openai gpt-4.1-mini us-east-1":                          7520,
	}
	if !reflect.DeepEqual(tokens, want) {
		t.Errorf("tokens = %v, want %v", tokens, want)
	}
}

// Fixture 2: an OpenLLMetry-style JavaScript export (one document, integers
// as JSON numbers, llm.request.type instead of gen_ai.operation.name, indexed
// content attributes, upper-case hex IDs, two malformed spans).
func TestParseOpenLLMetryExport(t *testing.T) {
	res, err := Parse(readFixture(t, filepath.Join("testdata", "openllmetry-js.json")), mustMapping(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Rejected != 2 {
		t.Errorf("Rejected = %d, want 2 (bad trace ID, no start time)", res.Rejected)
	}
	at := func(ns int64) time.Time { return time.Unix(0, ns).UTC() }
	want := []Span{
		{TraceID: "5b8efff798038103d269b633813fc60c", Start: at(1760500000000000000), Kind: KindLLM, Operation: "chat", Provider: "openai",
			Model: "gpt-4o-2024-08-06", Region: "ap-southeast-2", InputTokens: 812, OutputTokens: 96, ContentPresent: true,
			PII: true, Redacted: true, Guardrail: "allow", PromptName: "refunds", PromptVersion: "7", Slot: "support-bot"},
		{TraceID: "5b8efff798038103d269b633813fc60c", Start: at(1760500001300000000), Kind: KindTool, Operation: "execute_tool",
			Provider: "unknown", Model: "unknown", Region: "ap-southeast-2", Error: true, HighImpact: true, Slot: "support-bot"},
		{TraceID: "0af7651916cd43dd8448eb211c80319c", Start: at(1760500002000000000), Kind: KindLLM, Operation: "embedding", Provider: "openai",
			Model: "text-embedding-3-small", Region: "ap-southeast-2", InputTokens: 40, ContentPresent: true, Slot: "support-bot"},
		{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", Start: at(1760503600000000000), Kind: KindLLM, Operation: "chat", Provider: "anthropic",
			Model: "claude-sonnet-4-5", Region: "ap-southeast-2", InputTokens: 1500, OutputTokens: 200, Error: true, ContentPresent: true,
			Escalation: true, Slot: "triage"},
	}
	if len(res.Spans) != len(want) {
		t.Fatalf("got %d spans, want %d: %+v", len(res.Spans), len(want), res.Spans)
	}
	for i := range want {
		if res.Spans[i] != want[i] {
			t.Errorf("span %d\n got %+v\nwant %+v", i, res.Spans[i], want[i])
		}
	}
	assertNoSecret(t, res.Spans, "SECRET")
}

// assertNoSecret checks every string field of every span.
func assertNoSecret(t *testing.T, spans []Span, secret string) {
	t.Helper()
	for i, s := range spans {
		v := reflect.ValueOf(s)
		for f := 0; f < v.NumField(); f++ {
			if v.Field(f).Kind() == reflect.String && strings.Contains(v.Field(f).String(), secret) {
				t.Fatalf("span %d field %s carries content: %q", i, v.Type().Field(f).Name, v.Field(f).String())
			}
		}
	}
}

func spanJSON(attrs, events string) []byte {
	return []byte(`{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[{"spans":[{"traceId":"0af7651916cd43dd8448eb211c80319c",` +
		`"startTimeUnixNano":"1760500000000000000","name":"n","attributes":[` + attrs + `],"events":[` + events + `]}]}]}]}`)
}

func strAttr(k, v string) string {
	b, _ := json.Marshal(v)
	return `{"key":"` + k + `","value":{"stringValue":` + string(b) + `}}`
}

// The no-content property: text placed in any content attribute, an event, an
// unmapped attribute or the span name never reaches a parsed span.
func FuzzNoContentInSpans(f *testing.F) {
	for _, s := range []string{"SECRET claim text", "Jane Citizen 4421", "{\"role\":\"user\"}", "line\nbreak \u2028", "\x00\xff\xfe"} {
		f.Add(s)
	}
	m := mustMapping(f)
	f.Fuzz(func(t *testing.T, secret string) {
		// The span's own mapped values ("chat", "openai", "m1", "unknown", the
		// trace ID) are legitimately in it; a secret inside them proves nothing.
		if len(secret) < 4 || strings.Contains("chat openai m1 unknown 0af7651916cd43dd8448eb211c80319c", secret) {
			return
		}
		attrs := strings.Join([]string{
			strAttr("gen_ai.operation.name", "chat"), strAttr("gen_ai.provider.name", "openai"), strAttr("gen_ai.request.model", "m1"),
			strAttr("gen_ai.input.messages", secret), strAttr("gen_ai.output.messages", secret), strAttr("gen_ai.prompt", secret),
			strAttr("gen_ai.prompt.0.content", secret), strAttr("gen_ai.completion.3.content", secret), strAttr("llm.prompts.1.content", secret),
			strAttr("user.question", secret), strAttr("http.request.body", secret),
		}, ",")
		events := `{"name":"gen_ai.content.prompt","attributes":[` + strAttr("gen_ai.prompt", secret) + `]}`
		res, err := Parse(spanJSON(attrs, events), m)
		if err != nil {
			return // input made the JSON invalid; nothing parsed, nothing leaked
		}
		if len(res.Spans) != 1 || !res.Spans[0].ContentPresent {
			t.Fatalf("spans = %+v; want one span with content present", res.Spans)
		}
		assertNoSecret(t, res.Spans, secret)
	})
}

func TestParseValueForms(t *testing.T) {
	m := mustMapping(t)
	attrs := strings.Join([]string{
		`{"key":"gen_ai.operation.name","value":{"stringValue":"chat"}}`,
		`{"key":"gen_ai.usage.input_tokens","value":{"intValue":"9007199254740991"}}`,
		`{"key":"gen_ai.usage.output_tokens","value":{"doubleValue":12.0}}`,
		`{"key":"plumbline.tool.high_impact","value":{"stringValue":"TRUE"}}`,
		`{"key":"plumbline.redaction.applied","value":{"intValue":1}}`,
		`{"key":"gen_ai.request.model","value":{"intValue":5}}`,
	}, ",")
	res, err := Parse(spanJSON(attrs, ""), m)
	if err != nil || len(res.Spans) != 1 {
		t.Fatalf("Parse = %+v, %v", res, err)
	}
	s := res.Spans[0]
	if s.InputTokens != 9007199254740991 || s.OutputTokens != 12 || !s.HighImpact || s.Redacted || s.Model != "unknown" {
		t.Errorf("span = %+v", s)
	}
}

func TestParseRejects(t *testing.T) {
	m := mustMapping(t)
	for name, body := range map[string]string{
		"not JSON":            `{`,
		"not an object":       `[]`,
		"trailing data":       `{"resourceSpans":[]} x`,
		"wrong resourceSpans": `{"resourceSpans":{}}`,
	} {
		if _, err := Parse([]byte(body), m); err == nil {
			t.Errorf("%s: Parse accepted %q", name, body)
		}
	}
	for name, span := range map[string]string{
		"short trace ID":     `{"traceId":"abc","startTimeUnixNano":"1"}`,
		"all-zero trace ID":  `{"traceId":"00000000000000000000000000000000","startTimeUnixNano":"1"}`,
		"non-hex trace ID":   `{"traceId":"zzf7651916cd43dd8448eb211c80319c","startTimeUnixNano":"1"}`,
		"no start":           `{"traceId":"0af7651916cd43dd8448eb211c80319c"}`,
		"start not a number": `{"traceId":"0af7651916cd43dd8448eb211c80319c","startTimeUnixNano":"soon"}`,
		"negative start":     `{"traceId":"0af7651916cd43dd8448eb211c80319c","startTimeUnixNano":-5}`,
	} {
		res, err := Parse([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[`+span+`]}]}]}`), m)
		if err != nil || res.Rejected != 1 || len(res.Spans) != 0 {
			t.Errorf("%s: Parse = %+v, %v; want one rejected span", name, res, err)
		}
	}
}

func TestMappingRejectsBadSemconv(t *testing.T) {
	if _, err := NewMapping([]byte(`{"version":"x"}`)); err == nil {
		t.Error("NewMapping accepted an incomplete map")
	}
	if _, err := NewMapping([]byte(`{`)); err == nil {
		t.Error("NewMapping accepted bad JSON")
	}
}

type sink struct{ results []Result }

func (s *sink) put(r Result) { s.results = append(s.results, r) }

func post(t *testing.T, h http.Handler, body []byte, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReceiver(t *testing.T) {
	fixture := readFixture(t, filepath.Join("testdata", "openllmetry-js.json"))
	good := spanJSON(strAttr("gen_ai.operation.name", "chat"), "")
	newRecv := func() (*Receiver, *sink) {
		s := &sink{}
		return &Receiver{Mapping: mustMapping(t), Sink: s.put, MaxBody: 1 << 16, MaxDecoded: 1 << 18}, s
	}

	t.Run("JSON body", func(t *testing.T) {
		r, s := newRecv()
		rec := post(t, r, good, nil)
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "{}" || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("got %d %q", rec.Code, rec.Body.String())
		}
		if len(s.results) != 1 || len(s.results[0].Spans) != 1 {
			t.Fatalf("sink got %+v", s.results)
		}
	})
	t.Run("gzip body", func(t *testing.T) {
		r, s := newRecv()
		rec := post(t, r, gz(t, fixture), map[string]string{"Content-Encoding": "gzip"})
		if rec.Code != 200 || len(s.results) != 1 || len(s.results[0].Spans) != 4 {
			t.Fatalf("got %d %q, sink %d", rec.Code, rec.Body.String(), len(s.results))
		}
		// Rejected spans are reported as OTLP partial success.
		var ps struct {
			PartialSuccess struct {
				RejectedSpans string `json:"rejectedSpans"`
				ErrorMessage  string `json:"errorMessage"`
			} `json:"partialSuccess"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &ps); err != nil || ps.PartialSuccess.RejectedSpans != "2" {
			t.Errorf("partial success = %q (%v)", rec.Body.String(), err)
		}
	})
	t.Run("charset in content type", func(t *testing.T) {
		r, _ := newRecv()
		if rec := post(t, r, good, map[string]string{"Content-Type": "application/json; charset=utf-8"}); rec.Code != 200 {
			t.Errorf("got %d", rec.Code)
		}
	})

	errorCases := []struct {
		name   string
		method string
		body   []byte
		header map[string]string
		code   int
	}{
		{"protobuf", http.MethodPost, []byte("\x0a\x00"), map[string]string{"Content-Type": "application/x-protobuf"}, http.StatusUnsupportedMediaType},
		{"GET", http.MethodGet, nil, nil, http.StatusMethodNotAllowed},
		{"bad JSON", http.MethodPost, []byte("{"), nil, http.StatusBadRequest},
		{"body too large", http.MethodPost, bytes.Repeat([]byte(" "), 1<<16+1), nil, http.StatusRequestEntityTooLarge},
		{"gzip bomb", http.MethodPost, gz(t, append([]byte(`{"resourceSpans":[]}`), bytes.Repeat([]byte(" "), 1<<18)...)), map[string]string{"Content-Encoding": "gzip"}, http.StatusRequestEntityTooLarge},
		{"bad gzip", http.MethodPost, []byte("not gzip"), map[string]string{"Content-Encoding": "gzip"}, http.StatusBadRequest},
		{"unknown encoding", http.MethodPost, good, map[string]string{"Content-Encoding": "br"}, http.StatusUnsupportedMediaType},
	}
	for _, c := range errorCases {
		t.Run(c.name, func(t *testing.T) {
			r, s := newRecv()
			req := httptest.NewRequest(c.method, "/v1/traces", bytes.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			for k, v := range c.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != c.code {
				t.Errorf("got %d %q, want %d", rec.Code, rec.Body.String(), c.code)
			}
			if len(s.results) != 0 {
				t.Errorf("sink called on a refused request")
			}
			if strings.Contains(rec.Body.String(), "SECRET") {
				t.Error("error response echoes request content")
			}
		})
	}
}

// The receiver never puts request content into its responses or errors.
func TestReceiverErrorsDoNotEchoContent(t *testing.T) {
	r := &Receiver{Mapping: mustMapping(t), Sink: func(Result) {}}
	body := []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"SECRET-` + strings.Repeat("x", 30) + `"}]}]}],"x":"SECRET`)
	rec := post(t, r, body, nil)
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Errorf("response %q echoes content", rec.Body.String())
	}
	_, _ = io.Copy(io.Discard, rec.Body)
}
