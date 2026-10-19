package otlp

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
)

// Kind classifies a span for the objectives.
type Kind string

const (
	KindOther Kind = "other"
	KindLLM   Kind = "llm"   // a model call (chat, completion, embeddings)
	KindAgent Kind = "agent" // invoke_agent
	KindTool  Kind = "tool"  // execute_tool
)

// Span holds the only facts kept about a span: no names, no free text, no
// content. TraceID stays inside the agent (it becomes a salted Merkle leaf).
type Span struct {
	TraceID        string // lower-case hex
	Start          time.Time
	Kind           Kind
	Operation      string
	Provider       string
	Model          string
	Region         string
	InputTokens    int64
	OutputTokens   int64
	Error          bool
	ContentPresent bool
	PII            bool
	Redacted       bool
	Guardrail      string // decision on the LLM call: allow | block | flag; "" if none
	PromptName     string
	PromptVersion  string
	Slot           string // agent name, else service.name: model transitions are per slot
	HighImpact     bool
	Approved       bool
	Escalation     bool
	CanaryKind     string // a guardrail canary's kind (batch.CanaryKind), "" if not a canary
	CanaryCaught   bool
}

// Result is one parsed export. Rejected counts spans that could not be used
// (bad trace ID, missing start time); OTLP reports them as partial success.
type Result struct {
	Spans    []Span
	Rejected int
}

type keyValue struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"` // decoded only for readable, non-content keys
}

