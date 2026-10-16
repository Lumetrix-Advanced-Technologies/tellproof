package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/batchsign"
)

// ErrRefused: the server refused a batch in a way retrying cannot fix (bad
// signature, a seq gap or wrong prev, a different batch already stored under
// this seq). The batch stays queued and the agent reports it.
var ErrRefused = errors.New("queue: the server refused a batch")

// Sender delivers queued batches to POST {Server}/v1/batches, in seq order,
// to one host only.
type Sender struct {
	Queue      *Queue
	Server     string
	HTTP       *http.Client
	MaxBackoff time.Duration                                    // default 15 minutes (§3)
	Sleep      func(ctx context.Context, d time.Duration) error // nil: a context-aware timer
}

// Drain sends every queued batch, retrying network errors and 5xx with
// exponential backoff, until the queue is empty, ctx ends, or the server
// refuses a batch (ErrRefused).
func (s *Sender) Drain(ctx context.Context) error {
	backoff := time.Second
	for {
		seqs, err := s.Queue.Pending()
		if err != nil {
			return err
		}
		if len(seqs) == 0 {
			return nil
		}
		err = s.send(ctx, seqs[0])
		if err == nil {
			backoff = time.Second
			continue
		}
		if errors.Is(err, ErrRefused) || ctx.Err() != nil {
			return err
		}
		if err := s.sleep(ctx, jitter(backoff)); err != nil {
			return err
		}
		backoff = min(backoff*2, s.maxBackoff())
	}
}

func (s *Sender) maxBackoff() time.Duration {
	if s.MaxBackoff > 0 {
		return s.MaxBackoff
	}
	return 15 * time.Minute
}

// jitter spreads retries from many agents: between half and the full wait.
func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func (s *Sender) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// send posts one batch. Server answers (P4-2): 201 stored {digest}; 200
// already stored {digest} (idempotent by agent_id and seq); 401 bad
// signature; 409 seq gap or wrong prev; 400/413 invalid.
func (s *Sender) send(ctx context.Context, seq int64) error {
	env, err := s.Queue.Envelope(seq)
	if err != nil {
		return err
	}
	body, err := env.Encode()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Server+"/v1/batches", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("queue: sending batch %d: %w", seq, err)
	}
	defer resp.Body.Close()
	var reply struct {
		Digest string `json:"digest"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&reply)
	want := batchsign.Digest(env)
	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
		if reply.Digest != want {
			return fmt.Errorf("%w: batch %d: the server holds a different batch under this seq (digest %s, ours %s)", ErrRefused, seq, reply.Digest, want)
		}
		return s.Queue.Remove(seq)
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("queue: batch %d: HTTP %d", seq, resp.StatusCode)
	default:
		return fmt.Errorf("%w: batch %d: HTTP %d", ErrRefused, seq, resp.StatusCode)
	}
}
