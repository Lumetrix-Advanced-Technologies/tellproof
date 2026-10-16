// Package checkpoint builds and verifies the daily checkpoints
// (docs/architecture.md §4.5, D-043): one signed statement per local day
// whose Merkle root covers the digest of every pack issued that day, in issue
// order, and which names the previous day's checkpoint. Published in a public
// repository, the chain of checkpoints is an append-only log: anyone can prove
// a pack existed by a given day, and the issuer cannot quietly drop or reorder
// packs it has already published.
package checkpoint

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"time"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/dsse"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/jcs"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/merkle"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/pack"
)

const (
	// PredicateType uses the placeholder domain until D-008 is decided.
	PredicateType = "https://lumetrix.example/plumbline/checkpoint/v0.1"
	Schema        = "plumbline-checkpoint/v0.1"
	LeafRule      = `sha256(0x00 || pack_digest), pack_digest as ASCII "sha256:<hex>"`
	NodeRule      = "sha256(0x01 || left || right); odd node carried up; all-zero root when no packs"
)

// Predicate is one day's checkpoint.
type Predicate struct {
	CheckpointSchema     string   `json:"checkpoint_schema"`
	Day                  string   `json:"day"`      // YYYY-MM-DD, local
	Timezone             string   `json:"timezone"` // the day's timezone
	IssuedAt             string   `json:"issued_at"`
	PrevCheckpointDigest *string  `json:"prev_checkpoint_digest"`
	PackCount            int      `json:"pack_count"`
	Root                 string   `json:"root"`
	Leaf                 string   `json:"leaf"`
	Node                 string   `json:"node"`
	PackDigests          []string `json:"pack_digests"`
}

