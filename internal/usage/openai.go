package usage

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
)

// OpenAI polls the organisation Usage API (completions and embeddings) with
// an organisation admin key. Docs: platform.openai.com, Usage API; response
// fields checked against the official SDK's types (15 Oct 2026).
type OpenAI struct {
	BaseURL    string // default https://api.openai.com
	AdminKey   string
	ProjectIDs []string // optional: only these projects (production keys)
	HTTP       *http.Client
}

type openAIPage struct {
	Data []struct {
		StartTime int64 `json:"start_time"`
		Results   []struct {
			InputTokens      int64  `json:"input_tokens"` // includes input_cached_tokens
			OutputTokens     int64  `json:"output_tokens"`
			NumModelRequests int64  `json:"num_model_requests"`
			Model            string `json:"model"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool    `json:"has_more"`
	NextPage *string `json:"next_page"`
}

// Poll implements Poller.
func (o *OpenAI) Poll(ctx context.Context, start, end time.Time) ([]HourUsage, error) {
	if err := checkWindow(start, end, o.AdminKey); err != nil {
		return nil, err
	}
	base := o.BaseURL
	if base == "" {
		base = "https://api.openai.com"
	}
	rows := map[time.Time][]batch.Usage{}
	for _, kind := range []string{"completions", "embeddings"} {
		page := ""
		for n := 0; ; n++ {
			if n == maxPages {
				return nil, fmt.Errorf("usage: openai %s: more than %d pages", kind, maxPages)
			}
			q := url.Values{
				"start_time":   {strconv.FormatInt(start.Unix(), 10)},
				"end_time":     {strconv.FormatInt(end.Unix(), 10)},
				"bucket_width": {"1h"},
				"group_by":     {"model"},
				"limit":        {"168"},
			}
			for _, p := range o.ProjectIDs {
				q.Add("project_ids", p)
			}
			if page != "" {
				q.Set("page", page)
			}
			req, err := http.NewRequest(http.MethodGet, base+"/v1/organization/usage/"+kind+"?"+q.Encode(), nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+o.AdminKey)
			var p openAIPage
			if err := getJSON(ctx, o.HTTP, req, &p); err != nil {
				return nil, err
			}
			for _, b := range p.Data {
				h := time.Unix(b.StartTime, 0).UTC()
				for _, r := range b.Results {
					reqs := r.NumModelRequests
					rows[h] = append(rows[h], batch.Usage{Provider: "openai", Model: r.Model, InputTokens: r.InputTokens,
						OutputTokens: r.OutputTokens, Requests: &reqs})
				}
			}
			if !p.HasMore || p.NextPage == nil || *p.NextPage == "" {
				break
			}
			page = *p.NextPage
		}
	}
	return collect(rows), nil
}
