package commitment

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/aggregate"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/merkle"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/otlp"
)

var secret = []byte("parity-secret-0123456789")

func windows(t *testing.T) []aggregate.Window {
	t.Helper()
	m, err := otlp.DefaultMapping()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "concierge", "testdata", "otlp-sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	a := aggregate.New(aggregate.DefaultConfig())
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		res, err := otlp.Parse(line, m)
		if err != nil {
			t.Fatal(err)
		}
		a.Add(res.Spans, res.Spans[0].Start)
	}
	return a.FlushAll()
}

// Acceptance: a disclosed trace from a window verifies against that window's root.
func TestDisclosedTraceVerifiesAgainstItsWindow(t *testing.T) {
	for _, w := range windows(t) {
		c := ForWindow(secret, w.TraceIDs)
		root, err := hex.DecodeString(c.Commit.Root)
		if err != nil || len(root) != 32 {
			t.Fatalf("root %q", c.Commit.Root)
		}
		if c.Commit.Leaves != int64(len(w.TraceIDs)) {
			t.Fatalf("leaves = %d, want %d", c.Commit.Leaves, len(w.TraceIDs))
		}
		for _, id := range []string{w.TraceIDs[0], w.TraceIDs[len(w.TraceIDs)/2], w.TraceIDs[len(w.TraceIDs)-1]} {
			d, err := c.Disclose(secret, id)
			if err != nil {
				t.Fatalf("Disclose(%s): %v", id, err)
			}
			if d.TraceID != id || !merkle.VerifyProof(merkle.LeafHash(d.Salt, id), d.Proof, merkle.Hash(root)) {
				t.Errorf("trace %s does not verify against window %s", id, w.Window.Start)
			}
		}
		if _, err := c.Disclose(secret, "not-a-trace-in-this-window"); err == nil {
			t.Error("Disclose of an unknown trace succeeded")
		}
		if _, err := c.Disclose([]byte("another-secret-0123456"), w.TraceIDs[0]); err == nil {
			t.Error("Disclose with the wrong secret succeeded")
		}
	}
}

// The agent's window roots equal the concierge's for the same secret and input.
func TestRootsMatchConcierge(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	script := "import sys, json; sys.path.insert(0, 'concierge'); import ingest; " +
		"out, _ = ingest.ingest('otlp', ['concierge/testdata/otlp-sample.jsonl'], sys.argv[1].encode()); print(json.dumps([w['commit'] for w in out['windows']]))"
	cmd := exec.Command(py, "-c", script, string(secret))
	cmd.Dir = filepath.Join("..", "..")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("concierge ingest: %v", err)
	}
	var want []struct {
		Root   string `json:"root"`
		Leaves int64  `json:"leaves"`
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	ws := windows(t)
	if len(ws) != len(want) {
		t.Fatalf("%d windows vs %d", len(ws), len(want))
	}
	for i, w := range ws {
		c := ForWindow(secret, w.TraceIDs).Commit
		if c.Root != want[i].Root || c.Leaves != want[i].Leaves {
			t.Errorf("window %d: %s/%d, concierge %s/%d", i, c.Root, c.Leaves, want[i].Root, want[i].Leaves)
		}
	}
}

// A period commitment over window roots: the disclosure proof runs leaf ->
// window root -> period root, and any verifier checks it with the ordinary
// side/hash steps (D-027).
func TestPeriodProofChainsThroughTheWindowRoot(t *testing.T) {
	ws := windows(t)
	var commits []*Window
	for _, w := range ws {
		commits = append(commits, ForWindow(secret, w.TraceIDs))
	}
	p, err := ForPeriod(commits)
	if err != nil {
		t.Fatal(err)
	}
	if p.Leaves != commits[0].Commit.Leaves+commits[1].Commit.Leaves {
		t.Errorf("period leaves = %d", p.Leaves)
	}
	id := ws[1].TraceIDs[7]
	d, err := p.Disclose(secret, 1, id)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := hex.DecodeString(p.Root)
	if !merkle.VerifyProof(merkle.LeafHash(d.Salt, id), d.Proof, merkle.Hash(root)) {
		t.Error("period proof does not verify")
	}
	if _, err := p.Disclose(secret, 0, id); err == nil {
		t.Error("disclosing a trace under the wrong window succeeded")
	}
	if _, err := ForPeriod(nil); err == nil {
		t.Error("ForPeriod of no windows succeeded")
	}
}

func TestEmptyWindowHasZeroRoot(t *testing.T) {
	c := ForWindow(secret, nil)
	if c.Commit.Root != strings.Repeat("0", 64) || c.Commit.Leaves != 0 {
		t.Errorf("empty commit = %+v", c.Commit)
	}
}

func TestLoadSecret(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "salt")
	if err := os.WriteFile(good, []byte("a-file-secret-of-32-bytes-long!!\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(dir, "loose")
	if err := os.WriteFile(loose, []byte("a-file-secret-of-32-bytes-long!!"), 0o644); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("tooshort"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := LoadSecret("", good); err != nil || string(s) != "a-file-secret-of-32-bytes-long!!" {
		t.Errorf("file secret = %q, %v (trailing newline must be trimmed)", s, err)
	}
	if s, err := LoadSecret("an-env-secret-0123456789", ""); err != nil || string(s) != "an-env-secret-0123456789" {
		t.Errorf("env secret = %q, %v", s, err)
	}
	for name, c := range map[string][2]string{
		"none":           {"", ""},
		"world-readable": {"", loose},
		"too short file": {"", short},
		"too short env":  {"short", ""},
		"missing file":   {"", filepath.Join(dir, "absent")},
		"both given":     {"an-env-secret-0123456789", good},
	} {
		if _, err := LoadSecret(c[0], c[1]); err == nil {
			t.Errorf("%s: LoadSecret accepted it", name)
		} else if strings.Contains(err.Error(), "an-env-secret") || strings.Contains(err.Error(), "a-file-secret") {
			t.Errorf("%s: error %q includes the secret", name, err)
		}
	}
}
