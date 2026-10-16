package usage

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/batch"
)

// Anthropic polls the Usage and Cost Admin API (messages usage report) with an
// Admin API key. It reports tokens only, no request counts. Docs:
// platform.claude.com, Usage and Cost API (checked 15 Oct 2026).
type Anthropic struct {
	BaseURL      string // default https://api.anthropic.com
	AdminKey     string
	WorkspaceIDs []string // optional: only these workspaces (production keys)
	HTTP         *http.Client
}

type anthropicPage struct {
	Data []struct {
		StartingAt time.Time `json:"starting_at"`
		Results    []struct {
			UncachedInputTokens  int64 `json:"uncached_input_tokens"`
			CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
			CacheCreation        struct {
				Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
				Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
			} `json:"cache_creation"`
			OutputTokens int64   `json:"output_tokens"`
			Model        *string `json:"model"`
			InferenceGeo *string `json:"inference_geo"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool    `json:"has_more"`
	NextPage *string `json:"next_page"`
}

// Poll implements Poller. Billed input = uncached + cache creation + cache
// reads, so traces must count cached input the same way (D-028).
func (a *Anthropic) Poll(ctx context.Context, start, end time.Time) ([]HourUsage, error) {
	if err := checkWindow(start, end, a.AdminKey); err != nil {
		return nil, err
	}
	base := a.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	rows := map[time.Time][]batch.Usage{}
	page := ""
	for n := 0; ; n++ {
		if n == maxPages {
			return nil, fmt.Errorf("usage: anthropic: more than %d pages", maxPages)
		}
		q := url.Values{
			"starting_at":  {start.UTC().Format(time.RFC3339)},
			"ending_at":    {end.UTC().Format(time.RFC3339)},
			"bucket_width": {"1h"},
			"group_by[]":   {"model", "inference_geo"},
			"limit":        {"168"},
		}
		for _, w := range a.WorkspaceIDs {
			q.Add("workspace_ids[]", w)
		}
		if page != "" {
			q.Set("page", page)
		}
		req, err := http.NewRequest(http.MethodGet, base+"/v1/organizations/usage_report/messages?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", a.AdminKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("User-Agent", "plumbline-agent")
		var p anthropicPage
		if err := getJSON(ctx, a.HTTP, req, &p); err != nil {
			return nil, err
		}
		for _, b := range p.Data {
			h := b.StartingAt.UTC()
			for _, r := range b.Results {
				model, region := "", ""
				if r.Model != nil {
					model = *r.Model
				}
				if r.InferenceGeo != nil && *r.InferenceGeo != "not_available" {
					region = *r.InferenceGeo
				}
				in := r.UncachedInputTokens + r.CacheReadInputTokens + r.CacheCreation.Ephemeral1h + r.CacheCreation.Ephemeral5m
				rows[h] = append(rows[h], batch.Usage{Provider: "anthropic", Model: model, Region: region, InputTokens: in, OutputTokens: r.OutputTokens})
			}
		}
		if !p.HasMore || p.NextPage == nil || *p.NextPage == "" {
			break
		}
		page = *p.NextPage
	}
	return collect(rows), nil
}
