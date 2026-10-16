// Package queue is the agent's durable outbox (docs/architecture.md §3): each
// signed batch is written to its own file in the data directory and fsynced
// before the agent moves on, then sent in seq order and deleted only once the
// server has stored it (201, or 200 with the same digest for a batch it
// already had). A crash at any point loses no batch, and a batch re-sent after
// a crash is recognised by the server by (agent_id, seq), so nothing is stored
// twice. The chain state (seq, prev) is recovered from the newest file.
package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/batchsign"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/dsse"
)

const (
	stateFile = "state.json"
	suffix    = ".batch.json"
)

// entry is one queued batch: the envelope and the chain state after it.
type entry struct {
	State    batchsign.State `json:"state"`
	Envelope json.RawMessage `json:"envelope"`
}

// Queue is safe for concurrent use.
type Queue struct {
	dir   string
	mu    sync.Mutex
	state batchsign.State
}

// Open opens (creating, mode 0700) the queue directory and recovers the chain
// state: the newer of state.json and the newest queued batch.
func Open(dir string) (*Queue, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("queue: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { // MkdirAll leaves an existing directory's mode alone
		return nil, fmt.Errorf("queue: %w", err)
	}
	q := &Queue{dir: dir}
	if b, err := os.ReadFile(filepath.Join(dir, stateFile)); err == nil {
		if err := json.Unmarshal(b, &q.state); err != nil {
			return nil, fmt.Errorf("queue: %s is damaged: %w", stateFile, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("queue: %w", err)
	}
	seqs, err := q.list()
	if err != nil {
		return nil, err
	}
	if len(seqs) > 0 {
		e, err := q.read(seqs[len(seqs)-1])
		if err != nil {
			return nil, err
		}
		if e.State.Seq > q.state.Seq {
			q.state = e.State // the batch was queued but the state not yet saved
		}
	}
	return q, nil
}

// State is the chain position after the last batch queued.
func (q *Queue) State() batchsign.State {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.state
}

// Enqueue durably stores a batch built from State(). next must be the state
// batchsign.Builder.Build returned for it.
func (q *Queue) Enqueue(env *dsse.Envelope, next batchsign.State) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if next.Seq != q.state.Seq+1 {
		return fmt.Errorf("queue: batch seq %d does not follow %d", next.Seq, q.state.Seq)
	}
	raw, err := env.Encode()
	if err != nil {
		return err
	}
	b, err := json.Marshal(entry{State: next, Envelope: raw})
	if err != nil {
		return err
	}
	// The batch first, then the state: a crash between the two is repaired by Open.
	if err := writeDurably(q.dir, name(next.Seq), b); err != nil {
		return err
	}
	sb, _ := json.Marshal(next)
	if err := writeDurably(q.dir, stateFile, sb); err != nil {
		return err
	}
	q.state = next
	return nil
}

// Pending lists the queued seqs, oldest first.
func (q *Queue) Pending() ([]int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.list()
}

// Envelope reads a queued batch.
func (q *Queue) Envelope(seq int64) (*dsse.Envelope, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, err := q.read(seq)
	if err != nil {
		return nil, err
	}
	return dsse.Decode(e.Envelope)
}

// Remove deletes a batch the server has stored.
func (q *Queue) Remove(seq int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := os.Remove(filepath.Join(q.dir, name(seq))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(q.dir)
}

func name(seq int64) string { return fmt.Sprintf("%020d%s", seq, suffix) }

func (q *Queue) list() ([]int64, error) {
	ents, err := os.ReadDir(q.dir)
	if err != nil {
		return nil, fmt.Errorf("queue: %w", err)
	}
	var seqs []int64
	for _, e := range ents {
		if n, ok := strings.CutSuffix(e.Name(), suffix); ok {
			if seq, err := strconv.ParseInt(n, 10, 64); err == nil {
				seqs = append(seqs, seq)
			}
		}
	}
	slices.Sort(seqs)
	return seqs, nil
}

func (q *Queue) read(seq int64) (entry, error) {
	var e entry
	b, err := os.ReadFile(filepath.Join(q.dir, name(seq)))
	if err != nil {
		return e, fmt.Errorf("queue: %w", err)
	}
	if err := json.Unmarshal(b, &e); err != nil || e.State.Seq != seq {
		return e, fmt.Errorf("queue: queued batch %d is damaged", seq)
	}
	return e, nil
}

// writeDurably writes data to dir/name via a temp file: write, fsync, rename,
// fsync the directory. A reader sees the old file or the whole new one.
func writeDurably(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("queue: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("queue: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("queue: fsync dir: %w", err)
	}
	return nil
}
