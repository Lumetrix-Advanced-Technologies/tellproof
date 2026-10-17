// Package usage polls AI providers' usage APIs for the tokens they billed, per
// hour and model, so trace coverage (EO-LOG-01) can reconcile traced tokens
// against billed ones (docs/architecture.md §3, docs/library-docs.md). Admin
// keys are read-only; they and response bodies are never logged or echoed.
package usage

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
)

// HourUsage is one provider's billed usage for one UTC hour.
type HourUsage struct {
	Start time.Time
	Usage []batch.Usage
}

// Poller returns hourly usage for [start, end), both on the hour.
type Poller interface {
	Poll(ctx context.Context, start, end time.Time) ([]HourUsage, error)
}

const (
	maxPages     = 50       // a poll covers at most a week of hours; more pages means a loop
	maxBodyBytes = 16 << 20 // per response
)

func checkWindow(start, end time.Time, key string) error {
	switch {
	case key == "":
		return errors.New("usage: no admin key configured")
	case !end.After(start):
		return errors.New("usage: end must be after start")
	case !start.Equal(start.Truncate(time.Hour)) || !end.Equal(end.Truncate(time.Hour)):
		return errors.New("usage: start and end must be on the hour")
	}
	return nil
}

// getJSON GETs u and decodes the JSON body into v. Errors name the status, not
// the body, and never include request headers.
func getJSON(ctx context.Context, client *http.Client, req *http.Request, v any) error {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("usage: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		return fmt.Errorf("usage: %s %s: HTTP %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(v); err != nil {
		return fmt.Errorf("usage: %s: decoding the response: %w", req.URL.Path, err)
	}
	return nil
}

// collect merges usage rows into hours, summing rows with the same provider,
// model and region, in a stable order.
func collect(rows map[time.Time][]batch.Usage) []HourUsage {
	var out []HourUsage
	for start, us := range rows {
		type key struct{ provider, model, region string }
		merged := map[key]*batch.Usage{}
		var order []key
		for _, u := range us {
			k := key{u.Provider, u.Model, u.Region}
			m := merged[k]
			if m == nil {
				c := u
				merged[k] = &c
				order = append(order, k)
				continue
			}
			m.InputTokens += u.InputTokens
			m.OutputTokens += u.OutputTokens
			if m.Requests != nil && u.Requests != nil {
				n := *m.Requests + *u.Requests
				m.Requests = &n
			} else {
				m.Requests = nil
			}
		}
		h := HourUsage{Start: start}
		for _, k := range order {
			h.Usage = append(h.Usage, *merged[k])
		}
		slices.SortFunc(h.Usage, func(a, b batch.Usage) int {
			return cmp.Or(cmp.Compare(a.Provider, b.Provider), cmp.Compare(a.Model, b.Model), cmp.Compare(a.Region, b.Region))
		})
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b HourUsage) int { return a.Start.Compare(b.Start) })
	return out
}

// ProviderCoverage is traced against billed tokens for one provider.
type ProviderCoverage struct {
	Provider string
	Billed   int64
	Traced   int64
	HasUsage bool // false: traces but no usage data; listed, not counted in the total
}

// Percent is traced / billed × 100 (0 with no billed tokens).
func (p ProviderCoverage) Percent() float64 {
	if p.Billed == 0 {
		return 0
	}
	return 100 * float64(p.Traced) / float64(p.Billed)
}

// Coverage is token coverage over a period.
type Coverage struct {
	Providers []ProviderCoverage
	Billed    int64
	Traced    int64
}

// Percent is the total traced / billed × 100 over providers with usage data.
func (c Coverage) Percent() float64 {
	if c.Billed == 0 {
		return 0
	}
	return 100 * float64(c.Traced) / float64(c.Billed)
}

// Reconcile compares traced tokens with billed usage per provider (tokens:
// input + output). Usage APIs for OpenAI and Anthropic carry no cloud region,
// so coverage is matched per provider; only providers with usage data count
// toward the total (D-021, D-028).
func Reconcile(traced []batch.TokenCount, billed []HourUsage) Coverage {
	by := map[string]*ProviderCoverage{}
	get := func(p string) *ProviderCoverage {
		if by[p] == nil {
			by[p] = &ProviderCoverage{Provider: p}
		}
		return by[p]
	}
	for _, h := range billed {
		for _, u := range h.Usage {
			pc := get(u.Provider)
			pc.Billed += u.InputTokens + u.OutputTokens
			pc.HasUsage = true
		}
	}
	for _, t := range traced {
		get(t.Provider).Traced += t.InputTokens + t.OutputTokens
	}
	var c Coverage
	for _, pc := range by {
		c.Providers = append(c.Providers, *pc)
		if pc.HasUsage {
			c.Billed += pc.Billed
			c.Traced += pc.Traced
		}
	}
	slices.SortFunc(c.Providers, func(a, b ProviderCoverage) int { return cmp.Compare(a.Provider, b.Provider) })
	return c
}
