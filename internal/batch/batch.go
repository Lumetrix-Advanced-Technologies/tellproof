// Package batch holds the agent batch payload, schema plumbline-batch/v0.2
// (spec/batch.schema.json, docs/architecture.md §2.5), or v0.3 for a window
// with guardrail canaries (spec/batch-v0.3.schema.json): one hour of aggregates
// from one agent. It carries counts, ratios' parts, version labels and a
// salted Merkle root; never content and never trace IDs.
package batch

// Schema is the payload version this package writes for a window without
// canaries; SchemaCanaries for one with them (D-057). A window without
// canaries is byte-for-byte what a v0.2 agent writes, so a server that
// predates canaries keeps accepting every batch except those.
const (
	Schema         = "plumbline-batch/v0.2"
	SchemaCanaries = "plumbline-batch/v0.3"
)

// SchemaFor is the payload version for metrics m.
func SchemaFor(m Metrics) string {
	if len(m.Canaries) > 0 {
		return SchemaCanaries
	}
	return Schema
}

// PayloadType is the DSSE payload type of a signed batch.
const PayloadType = "application/vnd.lumetrix.plumbline.batch+json"

// Batch is the signed payload.
type Batch struct {
	Schema     string   `json:"schema"`
	AgentID    string   `json:"agent_id"`
	Seq        int64    `json:"seq"`
	Prev       *string  `json:"prev"` // "sha256:<hex>" of the previous batch payload, or null for the first
	Window     Window   `json:"window"`
	SemconvMap string   `json:"semconv_map"`
	Metrics    Metrics  `json:"metrics"`
	Observed   Observed `json:"observed"`
	Usage      []Usage  `json:"usage"`
	Commit     Commit   `json:"commit"`
}

// Window is a half-open UTC hour, RFC 3339 with a Z.
type Window struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// Metrics is everything the objectives measure for one window.
type Metrics struct {
	Tokens              []TokenCount `json:"tokens"`
	PII                 PII          `json:"pii"`
	ContentPresentSpans int64        `json:"content_present_spans"`
	Guardrails          Guardrails   `json:"guardrails"`
	Evals               []Eval       `json:"evals"`
	Agent               AgentCounts  `json:"agent"`
	Transitions         []Transition `json:"transitions"`
	Anomalies           []Anomaly    `json:"anomalies"`
	LateSpans           int64        `json:"late_spans"`
	Canaries            []Canary     `json:"canaries,omitempty"` // v0.3 only
}

// Canary counts the guardrail canaries of one kind in the window: synthetic,
// known-bad probes the app's SDK sent through its own guardrail, and how many
// the guardrail caught. The probes never leave the app; only these counts do.
type Canary struct {
	Kind   string `json:"kind"`
	Run    int64  `json:"run"`
	Caught int64  `json:"caught"`
}

// MaxCanaryKinds bounds the kinds per window; the rest count as "other".
const MaxCanaryKinds = 16

// CanaryKind normalises a kind name: 1 to 32 of a-z, 0-9, '_' and '-', else "other".
func CanaryKind(s string) string {
	if s == "" || len(s) > 32 {
		return "other"
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return "other"
		}
	}
	return s
}

type TokenCount struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Region       string `json:"region"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Calls        int64  `json:"calls"`
}

type PII struct {
	Spans         int64 `json:"pii_spans"`
	SpansRedacted int64 `json:"pii_spans_redacted"`
}

type Guardrails struct {
	LLMCalls     int64 `json:"llm_calls"`
	GuardedCalls int64 `json:"guarded_calls"`
	Blocked      int64 `json:"blocked"`
}

type Eval struct {
	Suite         string  `json:"suite"`
	Model         string  `json:"model"`
	PromptVersion string  `json:"prompt_version"`
	RanAt         string  `json:"ran_at"`
	PassRate      float64 `json:"pass_rate"`
}

type AgentCounts struct {
	Runs               int64 `json:"runs"`
	ToolCalls          int64 `json:"tool_calls"`
	ToolErrors         int64 `json:"tool_errors"`
	HighImpactCalls    int64 `json:"high_impact_calls"`
	HighImpactApproved int64 `json:"high_impact_approved"`
	Escalations        int64 `json:"escalations"`
}

// Transition is a model change within a slot, or a prompt version change.
type Transition struct {
	At   string `json:"at"`
	Kind string `json:"kind"` // model | prompt
	From string `json:"from"`
	To   string `json:"to"`
}

type Anomaly struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Kind  string `json:"kind"` // error_rate | guardrail_rate
}

// Observed lists what was live in the window (v0.2, for eval freshness).
type Observed struct {
	Models   []ObservedModel   `json:"models"`
	Versions []ObservedVersion `json:"versions"`
}

type ObservedModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type ObservedVersion struct {
	Model         string `json:"model"`
	PromptVersion string `json:"prompt_version"`
}

// Usage is provider-billed usage for reconciliation (P3-4).
type Usage struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Region       string `json:"region,omitempty"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Requests     *int64 `json:"requests"`
}

// Commit is the window's salted Merkle commitment (P3-3).
type Commit struct {
	Root   string `json:"root"`
	Leaves int64  `json:"leaves"`
}
