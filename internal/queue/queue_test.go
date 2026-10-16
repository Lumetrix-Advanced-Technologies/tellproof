package queue

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/aggregate"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/batch"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/batchsign"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/dsse"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/enrol"
)

const agentID = "agt_0123456789abcdef"

var seed = func() []byte { b, _ := hex.DecodeString(strings.Repeat("ab", 32)); return b }()

func identity() *enrol.Identity {
	return &enrol.Identity{AgentID: agentID, Key: ed25519.NewKeyFromSeed(seed)}
}

func window(seq int64) aggregate.Window {
	h := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Hour)
	return aggregate.Window{
		Window: batch.Window{Start: h.Format(time.RFC3339), End: h.Add(time.Hour).Format(time.RFC3339)},
		Metrics: batch.Metrics{Tokens: []batch.TokenCount{{Provider: "openai", Model: "m", Region: "r", InputTokens: seq, Calls: 1}},
			Evals: []batch.Eval{}, Transitions: []batch.Transition{}, Anomalies: []batch.Anomaly{}},
		Observed: batch.Observed{Models: []batch.ObservedModel{}, Versions: []batch.ObservedVersion{}},
	}
}

// enqueueThrough builds and queues batches until the queue's state reaches n.
func enqueueThrough(t testing.TB, q *Queue, n int64) {
	b, err := batchsign.NewBuilder(identity(), "v2")
	if err != nil {
		t.Fatal(err)
	}
	for st := q.State(); st.Seq < n; st = q.State() {
		env, _, next, err := b.Build(context.Background(), st, window(st.Seq+1), batch.Commit{Root: strings.Repeat("0", 64)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Enqueue(env, next); err != nil {
			t.Fatal(err)
		}
	}
}

// server stores batches by (agent, seq) the way P4-2 must: verify, refuse a
// gap or a wrong prev (409), and answer a seq it already holds with 200 and
// the stored digest.
type server struct {
	mu       sync.Mutex
	stored   map[int64]string
	posts    map[int64]int
	lastPrev string
	delay    time.Duration
	dropOnce map[int64]bool // store, then drop the connection without answering
	status   int            // force this status
}

func newServer() *server {
	return &server{stored: map[int64]string{}, posts: map[int64]int{}, dropOnce: map[int64]bool{}}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	time.Sleep(s.delay)
	body, _ := io.ReadAll(r.Body)
	env, err := dsse.Decode(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	bt, err := batchsign.Verify(env, identity().Key.Public().(ed25519.PublicKey), agentID)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posts[bt.Seq]++
	if s.status != 0 {
		w.WriteHeader(s.status)
		return
	}
	digest := batchsign.Digest(env)
	if have, ok := s.stored[bt.Seq]; ok {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"digest": have})
		return
	}
	prev := ""
	if bt.Prev != nil {
		prev = *bt.Prev
	}
	if int64(len(s.stored))+1 != bt.Seq || prev != s.lastPrev {
		w.WriteHeader(http.StatusConflict)
		return
	}
	s.stored[bt.Seq], s.lastPrev = digest, digest
	if s.dropOnce[bt.Seq] {
		delete(s.dropOnce, bt.Seq)
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"digest": digest})
}

func (s *server) storedOnce(t *testing.T, n int64) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if int64(len(s.stored)) != n {
		t.Errorf("server stored %d batches, want %d", len(s.stored), n)
	}
	for seq := int64(1); seq <= n; seq++ {
		if _, ok := s.stored[seq]; !ok {
			t.Errorf("batch %d was lost", seq)
		}
	}
}

func noSleep(context.Context, time.Duration) error { return nil }

