// Package pack decodes and verifies Tellproof evidence packs: in-toto
// statements in DSSE envelopes, chained by digest, with a disclosed trace
// proven against the pack's Merkle commitment (docs/architecture.md §2).
package pack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/dsse"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/jcs"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/merkle"
)

// Result is the outcome of every check, in the shape of manifest.json's
// "expect". Chain is null when no previous pack was given; Merkle is null when
// the pack discloses no trace (D-016).
type Result struct {
	Signature     bool  `json:"signature"`
	SubjectDigest bool  `json:"subject_digest"`
	Chain         *bool `json:"chain"`
	Merkle        *bool `json:"merkle"`
	Verified      bool  `json:"verified"`

	PackID string `json:"pack_id,omitempty"`
	SeqNo  int    `json:"seq_no,omitempty"`
	Digest string `json:"digest"`
	KeyID  string `json:"key_id,omitempty"`
	// SignatureErr says why Signature is false (see the dsse errors).
	SignatureErr error `json:"-"`
}

// Verify runs every check on env. Each check is computed on its own, so a pack
// that fails its signature still reports whether the rest holds. keys is the
// issuer's key set (ParseJWKS): the signature counts only if it verifies and
// its key may vouch for the pack's issued_at (D-042). prev is the previous pack
// in the chain, or nil to skip the chain check. An error means the payload is
// not a v0.1 pack statement and could not be evaluated; Verified is then false.
func Verify(env *dsse.Envelope, keys *KeySet, prev *dsse.Envelope) (Result, error) {
	if env == nil || keys == nil {
		return Result{}, errors.New("pack: nil envelope or key set")
	}
	r := Result{Digest: Digest(env)}
	_, r.KeyID, r.SignatureErr = dsse.Verify(env, dsse.PayloadTypeInToto, keys.PublicKeys())
	r.Signature = r.SignatureErr == nil

	st, err := DecodeStatement(env.Payload)
	if err != nil {
		return r, err
	}
	if r.Signature {
		if err := keys.Vouches(r.KeyID, st.Predicate.IssuedAt); err != nil {
			r.Signature, r.SignatureErr = false, err
		}
	}
	r.PackID, r.SeqNo = st.Predicate.PackID, st.Predicate.SeqNo
	r.SubjectDigest = CheckSubjectDigest(st)
	if prev != nil {
		ok := CheckChain(prev, st)
		r.Chain = &ok
	}
	if st.Predicate.Commitments.Disclosed != nil {
		ok := CheckDisclosure(st.Predicate.Commitments)
		r.Merkle = &ok
	}
	r.Verified = r.Signature && r.SubjectDigest && (r.Chain == nil || *r.Chain) && (r.Merkle == nil || *r.Merkle)
	return r, nil
}

// Digest is the pack digest used for chaining and checkpoints:
// "sha256:" + hex(sha256(payload)), where the payload is JCS(statement).
func Digest(env *dsse.Envelope) string {
	return "sha256:" + sha256Hex(env.Payload)
}

// CheckSubjectDigest reports whether the statement's subject digest is the
// SHA-256 of the canonical predicate as signed.
func CheckSubjectDigest(st *Statement) bool {
	return st.canonicalPredicate != nil && st.Subject[0].Digest["sha256"] == sha256Hex(st.canonicalPredicate)
}

// CheckChain reports whether next names prev as its previous pack.
func CheckChain(prev *dsse.Envelope, next *Statement) bool {
	p := next.Predicate.PrevPackDigest
	return prev != nil && p != nil && *p == Digest(prev)
}

// CheckDisclosure reports whether the disclosed trace proves to the
// commitment's root. The fields' format was checked by DecodeStatement.
func CheckDisclosure(c Commitments) bool {
	d := c.Disclosed
	if d == nil {
		return false
	}
	var salt [16]byte
	root, err1 := decodeHash(c.TraceMerkleRoot)
	_, err2 := hex.Decode(salt[:], []byte(d.Salt))
	if err1 != nil || err2 != nil || len(d.Salt) != 32 {
		return false
	}
	proof := make([]merkle.Step, len(d.Proof))
	for i, s := range d.Proof {
		h, err := decodeHash(s.Hash)
		if err != nil || len(s.Side) != 1 {
			return false
		}
		proof[i] = merkle.Step{Side: merkle.Side(s.Side[0]), Hash: h}
	}
	return merkle.VerifyProof(merkle.LeafHash(salt, d.TraceID), proof, root)
}

const maxSafeInteger = 1<<53 - 1

