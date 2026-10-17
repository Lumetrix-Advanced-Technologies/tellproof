// Package langfuse pulls observations (Observations API v2) and eval scores
// (Scores API v3) from a Langfuse project and turns them into the same span
// facts as the OTLP receiver, through the same semconv mapping, so both
// sources aggregate identically. Message input and output are checked for
// presence and never kept. Keys are never logged or echoed.
package langfuse

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/otlp"
)

const (
	maxPages     = 1000 // 1,000 observations a page: a million per pull
	maxAttempts  = 5
	maxBodyBytes = 64 << 20
)

// EvalScore names a Langfuse score that records an eval suite's results.
type EvalScore struct {
	Name        string  // score name in Langfuse
	Suite       string  // suite name for the batch's evals
	PassAtLeast float64 // a NUMERIC score passes at or above this; BOOLEAN true passes
}

// Puller reads one Langfuse project with its public and secret key.
type Puller struct {
	BaseURL    string // e.g. https://cloud.langfuse.com (EU), https://us.cloud.langfuse.com
	PublicKey  string
	SecretKey  string
	HTTP       *http.Client
	Mapping    *otlp.Mapping
	EvalScores []EvalScore
	// Sleep waits between retries; nil uses a context-aware timer.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Result is one pull.
type Result struct {
	Spans    []otlp.Span
	Evals    []batch.Eval
	Rejected int
}

type observation struct {
	ID            string          `json:"id"`
	TraceID       string          `json:"traceId"`
	StartTime     time.Time       `json:"startTime"`
	Type          string          `json:"type"`
	Level         string          `json:"level"`
	Model         string          `json:"providedModelName"`
	InputUsage    *float64        `json:"inputUsage"`
	OutputUsage   *float64        `json:"outputUsage"`
	PromptName    *string         `json:"promptName"`
	PromptVersion *int64          `json:"promptVersion"`
	Metadata      json.RawMessage `json:"metadata"`
	Input         json.RawMessage `json:"input"`  // presence only
	Output        json.RawMessage `json:"output"` // presence only
}

type page[T any] struct {
	Data []T `json:"data"`
	Meta struct {
		Cursor *string `json:"cursor"`
	} `json:"meta"`
}

type score struct {
	Name      string          `json:"name"`
	Value     json.RawMessage `json:"value"`
	DataType  string          `json:"dataType"`
	Timestamp time.Time       `json:"timestamp"`
	Subject   struct {
		Kind    string `json:"kind"`
		ID      string `json:"id"`
		TraceID string `json:"traceId"`
	} `json:"subject"`
}

// operation maps Langfuse observation types to OTel GenAI operation names.
func operation(observationType string) (string, bool) {
	op, ok := map[string]string{"GENERATION": "chat", "EMBEDDING": "embeddings", "AGENT": "invoke_agent", "TOOL": "execute_tool"}[strings.ToUpper(observationType)]
	return op, ok
}

// Pull reads observations that started in [from, to) and the eval scores in
// the same range.
func (p *Puller) Pull(ctx context.Context, from, to time.Time) (Result, error) {
	switch {
	case p.PublicKey == "" || p.SecretKey == "":
		return Result{}, errors.New("langfuse: public and secret keys are required")
	case !to.After(from):
		return Result{}, errors.New("langfuse: the end must be after the start")
	case p.Mapping == nil:
		return Result{}, errors.New("langfuse: no semconv mapping")
	}
	var res Result
	gens := map[string]otlp.Span{}     // observation ID -> span, for eval scores
	firstGen := map[string]otlp.Span{} // trace ID -> earliest generation
	q := url.Values{
		"fromStartTime": {from.UTC().Format(time.RFC3339)},
		"toStartTime":   {to.UTC().Format(time.RFC3339)},
		"fields":        {"core,basic,model,usage,prompt,metadata,io"},
		"limit":         {"1000"},
	}
	err := paginate(ctx, p, "/api/public/v2/observations", q, func(o observation) {
		s, ok := p.span(o)
		if !ok {
			res.Rejected++
			return
		}
		res.Spans = append(res.Spans, s)
		if s.Kind == otlp.KindLLM {
			gens[o.ID] = s
			if f, seen := firstGen[s.TraceID]; !seen || s.Start.Before(f.Start) {
				firstGen[s.TraceID] = s
			}
		}
	})
	if err != nil {
		return Result{}, err
	}
	// Pages arrive newest first; keep spans in time order.
	slices.SortStableFunc(res.Spans, func(a, b otlp.Span) int { return cmp.Or(a.Start.Compare(b.Start), cmp.Compare(a.TraceID, b.TraceID)) })
	if len(p.EvalScores) > 0 {
		evals, err := p.evals(ctx, from, to, gens, firstGen)
		if err != nil {
			return Result{}, err
		}
		res.Evals = evals
	}
	return res, nil
}

// span maps one observation through the semconv mapping.
func (p *Puller) span(o observation) (otlp.Span, bool) {
	attrs, resource := map[string]any{}, map[string]any{}
	var md map[string]any
	if len(o.Metadata) > 0 && json.Unmarshal(o.Metadata, &md) == nil {
		for k, v := range md {
			switch k {
			case "attributes": // OpenTelemetry ingestion keeps span attributes here
				if m, ok := v.(map[string]any); ok {
					for ak, av := range m {
						attrs[ak] = av
					}
				}
			case "resourceAttributes":
				if m, ok := v.(map[string]any); ok {
					for rk, rv := range m {
						resource[rk] = rv
					}
				}
			case "provider": // SDK-style metadata
				setDefault(attrs, "gen_ai.provider.name", v)
			case "region":
				setDefault(attrs, "cloud.region", v)
			default:
				attrs[k] = v
			}
		}
	}
	if op, ok := operation(o.Type); ok {
		setDefault(attrs, "gen_ai.operation.name", op)
	}
	if o.Model != "" {
		attrs["gen_ai.response.model"] = o.Model
	}
	if o.InputUsage != nil {
		attrs["gen_ai.usage.input_tokens"] = *o.InputUsage
	}
	if o.OutputUsage != nil {
		attrs["gen_ai.usage.output_tokens"] = *o.OutputUsage
	}
	if o.PromptName != nil && *o.PromptName != "" {
		attrs["tellproof.prompt.name"] = *o.PromptName
	}
	if o.PromptVersion != nil {
		attrs["tellproof.prompt.version"] = strconv.FormatInt(*o.PromptVersion, 10)
	}
	content := present(o.Input) || present(o.Output)
	return p.Mapping.Build(strings.ToLower(o.TraceID), o.StartTime, attrs, resource, strings.EqualFold(o.Level, "ERROR"), content)
}

func setDefault(m map[string]any, k string, v any) {
	if _, ok := m[k]; !ok {
		m[k] = v
	}
}

// present reports whether a raw JSON value holds anything (its text is never decoded).
func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != `""` && s != "{}" && s != "[]"
}