var (
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	dayRE    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// Leaf is a pack digest's leaf.
func Leaf(packDigest string) merkle.Hash {
	return sha256.Sum256(append([]byte{0x00}, packDigest...))
}

func tree(digests []string) (*merkle.Tree, error) {
	leaves := make([]merkle.Hash, len(digests))
	for i, d := range digests {
		leaves[i] = Leaf(d)
	}
	return merkle.NewTree(leaves)
}

// Root is the Merkle root over the digests in order, hex; all zeros for none.
func Root(digests []string) (string, error) {
	if len(digests) == 0 {
		return hex.EncodeToString(make([]byte, 32)), nil
	}
	t, err := tree(digests)
	if err != nil {
		return "", err
	}
	r := t.Root()
	return hex.EncodeToString(r[:]), nil
}

// Build makes a day's predicate.
func Build(day, timezone string, issuedAt time.Time, digests []string, prev *string) (Predicate, error) {
	root, err := Root(digests)
	if err != nil {
		return Predicate{}, err
	}
	if digests == nil {
		digests = []string{}
	}
	return Predicate{CheckpointSchema: Schema, Day: day, Timezone: timezone, IssuedAt: issuedAt.Format(time.RFC3339),
		PrevCheckpointDigest: prev, PackCount: len(digests), Root: root, Leaf: LeafRule, Node: NodeRule, PackDigests: digests}, nil
}

// Statement is JCS of the in-toto statement wrapping p (subject "checkpoint-<day>").
func Statement(p Predicate) ([]byte, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	cp, err := jcs.Canonicalize(raw)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(cp)
	st := map[string]any{"_type": pack.StatementType, "predicateType": PredicateType, "predicate": json.RawMessage(cp),
		"subject": []any{map[string]any{"name": "checkpoint-" + p.Day, "digest": map[string]string{"sha256": hex.EncodeToString(sum[:])}}}}
	raw, err = json.Marshal(st)
	if err != nil {
		return nil, err
	}
	return jcs.Canonicalize(raw)
}

// Digest is a checkpoint's digest, "sha256:" + hex(sha256(payload)), as the next day names it.
func Digest(env *dsse.Envelope) string {
	sum := sha256.Sum256(env.Payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Decode parses a checkpoint statement strictly: canonical, no unknown
// fields, the checkpoint types, well-formed digests, and a root and count
// that match the listed digests.
func Decode(payload []byte) (*Predicate, error) {
	c, err := jcs.Canonicalize(payload)
	if err != nil || !bytes.Equal(c, payload) {
		return nil, errors.New("checkpoint: the payload is not canonical JSON")
	}
	var st struct {
		Type          string          `json:"_type"`
		PredicateType string          `json:"predicateType"`
		Predicate     json.RawMessage `json:"predicate"`
		Subject       []struct {
			Name   string            `json:"name"`
			Digest map[string]string `json:"digest"`
		} `json:"subject"`
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return nil, fmt.Errorf("checkpoint: statement: %w", err)
	}
	if st.Type != pack.StatementType || st.PredicateType != PredicateType || len(st.Subject) != 1 {
		return nil, errors.New("checkpoint: not a checkpoint statement")
	}
	var p Predicate
	pd := json.NewDecoder(bytes.NewReader(st.Predicate))
	pd.DisallowUnknownFields()
	if err := pd.Decode(&p); err != nil {
		return nil, fmt.Errorf("checkpoint: predicate: %w", err)
	}
	if _, err := pd.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("checkpoint: trailing data")
	}
	sum := sha256.Sum256(st.Predicate)
	switch {
	case st.Subject[0].Name != "checkpoint-"+p.Day || st.Subject[0].Digest["sha256"] != hex.EncodeToString(sum[:]):
		return nil, errors.New("checkpoint: the subject does not match the predicate")
	case p.CheckpointSchema != Schema || !dayRE.MatchString(p.Day):
		return nil, errors.New("checkpoint: wrong schema or day")
	case p.PrevCheckpointDigest != nil && !digestRE.MatchString(*p.PrevCheckpointDigest):
		return nil, errors.New("checkpoint: prev_checkpoint_digest is malformed")
	case p.PackCount != len(p.PackDigests):
		return nil, errors.New("checkpoint: pack_count does not match the listed digests")
	}
	seen := map[string]bool{}
	for _, d := range p.PackDigests {
		if !digestRE.MatchString(d) || seen[d] {
			return nil, fmt.Errorf("checkpoint: pack digest %q is malformed or listed twice", d)
		}
		seen[d] = true
	}
	root, err := Root(p.PackDigests)
	if err != nil || root != p.Root {
		return nil, errors.New("checkpoint: the root does not match the listed digests")
	}
	return &p, nil
}

// Verify checks a checkpoint envelope: the issuer's signature by a key that
// may vouch for its issued_at (D-042), the statement (Decode), and, when prev
// is given, that it names prev.
func Verify(env *dsse.Envelope, keys *pack.KeySet, prev *dsse.Envelope) (*Predicate, error) {
	_, kid, err := dsse.Verify(env, dsse.PayloadTypeInToto, keys.PublicKeys())
	if err != nil {
		return nil, fmt.Errorf("checkpoint: signature: %w", err)
	}
	p, err := Decode(env.Payload)
	if err != nil {
		return nil, err
	}
	if err := keys.Vouches(kid, p.IssuedAt); err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	if prev != nil && (p.PrevCheckpointDigest == nil || *p.PrevCheckpointDigest != Digest(prev)) {
		return nil, errors.New("checkpoint: it does not follow the previous checkpoint")
	}
	return p, nil
}

// Proof is a pack's inclusion proof in a checkpoint.
type Proof struct {
	PackDigest string
	Index      int
	Steps      []merkle.Step
}

// Prove finds packDigest in p and returns its proof, checked against p.Root.
func Prove(p *Predicate, packDigest string) (*Proof, error) {
	i := slices.Index(p.PackDigests, packDigest)
	if i < 0 {
		return nil, fmt.Errorf("checkpoint: pack %s is not in the checkpoint for %s", packDigest, p.Day)
	}
	t, err := tree(p.PackDigests)
	if err != nil {
		return nil, err
	}
	steps, err := t.Proof(i)
	if err != nil {
		return nil, err
	}
	raw, _ := hex.DecodeString(p.Root)
	if !merkle.VerifyProof(Leaf(packDigest), steps, merkle.Hash(raw)) {
		return nil, errors.New("checkpoint: the inclusion proof does not reach the root")
	}
	return &Proof{PackDigest: packDigest, Index: i, Steps: steps}, nil
}
