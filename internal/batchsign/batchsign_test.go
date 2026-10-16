package batchsign

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/aggregate"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/batch"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/commitment"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/dsse"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/enrol"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/jcs"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/otlp"
)

const agentID = "agt_0123456789abcdef"

func identity(t *testing.T) *enrol.Identity {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &enrol.Identity{AgentID: agentID, Server: "https://x", Key: priv}
}

func sampleWindows(t *testing.T) []aggregate.Window {
	t.Helper()
	m, _ := otlp.DefaultMapping()
	b, err := os.ReadFile(filepath.Join("..", "..", "concierge", "testdata", "otlp-sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	a := aggregate.New(aggregate.DefaultConfig())
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			res, _ := otlp.Parse(line, m)
			a.Add(res.Spans, res.Spans[0].Start)
		}
	}
	return a.FlushAll()
}

func build(t *testing.T, id *enrol.Identity) (envs []*dsse.Envelope, payloads [][]byte, states []State) {
	t.Helper()
	b, err := NewBuilder(id, "v2")
	if err != nil {
		t.Fatal(err)
	}
	st := State{}
	secret := []byte("batch-test-secret-0123")
	for _, w := range sampleWindows(t) {
		env, payload, next, err := b.Build(context.Background(), st, w, commitment.ForWindow(secret, w.TraceIDs).Commit, nil)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		envs, payloads, states = append(envs, env), append(payloads, payload), append(states, next)
		st = next
	}
	return
}

func TestBatchesChainAndVerify(t *testing.T) {
	id := identity(t)
	envs, payloads, states := build(t, id)
	if len(envs) != 2 {
		t.Fatalf("%d batches", len(envs))
	}
	pub := id.Key.Public().(ed25519.PublicKey)
	for i, env := range envs {
		got, err := Verify(env, pub, agentID)
		if err != nil {
			t.Fatalf("batch %d: Verify: %v", i, err)
		}
		if got.Seq != int64(i+1) || got.AgentID != agentID || got.Schema != batch.Schema {
			t.Errorf("batch %d: seq %d agent %s schema %s", i, got.Seq, got.AgentID, got.Schema)
		}
		sum := sha256.Sum256(payloads[i])
		if want := "sha256:" + hex.EncodeToString(sum[:]); states[i].Prev == nil || *states[i].Prev != want || Digest(env) != want {
			t.Errorf("batch %d: state prev %v, digest %s", i, states[i].Prev, Digest(env))
		}
		if i == 0 && got.Prev != nil {
			t.Error("first batch has a prev")
		}
		if i == 1 && (got.Prev == nil || *got.Prev != *states[0].Prev) {
			t.Errorf("second batch prev %v, want %s", got.Prev, *states[0].Prev)
		}
		if env.PayloadType != batch.PayloadType || env.Signatures[0].KeyID != agentID {
			t.Errorf("envelope type %s keyid %s", env.PayloadType, env.Signatures[0].KeyID)
		}
		if canon, err := jcs.Canonicalize(payloads[i]); err != nil || !bytes.Equal(canon, payloads[i]) {
			t.Errorf("batch %d payload is not canonical", i)
		}
	}
}

// Every batch payload validates against spec/batch.schema.json (checked with
// the concierge's schema validator).
func TestPayloadsMatchTheBatchSchema(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	_, payloads, _ := build(t, identity(t))
	for i, p := range payloads {
		script := "import sys, json; sys.path.insert(0, 'concierge'); import schema; " +
			"e = schema.errors(json.loads(sys.stdin.read()), json.load(open('spec/batch.schema.json'))); print('\\n'.join(e)); sys.exit(1 if e else 0)"
		cmd := exec.Command(py, "-c", script)
		cmd.Dir = filepath.Join("..", "..")
		cmd.Stdin = bytes.NewReader(p)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("batch %d does not match the schema: %s", i, out)
		}
	}
}

func TestVerifyRejects(t *testing.T) {
	id := identity(t)
	envs, _, _ := build(t, id)
	pub := id.Key.Public().(ed25519.PublicKey)
	other, _, _ := ed25519.GenerateKey(rand.Reader)

	resign := func(payload []byte) *dsse.Envelope {
		s, _ := dsse.NewEd25519Signer(id.Key, agentID)
		env, err := dsse.Sign(context.Background(), s, batch.PayloadType, payload)
		if err != nil {
			t.Fatal(err)
		}
		return env
	}
	tampered := *envs[0]
	tampered.Payload = bytes.Replace(bytes.Clone(envs[0].Payload), []byte(`"late_spans":0`), []byte(`"late_spans":9`), 1)
	cases := []struct {
		name string
		env  *dsse.Envelope
		pub  ed25519.PublicKey
		id   string
		want error
	}{
		{"tampered payload", &tampered, pub, agentID, dsse.ErrBadSignature},
		{"another agent's key", envs[0], other, agentID, dsse.ErrBadSignature},
		{"keyid is another agent", envs[0], pub, "agt_zzzzzzzzzzzz", dsse.ErrUnknownKey},
		{"unknown field", resign(bytes.Replace(envs[0].Payload, []byte(`{"agent_id"`), []byte(`{"extra":1,"agent_id"`), 1)), pub, agentID, ErrInvalid},
		{"old schema version", resign(bytes.Replace(envs[0].Payload, []byte(batch.Schema), []byte("plumbline-batch/v0.1"), 1)), pub, agentID, ErrInvalid},
		{"not canonical", resign(append([]byte(" "), envs[0].Payload...)), pub, agentID, ErrInvalid},
		{"agent_id differs from the signer", resign(bytes.Replace(envs[0].Payload, []byte(agentID), []byte("agt_abcdefghij"), 1)), pub, agentID, ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Verify(c.env, c.pub, c.id); !errors.Is(err, c.want) {
				t.Errorf("Verify error = %v, want %v", err, c.want)
			}
		})
	}
}

func TestBuildRefusesNumbersThatWouldNotSurviveJSON(t *testing.T) {
	b, _ := NewBuilder(identity(t), "v2")
	w := sampleWindows(t)[0]
	w.Metrics.LateSpans = 1<<53 + 1
	if _, _, _, err := b.Build(context.Background(), State{}, w, batch.Commit{Root: strings.Repeat("0", 64)}, nil); err == nil {
		t.Error("a count beyond 2^53 was accepted")
	}
}