// TestHelperAgent is the agent process the kill test starts and kills.
func TestHelperAgent(t *testing.T) {
	if os.Getenv("PLUMBLINE_QUEUE_HELPER") != "1" {
		t.Skip("helper process for TestKillMidSendLosesNothing")
	}
	n, _ := strconv.ParseInt(os.Getenv("HELPER_N"), 10, 64)
	q, err := Open(os.Getenv("HELPER_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	enqueueThrough(t, q, n)
	s := &Sender{Queue: q, Server: os.Getenv("HELPER_SERVER"), Sleep: func(ctx context.Context, d time.Duration) error { time.Sleep(10 * time.Millisecond); return nil }}
	if err := s.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Acceptance: killing the agent mid-send and restarting loses no batch and
// sends none twice (the server dedupes by agent and seq).
func TestKillMidSendLosesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("starts and kills processes")
	}
	srv := newServer()
	srv.delay = 30 * time.Millisecond
	hs := httptest.NewServer(srv)
	defer hs.Close()
	dir := filepath.Join(t.TempDir(), "queue")
	const n = 12
	run := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperAgent$", "-test.count=1")
		cmd.Env = append(os.Environ(), "PLUMBLINE_QUEUE_HELPER=1", "HELPER_DIR="+dir, "HELPER_SERVER="+hs.URL, fmt.Sprintf("HELPER_N=%d", n))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	// Kill it three times at different points, then let it finish.
	for _, after := range []int{2, 5, 9} {
		cmd := run()
		deadline := time.Now().Add(20 * time.Second)
		for {
			srv.mu.Lock()
			got := len(srv.stored)
			srv.mu.Unlock()
			if got >= after || time.Now().After(deadline) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_ = cmd.Wait()
	}
	if err := run().Wait(); err != nil {
		t.Fatalf("final run: %v", err)
	}
	srv.storedOnce(t, n)
	q, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := q.Pending(); len(p) != 0 {
		t.Errorf("queue still holds %v", p)
	}
	if q.State().Seq != n {
		t.Errorf("state seq = %d", q.State().Seq)
	}
}

func TestRecoversStateFromTheNewestBatch(t *testing.T) {
	dir := t.TempDir()
	q, _ := Open(dir)
	enqueueThrough(t, q, 3)
	// Simulate a crash after the batch file was written but before the state.
	b, _ := json.Marshal(batchsign.State{Seq: 2})
	if err := os.WriteFile(filepath.Join(dir, stateFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	q2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if q2.State().Seq != 3 || q2.State().Prev == nil || *q2.State().Prev != *q.State().Prev {
		t.Errorf("recovered %+v, want the state after batch 3", q2.State())
	}
	if p, _ := q2.Pending(); len(p) != 3 {
		t.Errorf("pending = %v", p)
	}
}

func TestLostResponseIsResentAndStoredOnce(t *testing.T) {
	srv := newServer()
	srv.dropOnce[2] = true
	hs := httptest.NewServer(srv)
	defer hs.Close()
	q, _ := Open(t.TempDir())
	enqueueThrough(t, q, 3)
	s := &Sender{Queue: q, Server: hs.URL, HTTP: hs.Client(), Sleep: noSleep}
	if err := s.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	srv.storedOnce(t, 3)
	if srv.posts[2] != 2 {
		t.Errorf("batch 2 posted %d times, want 2 (the second answered 200)", srv.posts[2])
	}
}

func TestRefusalsStopAndKeepTheBatch(t *testing.T) {
	for name, status := range map[string]int{"seq gap": http.StatusConflict, "bad signature": http.StatusUnauthorized, "invalid": http.StatusBadRequest} {
		t.Run(name, func(t *testing.T) {
			srv := newServer()
			srv.status = status
			hs := httptest.NewServer(srv)
			defer hs.Close()
			q, _ := Open(t.TempDir())
			enqueueThrough(t, q, 1)
			err := (&Sender{Queue: q, Server: hs.URL, HTTP: hs.Client(), Sleep: noSleep}).Drain(context.Background())
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("Drain = %v, want ErrRefused", err)
			}
			if p, _ := q.Pending(); len(p) != 1 {
				t.Errorf("the refused batch was deleted")
			}
		})
	}
	t.Run("different batch under the same seq", func(t *testing.T) {
		srv := newServer()
		srv.stored[1] = "sha256:" + strings.Repeat("0", 64)
		hs := httptest.NewServer(srv)
		defer hs.Close()
		q, _ := Open(t.TempDir())
		enqueueThrough(t, q, 1)
		err := (&Sender{Queue: q, Server: hs.URL, HTTP: hs.Client(), Sleep: noSleep}).Drain(context.Background())
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "different batch") {
			t.Fatalf("Drain = %v", err)
		}
	})
}

func TestBackoffDoublesToTheCap(t *testing.T) {
	srv := newServer()
	srv.status = http.StatusServiceUnavailable
	hs := httptest.NewServer(srv)
	defer hs.Close()
	q, _ := Open(t.TempDir())
	enqueueThrough(t, q, 1)
	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	s := &Sender{Queue: q, Server: hs.URL, HTTP: hs.Client(), MaxBackoff: 8 * time.Second, Sleep: func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		if len(waits) == 6 {
			cancel()
			return context.Canceled
		}
		return nil
	}}
	if err := s.Drain(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Drain = %v", err)
	}
	limits := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, w := range waits {
		if w < limits[i]/2 || w > limits[i] {
			t.Errorf("wait %d = %v, want between %v and %v", i, w, limits[i]/2, limits[i])
		}
	}
}

func TestEnqueueRefusesOutOfOrder(t *testing.T) {
	q, _ := Open(t.TempDir())
	if err := q.Enqueue(&dsse.Envelope{}, batchsign.State{Seq: 2}); err == nil {
		t.Error("a batch out of order was queued")
	}
	if info, _ := os.Stat(q.dir); info.Mode().Perm() != 0o700 {
		t.Errorf("queue dir mode %v", info.Mode().Perm())
	}
}