type exportRequest struct {
	ResourceSpans []struct {
		Resource struct {
			Attributes []keyValue `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Spans []rawSpan `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

type rawSpan struct {
	TraceID string          `json:"traceId"`
	Start   json.RawMessage `json:"startTimeUnixNano"`
	Attrs   []keyValue      `json:"attributes"`
	Events  []struct {
		Attrs []keyValue `json:"attributes"`
	} `json:"events"`
	Status struct {
		Code json.RawMessage `json:"code"`
	} `json:"status"`
}

// Parse decodes one ExportTraceServiceRequest in the OTLP JSON mapping.
func Parse(body []byte, m *Mapping) (Result, error) {
	var req exportRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&req); err != nil {
		return Result{}, errors.New("otlp: body is not an OTLP JSON trace export")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Result{}, errors.New("otlp: unexpected data after the export")
	}
	var res Result
	for _, rs := range req.ResourceSpans {
		resVals, _ := m.read(rs.Resource.Attributes)
		for _, ss := range rs.ScopeSpans {
			for _, raw := range ss.Spans {
				s, ok := m.normalise(raw, resVals)
				if !ok {
					res.Rejected++
					continue
				}
				res.Spans = append(res.Spans, s)
			}
		}
	}
	return res, nil
}

// value is one decoded attribute value; only scalars are ever decoded.
type value struct {
	s     string
	n     float64
	isNum bool
	b     bool
	isB   bool
}

// read decodes the readable attributes and reports whether any content
// attribute is present. Content and unmapped values are skipped undecoded.
func (m *Mapping) read(kvs []keyValue) (map[string]value, bool) {
	vals := map[string]value{}
	content := false
	for _, kv := range kvs {
		if m.isContent(kv.Key) {
			content = true
			continue
		}
		if !m.canRead(kv.Key) {
			continue
		}
		if v, ok := decodeScalar(kv.Value); ok {
			vals[kv.Key] = v
		}
	}
	return vals, content
}

func decodeScalar(raw json.RawMessage) (value, bool) {
	var any struct {
		String *string          `json:"stringValue"`
		Bool   *bool            `json:"boolValue"`
		Int    *json.RawMessage `json:"intValue"` // a string or a number in OTLP JSON
		Double *float64         `json:"doubleValue"`
	}
	if json.Unmarshal(raw, &any) != nil {
		return value{}, false
	}
	switch {
	case any.String != nil:
		return value{s: *any.String}, true
	case any.Bool != nil:
		return value{b: *any.Bool, isB: true}, true
	case any.Double != nil:
		return value{n: *any.Double, isNum: true}, true
	case any.Int != nil:
		if n, ok := parseInt(*any.Int); ok {
			return value{n: float64(n), isNum: true, s: strconv.FormatInt(n, 10)}, true
		}
	}
	return value{}, false
}

func parseInt(raw json.RawMessage) (int64, bool) {
	s := strings.Trim(string(raw), `"`)
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

func (m *Mapping) pick(field string, vals ...map[string]value) (value, bool) {
	for _, vs := range vals {
		for _, name := range m.fields[field] {
			if v, ok := vs[name]; ok && (v.isNum || v.isB || v.s != "") {
				return v, true
			}
		}
	}
	return value{}, false
}

func (m *Mapping) str(field string, vals ...map[string]value) string {
	if v, ok := m.pick(field, vals...); ok && !v.isNum && !v.isB {
		return v.s
	}
	return ""
}

func (m *Mapping) count(field string, vals ...map[string]value) int64 {
	v, ok := m.pick(field, vals...)
	if !ok {
		return 0
	}
	if v.isNum {
		if v.n < 0 || v.n > math.MaxInt64 || v.n != math.Trunc(v.n) {
			return 0
		}
		return int64(v.n)
	}
	if n, err := strconv.ParseInt(v.s, 10, 64); err == nil && n >= 0 {
		return n
	}
	return 0
}

func (m *Mapping) flag(field string, vals ...map[string]value) bool {
	v, ok := m.pick(field, vals...)
	return ok && ((v.isB && v.b) || (!v.isB && !v.isNum && strings.EqualFold(v.s, "true")))
}

func (m *Mapping) normalise(raw rawSpan, resVals map[string]value) (Span, bool) {
	traceID := strings.ToLower(raw.TraceID)
	if b, err := hex.DecodeString(traceID); err != nil || len(b) != 16 || traceID == strings.Repeat("0", 32) {
		return Span{}, false
	}
	start, ok := parseInt(raw.Start)
	if !ok || start <= 0 {
		return Span{}, false
	}
	spanVals, content := m.read(raw.Attrs)
	for _, ev := range raw.Events { // older SDKs put content in span events
		if _, c := m.read(ev.Attrs); c {
			content = true
		}
	}
	return m.build(traceID, time.Unix(0, start).UTC(), spanVals, resVals, statusError(raw.Status.Code), content), true
}

// Build makes a Span from attributes other sources already decoded (the
// Langfuse puller): Go strings, bools and numbers by attribute name. Like the
// OTLP path, it reads only mapped names, and a content attribute among them
// only marks the span as carrying content. traceID must be non-empty.
func (m *Mapping) Build(traceID string, start time.Time, attrs, resource map[string]any, isError, content bool) (Span, bool) {
	if traceID == "" || len(traceID) > 256 || start.IsZero() {
		return Span{}, false
	}
	conv := func(in map[string]any) map[string]value {
		out := map[string]value{}
		for k, v := range in {
			if m.isContent(k) {
				content = true
				continue
			}
			if !m.canRead(k) {
				continue
			}
			switch x := v.(type) {
			case string:
				out[k] = value{s: x}
			case bool:
				out[k] = value{b: x, isB: true}
			case float64:
				out[k] = value{n: x, isNum: true}
			case int:
				out[k] = value{n: float64(x), isNum: true}
			case int64:
				out[k] = value{n: float64(x), isNum: true}
			}
		}
		return out
	}
	spanVals, resVals := conv(attrs), conv(resource)
	return m.build(traceID, start.UTC(), spanVals, resVals, isError, content), true
}

func (m *Mapping) build(traceID string, start time.Time, spanVals, resVals map[string]value, isError, content bool) Span {
	// Span attributes win over resource attributes.
	op := m.str("operation", spanVals, resVals)
	kind := KindOther
	switch {
	case op == "invoke_agent":
		kind = KindAgent
	case op == "execute_tool":
		kind = KindTool
	case m.llmOps[op]:
		kind = KindLLM
	}
	model := m.str("response_model", spanVals, resVals)
	if model == "" {
		model = m.str("request_model", spanVals, resVals)
	}
	// What a batch carries must be an identifier (D-065): content or a
	// credential in one of these attributes becomes "invalid".
	id := defaultIdentifiers
	s := Span{
		TraceID:        traceID,
		Start:          start,
		Kind:           kind,
		Operation:      op,
		Provider:       orUnknown(id.clean(m.str("provider", spanVals, resVals))),
		Model:          orUnknown(id.clean(model)),
		Region:         orUnknown(id.clean(m.str("region", spanVals, resVals))),
		InputTokens:    m.count("input_tokens", spanVals, resVals),
		OutputTokens:   m.count("output_tokens", spanVals, resVals),
		Error:          isError,
		ContentPresent: content,
		PII:            slices.Contains(piiValues(), m.str("pii_marker", spanVals, resVals)),
		Redacted:       m.flag("redacted", spanVals, resVals),
		Guardrail:      m.str("guardrail_decision", spanVals),
		PromptName:     m.str("prompt_name", spanVals, resVals),
		PromptVersion:  id.clean(m.str("prompt_version", spanVals, resVals)),
		Slot:           m.str("agent_slot", spanVals, resVals),
		HighImpact:     m.flag("tool_high_impact", spanVals),
		Approved:       m.flag("tool_approved", spanVals),
		Escalation:     m.flag("escalation", spanVals),
	}
	if _, ok := m.pick("canary_kind", spanVals); ok {
		kind := m.str("canary_kind", spanVals)
		if !id.valid(kind) {
			kind = "" // content or a credential is no kind: "other"
		}
		s.CanaryKind = batch.CanaryKind(strings.ToLower(kind))
		s.CanaryCaught = m.flag("canary_caught", spanVals)
	}
	if s.Slot == "" {
		if v, ok := resVals["service.name"]; ok && !v.isNum && !v.isB {
			s.Slot = v.s
		} else if v, ok := spanVals["service.name"]; ok && !v.isNum && !v.isB {
			s.Slot = v.s
		}
	}
	return s
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// statusError: OTLP JSON status codes arrive as numbers (2) or enum names.
func statusError(raw json.RawMessage) bool {
	s := strings.Trim(string(raw), `"`)
	return s == "2" || s == "STATUS_CODE_ERROR"
}

// errTooLarge is returned by the receiver for oversized bodies.
var errTooLarge = fmt.Errorf("otlp: request body too large")