// evals turns configured eval scores into one run per suite, model and prompt
// version: the share of scores that pass.
func (p *Puller) evals(ctx context.Context, from, to time.Time, gens, firstGen map[string]otlp.Span) ([]batch.Eval, error) {
	bySuite := map[string]EvalScore{}
	var names []string
	for _, e := range p.EvalScores {
		bySuite[e.Name] = e
		names = append(names, e.Name)
	}
	type key struct{ suite, model, version string }
	type tally struct {
		pass, total int
		last        time.Time
	}
	runs := map[key]*tally{}
	q := url.Values{
		"fromTimestamp": {from.UTC().Format(time.RFC3339)},
		"toTimestamp":   {to.UTC().Format(time.RFC3339)},
		"name":          {strings.Join(names, ",")},
		"fields":        {"core,subject"},
		"limit":         {"100"},
	}
	err := paginate(ctx, p, "/api/public/v3/scores", q, func(s score) {
		cfg, ok := bySuite[s.Name]
		if !ok {
			return
		}
		var g otlp.Span
		switch s.Subject.Kind {
		case "observation":
			g, ok = gens[s.Subject.ID]
		case "trace":
			g, ok = firstGen[strings.ToLower(s.Subject.ID)]
		default:
			ok = false
		}
		if !ok {
			return
		}
		var pass bool
		switch strings.ToUpper(s.DataType) {
		case "BOOLEAN":
			var b bool
			if json.Unmarshal(s.Value, &b) != nil {
				return
			}
			pass = b
		case "NUMERIC":
			var f float64
			if json.Unmarshal(s.Value, &f) != nil {
				return
			}
			pass = f >= cfg.PassAtLeast
		default:
			return
		}
		k := key{cfg.Suite, g.Model, g.PromptVersion}
		t := runs[k]
		if t == nil {
			t = &tally{}
			runs[k] = t
		}
		t.total++
		if pass {
			t.pass++
		}
		if s.Timestamp.After(t.last) {
			t.last = s.Timestamp
		}
	})
	if err != nil {
		return nil, err
	}
	var out []batch.Eval
	for k, t := range runs {
		out = append(out, batch.Eval{Suite: k.suite, Model: k.model, PromptVersion: k.version,
			RanAt: t.last.UTC().Format("2006-01-02T15:04:05Z"), PassRate: float64(t.pass) / float64(t.total)})
	}
	slices.SortFunc(out, func(a, b batch.Eval) int {
		return cmp.Or(cmp.Compare(a.Suite, b.Suite), cmp.Compare(a.Model, b.Model), cmp.Compare(a.PromptVersion, b.PromptVersion))
	})
	return out, nil
}

// paginate follows meta.cursor until it is null, retrying 429 and 5xx with backoff.
func paginate[T any](ctx context.Context, p *Puller, path string, q url.Values, each func(T)) error {
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = "https://cloud.langfuse.com"
	}
	cursor := ""
	for n := 0; ; n++ {
		if n == maxPages {
			return fmt.Errorf("langfuse: %s: more than %d pages", path, maxPages)
		}
		pq := url.Values{}
		for k, v := range q {
			pq[k] = v
		}
		if cursor != "" {
			pq.Set("cursor", cursor)
		}
		var pg page[T]
		if err := p.get(ctx, base+path+"?"+pq.Encode(), &pg); err != nil {
			return err
		}
		for _, item := range pg.Data {
			each(item)
		}
		if pg.Meta.Cursor == nil || *pg.Meta.Cursor == "" {
			return nil
		}
		cursor = *pg.Meta.Cursor
	}
}

func (p *Puller) get(ctx context.Context, u string, v any) error {
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	sleep := p.Sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	path := u
	if pu, err := url.Parse(u); err == nil {
		path = pu.Path
	}
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.SetBasicAuth(p.PublicKey, p.SecretKey)
		req.Header.Set("User-Agent", "tellproof-agent")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("langfuse: %s: %w", path, err)
		}
		if resp.StatusCode == http.StatusOK {
			err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(v)
			resp.Body.Close()
			if err != nil {
				return fmt.Errorf("langfuse: %s: decoding the response: %w", path, err)
			}
			return nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		resp.Body.Close()
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if !retry || attempt == maxAttempts {
			return fmt.Errorf("langfuse: %s: HTTP %d", path, resp.StatusCode)
		}
		wait := backoff
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			wait = time.Duration(s) * time.Second
		}
		if wait > time.Minute {
			wait = time.Minute
		}
		if err := sleep(ctx, wait); err != nil {
			return err
		}
		backoff *= 2
	}
}
