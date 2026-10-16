package usage

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/batch"
)

var (
	h10 = time.Date(2025, 10, 1, 10, 0, 0, 0, time.UTC)
	h11 = h10.Add(time.Hour)
	h13 = h10.Add(3 * time.Hour)
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeAPI serves recorded responses and records what was asked.
type fakeAPI struct {
	t        *testing.T
	pages    map[string][]byte // "path?page" -> body
	requests []*http.Request
	status   int
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests = append(f.requests, r.Clone(context.Background()))
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
		return
	}
	body, ok := f.pages[r.URL.Path+"?"+r.URL.Query().Get("page")]
	if !ok {
		f.t.Errorf("unexpected request %s", r.URL)
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func find(hs []HourUsage, start time.Time, provider, model string) (batch.Usage, bool) {
	for _, h := range hs {
		if !h.Start.Equal(start) {
			continue
		}
		for _, u := range h.Usage {
			if u.Provider == provider && u.Model == model {
				return u, true
			}
		}
	}
	return batch.Usage{}, false
}

func TestOpenAIPoller(t *testing.T) {
	api := &fakeAPI{t: t, pages: map[string][]byte{
		"/v1/organization/usage/completions?":         fixture(t, "openai-completions-1.json"),
		"/v1/organization/usage/completions?page_AAA": fixture(t, "openai-completions-2.json"),
		"/v1/organization/usage/embeddings?":          fixture(t, "openai-embeddings.json"),
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	p := &OpenAI{BaseURL: srv.URL, AdminKey: "sk-admin-test", HTTP: srv.Client(), ProjectIDs: []string{"proj_1"}}
	got, err := p.Poll(context.Background(), h10, h13)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	u, ok := find(got, h10, "openai", "gpt-4o-2024-08-06")
	if !ok || u.InputTokens != 120000 || u.OutputTokens != 30000 || u.Requests == nil || *u.Requests != 400 || u.Region != "" {
		t.Errorf("gpt-4o at 10:00 = %+v (cached tokens are already inside input_tokens)", u)
	}
	if u, ok := find(got, h10, "openai", "text-embedding-3-small"); !ok || u.InputTokens != 40000 || u.OutputTokens != 0 {
		t.Errorf("embeddings at 10:00 = %+v", u)
	}
	if u, ok := find(got, h11, "openai", "gpt-4o-2024-08-06"); !ok || u.InputTokens != 80000 {
		t.Errorf("gpt-4o at 11:00 (second page) = %+v", u)
	}
	// The request: admin key as a bearer token, hourly buckets by model, Unix seconds.
	q := api.requests[0].URL.Query()
	if api.requests[0].Header.Get("Authorization") != "Bearer sk-admin-test" ||
		q.Get("bucket_width") != "1h" || q.Get("start_time") != "1759312800" || q.Get("end_time") != "1759323600" ||
		q.Get("group_by") != "model" || q.Get("project_ids") != "proj_1" {
		t.Errorf("request = %s %v", api.requests[0].URL, api.requests[0].Header)
	}
	if len(api.requests) != 3 {
		t.Errorf("made %d requests, want 3 (two completions pages, one embeddings)", len(api.requests))
	}
}

func TestAnthropicPoller(t *testing.T) {
	api := &fakeAPI{t: t, pages: map[string][]byte{
		"/v1/organizations/usage_report/messages?":                                  fixture(t, "anthropic-messages-1.json"),
		"/v1/organizations/usage_report/messages?page_MjAyNS0xMC0wMVQxMTowMDowMFo=": fixture(t, "anthropic-messages-2.json"),
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	p := &Anthropic{BaseURL: srv.URL, AdminKey: "sk-ant-admin01-test", HTTP: srv.Client(), WorkspaceIDs: []string{"wrkspc_prod"}}
	got, err := p.Poll(context.Background(), h10, h13)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	// Input = uncached + cache creation + cache reads; no request counts.
	u, ok := find(got, h10, "anthropic", "claude-sonnet-4-5")
	if !ok || u.InputTokens != 90000+5000+25000 || u.OutputTokens != 40000 || u.Requests != nil || u.Region != "global" {
		t.Errorf("10:00 = %+v", u)
	}
	if u, ok := find(got, h11, "anthropic", "claude-sonnet-4-5"); !ok || u.InputTokens != 70000 || u.Region != "" {
		t.Errorf("11:00 = %+v (not_available is no region)", u)
	}
	r := api.requests[0]
	q := r.URL.Query()
	if r.Header.Get("x-api-key") != "sk-ant-admin01-test" || r.Header.Get("anthropic-version") != "2023-06-01" ||
		q.Get("bucket_width") != "1h" || q.Get("starting_at") != "2025-10-01T10:00:00Z" || q.Get("ending_at") != "2025-10-01T13:00:00Z" ||
		strings.Join(q["group_by[]"], ",") != "model,inference_geo" || q.Get("workspace_ids[]") != "wrkspc_prod" {
		t.Errorf("request = %s %v", r.URL, r.Header)
	}
}

// Acceptance: against recorded responses, coverage matches a hand calculation
// within 0.1 point.
func TestCoverageMatchesHandCalculation(t *testing.T) {
	billed := []HourUsage{
		{Start: h10, Usage: []batch.Usage{
			{Provider: "openai", Model: "gpt-4o-2024-08-06", InputTokens: 120000, OutputTokens: 30000},
			{Provider: "openai", Model: "gpt-4o-mini", InputTokens: 50000, OutputTokens: 5000},
			{Provider: "openai", Model: "text-embedding-3-small", InputTokens: 40000},
			{Provider: "anthropic", Model: "claude-sonnet-4-5", Region: "global", InputTokens: 120000, OutputTokens: 40000},
		}},
		{Start: h11, Usage: []batch.Usage{
			{Provider: "openai", Model: "gpt-4o-2024-08-06", InputTokens: 80000, OutputTokens: 20000},
			{Provider: "anthropic", Model: "claude-sonnet-4-5", InputTokens: 70000, OutputTokens: 20000},
		}},
	}
	traced := []batch.TokenCount{
		{Provider: "openai", Model: "gpt-4o-2024-08-06", Region: "us-east-1", InputTokens: 197000, OutputTokens: 49500},
		{Provider: "openai", Model: "gpt-4o-mini", Region: "us-east-1", InputTokens: 50000, OutputTokens: 5000},
		{Provider: "openai", Model: "text-embedding-3-small", Region: "us-east-1", InputTokens: 40050},
		{Provider: "anthropic", Model: "claude-sonnet-4-5", Region: "global", InputTokens: 186600, OutputTokens: 60000},
		{Provider: "aws.bedrock", Model: "anthropic.claude-sonnet-4-5", Region: "ap-southeast-2", InputTokens: 999},
	}
	// Hand calculation:
	//   openai    billed 150000 + 55000 + 40000 + 100000 = 345000; traced 246500 + 55000 + 40050 = 341550 -> 99.0%
	//   anthropic billed 160000 + 90000 = 250000;                 traced 246600                       -> 98.64%
	//   total     billed 595000; traced 588150 (bedrock has no usage: listed, not counted)          -> 98.85%
	cov := Reconcile(traced, billed)
	want := map[string]float64{"openai": 99.0, "anthropic": 98.64}
	for _, p := range cov.Providers {
		if w, ok := want[p.Provider]; ok {
			if math.Abs(p.Percent()-w) > 0.1 {
				t.Errorf("%s coverage %.2f%%, hand calculation %.2f%%", p.Provider, p.Percent(), w)
			}
			delete(want, p.Provider)
		}
		if p.Provider == "aws.bedrock" && (p.HasUsage || p.Traced != 999) {
			t.Errorf("bedrock without usage = %+v", p)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing providers %v", want)
	}
	if math.Abs(cov.Percent()-98.85) > 0.1 || cov.Billed != 595000 || cov.Traced != 588150 {
		t.Errorf("total coverage %.2f%% (%d/%d), hand calculation 98.85%%", cov.Percent(), cov.Traced, cov.Billed)
	}
}

func TestPollErrorsNeverIncludeTheKey(t *testing.T) {
	srv := httptest.NewServer(&fakeAPI{t: t, status: http.StatusUnauthorized})
	defer srv.Close()
	for name, p := range map[string]Poller{
		"openai":    &OpenAI{BaseURL: srv.URL, AdminKey: "sk-admin-SECRET", HTTP: srv.Client()},
		"anthropic": &Anthropic{BaseURL: srv.URL, AdminKey: "sk-ant-admin01-SECRET", HTTP: srv.Client()},
	} {
		_, err := p.Poll(context.Background(), h10, h13)
		if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: err = %v; want a 401 error without the key", name, err)
		}
	}
}

func TestPollRejectsBadWindows(t *testing.T) {
	p := &OpenAI{BaseURL: "http://unused", AdminKey: "k"}
	if _, err := p.Poll(context.Background(), h13, h10); err == nil {
		t.Error("end before start accepted")
	}
	if _, err := p.Poll(context.Background(), h10.Add(30*time.Minute), h13); err == nil {
		t.Error("start not on the hour accepted")
	}
	if _, err := (&OpenAI{BaseURL: "http://unused"}).Poll(context.Background(), h10, h13); err == nil {
		t.Error("missing admin key accepted")
	}
}

func TestPollFollowsAtMostMaxPages(t *testing.T) {
	loop := []byte(`{"object":"page","has_more":true,"next_page":"again","data":[]}`)
	srv := httptest.NewServer(&fakeAPI{t: t, pages: map[string][]byte{
		"/v1/organization/usage/completions?": loop, "/v1/organization/usage/completions?again": loop,
	}})
	defer srv.Close()
	_, err := (&OpenAI{BaseURL: srv.URL, AdminKey: "k", HTTP: srv.Client()}).Poll(context.Background(), h10, h13)
	if err == nil || !strings.Contains(err.Error(), "pages") {
		t.Errorf("err = %v; want a page-limit error", err)
	}
}
