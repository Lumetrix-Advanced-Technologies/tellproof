// Package batchsign turns a closed window into a signed agent batch: the v0.2
// payload in canonical JSON, a DSSE envelope signed with the agent's Ed25519
// key under its agent ID, chained by seq and prev (docs/architecture.md §2.5).
// Verify is the matching server-side check (P4-2).
package batchsign

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/aggregate"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/batch"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/dsse"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/enrol"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/jcs"
)

// ErrInvalid: the envelope verified but its payload is not a valid v0.2 batch.
var ErrInvalid = errors.New("batchsign: invalid batch payload")

// State is the chain position after the last batch issued: its seq and the
// digest of its payload. The zero State means no batch yet.
type State struct {
	Seq  int64   `json:"seq"`
	Prev *string `json:"prev"`
}

// Builder signs batches for one enrolled agent.
type Builder struct {
	agentID    string
	semconvMap string
	signer     *dsse.Ed25519Signer
}

// NewBuilder returns a Builder for id, recording which semconv map version
// produced its metrics.
func NewBuilder(id *enrol.Identity, semconvMapVersion string) (*Builder, error) {
	s, err := dsse.NewEd25519Signer(id.Key, id.AgentID)
	if err != nil {
		return nil, err
	}
	return &Builder{agentID: id.AgentID, semconvMap: semconvMapVersion, signer: s}, nil
}

// Build makes the next batch after st. It returns the envelope, its payload
// and the State to persist once the envelope is safely queued.
func (b *Builder) Build(ctx context.Context, st State, w aggregate.Window, commit batch.Commit, usage []batch.Usage) (*dsse.Envelope, []byte, State, error) {
	if usage == nil {
		usage = []batch.Usage{}
	}
	bt := batch.Batch{Schema: batch.Schema, AgentID: b.agentID, Seq: st.Seq + 1, Prev: st.Prev, Window: w.Window,
		SemconvMap: b.semconvMap, Metrics: w.Metrics, Observed: w.Observed, Usage: usage, Commit: commit}
	raw, err := json.Marshal(bt)
	if err != nil {
		return nil, nil, State{}, err
	}
	payload, err := jcs.Canonicalize(raw)
	if err != nil {
		return nil, nil, State{}, fmt.Errorf("batchsign: canonical form: %w", err)
	}
	// Every number must survive JSON exactly (counts above 2^53 would not).
	var back batch.Batch
	if err := json.Unmarshal(payload, &back); err != nil {
		return nil, nil, State{}, err
	}
	again, _ := json.Marshal(back)
	if !bytes.Equal(again, raw) {
		return nil, nil, State{}, errors.New("batchsign: a count is too large to encode exactly")
	}
	env, err := dsse.Sign(ctx, b.signer, batch.PayloadType, payload)
	if err != nil {
		return nil, nil, State{}, err
	}
	d := Digest(env)
	return env, payload, State{Seq: bt.Seq, Prev: &d}, nil
}

// Digest is a batch's digest, "sha256:" + hex(sha256(payload)): what the next
// batch's prev names and what the server returns on ingest.
func Digest(env *dsse.Envelope) string {
	sum := sha256.Sum256(env.Payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Verify checks a batch envelope against an agent's enrolled public key and
// decodes it strictly: signature by agentID, canonical payload, no unknown
// fields, schema v0.2, and agent_id equal to the signer.
func Verify(env *dsse.Envelope, pub ed25519.PublicKey, agentID string) (*batch.Batch, error) {
	payload, _, err := dsse.Verify(env, batch.PayloadType, map[string]crypto.PublicKey{agentID: pub})
	if err != nil {
		return nil, err
	}
	canon, err := jcs.Canonicalize(payload)
	if err != nil || !bytes.Equal(canon, payload) {
		return nil, fmt.Errorf("%w: not in canonical form", ErrInvalid)
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var bt batch.Batch
	if err := dec.Decode(&bt); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data", ErrInvalid)
	}
	switch {
	case bt.Schema != batch.Schema:
		return nil, fmt.Errorf("%w: schema %q, want %q", ErrInvalid, bt.Schema, batch.Schema)
	case bt.AgentID != agentID:
		return nil, fmt.Errorf("%w: agent_id %q is not the signer", ErrInvalid, bt.AgentID)
	case bt.Seq < 1:
		return nil, fmt.Errorf("%w: seq must be at least 1", ErrInvalid)
	}
	return &bt, nil
}
