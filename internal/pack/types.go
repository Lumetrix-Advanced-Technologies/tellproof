package pack

// Statement and predicate types for pack v0.1 (spec/pack.schema.json,
// docs/architecture.md §2.3). Nullable fields are pointers without omitempty;
// optional fields have omitempty, so a decoded predicate re-encodes to the same
// canonical bytes.

const (
	StatementType = "https://in-toto.io/Statement/v1"
	// PredicateType uses a placeholder domain until D-008 is decided.
	PredicateType = "https://lumetrix.example/plumbline/pack/v0.1"
	PackSchema    = "assurance-pack/v0.1"
)

// Statement is an in-toto Statement v1 wrapping one pack.
type Statement struct {
	Type          string    `json:"_type"`
	Subject       []Subject `json:"subject"`
	PredicateType string    `json:"predicateType"`
	Predicate     Predicate `json:"predicate"`

	// canonicalPredicate is JCS(predicate) as signed, for the subject digest.
	canonicalPredicate []byte
}

// Subject names the pack and the digest of its predicate.
type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

// Predicate is one pack.
type Predicate struct {
	PackSchema       string         `json:"pack_schema"`
	PackID           string         `json:"pack_id"`
	SeqNo            int            `json:"seq_no"`
	PrevPackDigest   *string        `json:"prev_pack_digest"`
	IssuedAt         string         `json:"issued_at"`
	Period           Period         `json:"period"`
	SampleNotice     string         `json:"sample_notice,omitempty"`
	Subject          PackSubject    `json:"subject"`
	Profiles         []string       `json:"profiles"`
	AssuranceLevel   AssuranceLevel `json:"assurance_level"`
	Sources          []Source       `json:"sources"`
	Coverage         Coverage       `json:"coverage"`
	Lineage          []Lineage      `json:"lineage"`
	Changes          []Change       `json:"changes"`
	Incidents        []Incident     `json:"incidents"`
	Objectives       []Objective    `json:"objectives"`
	Commitments      Commitments    `json:"commitments"`
	Limitations      []string       `json:"limitations"`
	ObjectiveSetHash string         `json:"objective_set_hash,omitempty"`
	BatchDigests     []string       `json:"batch_digests,omitempty"`
}

type Period struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Timezone string `json:"timezone"`
}

type PackSubject struct {
	Organisation string `json:"organisation"`
	AISystem     string `json:"ai_system"`
	Environment  string `json:"environment"`
	Description  string `json:"description,omitempty"`
}

type AssuranceLevel struct {
	Level   int    `json:"level"`
	Meaning string `json:"meaning"`
}

type Source struct {
	Kind   string `json:"kind"`
	System string `json:"system"`
	Method string `json:"method"`
}

type Coverage struct {
	Basis     string             `json:"basis"`
	TotalPct  float64            `json:"total_pct"`
	Providers []ProviderCoverage `json:"providers"`
}

type ProviderCoverage struct {
	Provider       string `json:"provider"`
	Region         string `json:"region"`
	BilledTokens   int64  `json:"billed_tokens"`
	TracedTokens   int64  `json:"traced_tokens"`
	BilledRequests *int64 `json:"billed_requests"`
	TracedRequests *int64 `json:"traced_requests"`
}

type Lineage struct {
	Provider        string   `json:"provider"`
	Model           string   `json:"model"`
	Role            string   `json:"role"`
	Region          string   `json:"region"`
	ShareOfCallsPct *float64 `json:"share_of_calls_pct"`
}

type Change struct {
	Date string `json:"date"`
	Kind string `json:"kind"`
	What string `json:"what"`
}

type Incident struct {
	ID           string `json:"id"`
	Opened       string `json:"opened"`
	Acknowledged string `json:"acknowledged,omitempty"`
	Resolved     string `json:"resolved,omitempty"`
	Summary      string `json:"summary"`
}

type Objective struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	Threshold string   `json:"threshold"`
	Measured  string   `json:"measured"`
	Basis     string   `json:"basis"`
	Maps      []string `json:"maps"`
}

type Commitments struct {
	TraceMerkleRoot string     `json:"trace_merkle_root"`
	LeafCount       int        `json:"leaf_count"`
	Leaf            string     `json:"leaf"`
	Node            string     `json:"node"`
	Disclosed       *Disclosed `json:"disclosed"`
}

// Disclosed is one trace revealed with its inclusion proof.
type Disclosed struct {
	TraceID   string      `json:"trace_id"`
	LeafIndex int         `json:"leaf_index"`
	Salt      string      `json:"salt"`
	Proof     []ProofStep `json:"proof"`
}

type ProofStep struct {
	Side string `json:"side"`
	Hash string `json:"hash"`
}