var (
	hex64     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex32     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	digestRef = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// DecodeStatement parses a pack statement strictly: I-JSON (no duplicate keys,
// valid Unicode), no fields outside the v0.1 schema, the expected statement and
// predicate types, one subject with a SHA-256 digest, and well-formed digests,
// salt and proof steps.
func DecodeStatement(payload []byte) (*Statement, error) {
	v, err := jcs.Parse(payload)
	if err != nil {
		return nil, fmt.Errorf("pack: statement: %w", err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("pack: statement: not a JSON object")
	}
	canonical, err := jcs.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("pack: statement: %w", err)
	}
	// The payload is defined as JCS(statement) (§2.3). Requiring it exactly is a
	// rule a browser can enforce the same way, with JSON.parse and jcs (D-018).
	if !bytes.Equal(canonical, payload) {
		return nil, errors.New("pack: statement: payload is not in canonical form (RFC 8785)")
	}
	var canonicalPredicate []byte
	if p, ok := obj["predicate"]; ok {
		if canonicalPredicate, err = jcs.Marshal(p); err != nil {
			return nil, fmt.Errorf("pack: statement: predicate: %w", err)
		}
	}

	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var st Statement
	if err := dec.Decode(&st); err != nil {
		return nil, fmt.Errorf("pack: statement: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("pack: statement: unexpected data after the JSON document")
	}
	// encoding/json matches keys case-insensitively and lets later duplicates
	// win, where other verifiers read exact keys. Require the typed statement to
	// re-encode to exactly what was signed, so every verifier reads the same pack.
	typed, err := json.Marshal(&st)
	if err == nil {
		typed, err = jcs.Canonicalize(typed)
	}
	if err != nil || !bytes.Equal(typed, payload) {
		return nil, errors.New("pack: statement: fields do not match the v0.1 types exactly (key case, or an empty optional field)")
	}
	st.canonicalPredicate = canonicalPredicate
	if err := st.validate(); err != nil {
		return nil, fmt.Errorf("pack: statement: %w", err)
	}
	return &st, nil
}

func (st *Statement) validate() error {
	p := st.Predicate
	switch {
	case st.Type != StatementType:
		return fmt.Errorf("_type is %q, want %q", st.Type, StatementType)
	case st.PredicateType != PredicateType:
		return fmt.Errorf("predicateType is %q, want %q", st.PredicateType, PredicateType)
	case len(st.Subject) != 1:
		return fmt.Errorf("want exactly one subject, got %d", len(st.Subject))
	case !hex64.MatchString(st.Subject[0].Digest["sha256"]):
		return errors.New("subject digest must have a lowercase hex sha256")
	case st.canonicalPredicate == nil:
		return errors.New("predicate is missing")
	case p.PackSchema != PackSchema:
		return fmt.Errorf("pack_schema is %q, want %q", p.PackSchema, PackSchema)
	case p.PrevPackDigest != nil && !digestRef.MatchString(*p.PrevPackDigest):
		return errors.New(`prev_pack_digest must be null or "sha256:" and 64 lowercase hex`)
	case !hex64.MatchString(p.Commitments.TraceMerkleRoot):
		return errors.New("commitments.trace_merkle_root must be 64 lowercase hex")
	case p.ObjectiveSetHash != "" && !digestRef.MatchString(p.ObjectiveSetHash):
		return errors.New(`objective_set_hash must be "sha256:" and 64 lowercase hex`)
	}
	// encoding/json decodes null into a nil slice and re-encodes it as null, so
	// the exact-bytes check alone would let a required array be null.
	for name, isNil := range map[string]bool{
		"profiles": p.Profiles == nil, "sources": p.Sources == nil, "coverage.providers": p.Coverage.Providers == nil,
		"lineage": p.Lineage == nil, "changes": p.Changes == nil, "incidents": p.Incidents == nil,
		"objectives": p.Objectives == nil, "limitations": p.Limitations == nil,
	} {
		if isNil {
			return fmt.Errorf("%s must be an array, not null", name)
		}
	}
	// JavaScript numbers hold integers exactly only up to 2^53-1; beyond that the
	// browser verifier would read a different value (D-018).
	ints := map[string]int64{"seq_no": int64(p.SeqNo), "assurance_level.level": int64(p.AssuranceLevel.Level), "commitments.leaf_count": int64(p.Commitments.LeafCount)}
	if d := p.Commitments.Disclosed; d != nil {
		ints["commitments.disclosed.leaf_index"] = int64(d.LeafIndex)
	}
	for i, c := range p.Coverage.Providers {
		ints[fmt.Sprintf("coverage.providers[%d].billed_tokens", i)] = c.BilledTokens
		ints[fmt.Sprintf("coverage.providers[%d].traced_tokens", i)] = c.TracedTokens
		if c.BilledRequests != nil {
			ints[fmt.Sprintf("coverage.providers[%d].billed_requests", i)] = *c.BilledRequests
		}
		if c.TracedRequests != nil {
			ints[fmt.Sprintf("coverage.providers[%d].traced_requests", i)] = *c.TracedRequests
		}
	}
	for name, v := range ints {
		if v > maxSafeInteger || v < -maxSafeInteger {
			return fmt.Errorf("%s is outside the safe integer range", name)
		}
	}
	for i, o := range p.Objectives {
		if o.Maps == nil {
			return fmt.Errorf("objectives[%d].maps must be an array, not null", i)
		}
	}
	if d := p.Commitments.Disclosed; d != nil && d.Proof == nil {
		return errors.New("commitments.disclosed.proof must be an array, not null")
	}
	for _, d := range p.BatchDigests {
		if !digestRef.MatchString(d) {
			return fmt.Errorf("batch_digests entry %q is not a sha256 digest", d)
		}
	}
	if d := p.Commitments.Disclosed; d != nil {
		if !hex32.MatchString(d.Salt) {
			return errors.New("commitments.disclosed.salt must be 32 lowercase hex")
		}
		for i, s := range d.Proof {
			if s.Side != string(merkle.Left) && s.Side != string(merkle.Right) {
				return fmt.Errorf("commitments.disclosed.proof[%d].side is %q, want L or R", i, s.Side)
			}
			if !hex64.MatchString(s.Hash) {
				return fmt.Errorf("commitments.disclosed.proof[%d].hash must be 64 lowercase hex", i)
			}
		}
	}
	return nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func decodeHash(s string) (merkle.Hash, error) {
	var h merkle.Hash
	if len(s) != 2*len(h) {
		return h, errors.New("wrong length")
	}
	_, err := hex.Decode(h[:], []byte(s))
	return h, err
}
