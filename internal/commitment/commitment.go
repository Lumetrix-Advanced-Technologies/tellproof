// Package commitment builds the salted trace commitments of
// docs/architecture.md §2.4 inside the agent: one Merkle root per hourly
// window (sent in the batch), and a period root over window roots (D-027)
// from which a single trace can later be disclosed. The salt secret never
// leaves the customer, so a root reveals nothing about trace IDs.
package commitment

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/batch"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/merkle"
)

// MinSecretBytes is the shortest salt secret accepted.
const MinSecretBytes = 16

// LoadSecret reads the salt secret from an environment value or from a file
// readable only by its owner (mode 0600 or stricter). Exactly one must be set.
// Errors never include the secret.
func LoadSecret(envValue, path string) ([]byte, error) {
	var s []byte
	switch {
	case envValue != "" && path != "":
		return nil, errors.New("commitment: set the salt secret in the environment or in a file, not both")
	case envValue != "":
		s = []byte(envValue)
	case path != "":
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("commitment: salt secret file: %w", err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("commitment: salt secret file %s must be readable only by its owner (chmod 600)", path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("commitment: salt secret file: %w", err)
		}
		s = bytes.TrimRight(b, "\r\n")
	default:
		return nil, errors.New("commitment: no salt secret: set PLUMBLINE_SALT_SECRET or a salt secret file")
	}
	if len(s) < MinSecretBytes {
		return nil, fmt.Errorf("commitment: salt secret must be at least %d bytes", MinSecretBytes)
	}
	return s, nil
}

// Window is one hour's commitment: the batch's commit field and the leaves
// the agent keeps locally for later disclosure.
type Window struct {
	Commit batch.Commit
	Leaves []merkle.Hash
	tree   *merkle.Tree
}

// ForWindow commits to the window's trace IDs, in order. An empty window has
// the all-zero root and no leaves (as the concierge writes it).
func ForWindow(secret []byte, traceIDs []string) *Window {
	leaves := make([]merkle.Hash, len(traceIDs))
	for i, id := range traceIDs {
		leaves[i] = merkle.LeafHash(merkle.Salt(secret, id), id)
	}
	return FromLeaves(leaves)
}

// FromLeaves rebuilds a window commitment from stored leaves.
func FromLeaves(leaves []merkle.Hash) *Window {
	w := &Window{Commit: batch.Commit{Root: strings.Repeat("0", 64), Leaves: int64(len(leaves))}, Leaves: leaves}
	if len(leaves) > 0 {
		t, _ := merkle.NewTree(leaves) // cannot fail: there are leaves
		w.tree = t
		root := t.Root()
		w.Commit.Root = hex.EncodeToString(root[:])
	}
	return w
}

// Disclosure reveals one trace: its salt and the proof from its leaf to a root.
type Disclosure struct {
	TraceID   string
	LeafIndex int // within the window
	Salt      [16]byte
	Proof     []merkle.Step
}

// Disclose proves traceID is in this window. It fails if the trace is not
// here or the secret is not the one the window was committed with.
func (w *Window) Disclose(secret []byte, traceID string) (Disclosure, error) {
	salt := merkle.Salt(secret, traceID)
	leaf := merkle.LeafHash(salt, traceID)
	for i, l := range w.Leaves {
		if l == leaf {
			proof, err := w.tree.Proof(i)
			if err != nil {
				return Disclosure{}, err
			}
			return Disclosure{TraceID: traceID, LeafIndex: i, Salt: salt, Proof: proof}, nil
		}
	}
	return Disclosure{}, errors.New("commitment: trace is not in this window (or the salt secret differs)")
}

// Period is a commitment over a period's window roots: the Merkle tree whose
// leaves are the (non-empty) window roots, combined with the ordinary node
// hash. Its proofs extend a window proof up to the period root, so every
// verifier checks them unchanged (D-027).
type Period struct {
	Root    string
	Leaves  int64 // traces committed over the period
	windows []*Window
	index   []int // position of each window in the upper tree, -1 if empty
	tree    *merkle.Tree
}

// ForPeriod builds the period root from window commitments in time order.
func ForPeriod(windows []*Window) (*Period, error) {
	p := &Period{windows: windows, index: make([]int, len(windows))}
	var roots []merkle.Hash
	for i, w := range windows {
		p.index[i] = -1
		if w.Commit.Leaves == 0 {
			continue
		}
		p.index[i] = len(roots)
		roots = append(roots, w.tree.Root())
		p.Leaves += w.Commit.Leaves
	}
	if len(roots) == 0 {
		return nil, errors.New("commitment: the period has no committed traces")
	}
	t, err := merkle.NewTree(roots)
	if err != nil {
		return nil, err
	}
	p.tree = t
	root := t.Root()
	p.Root = hex.EncodeToString(root[:])
	return p, nil
}

// Disclose proves a trace from window w (its position in the period) all the
// way to the period root.
func (p *Period) Disclose(secret []byte, w int, traceID string) (Disclosure, error) {
	if w < 0 || w >= len(p.windows) || p.index[w] < 0 {
		return Disclosure{}, errors.New("commitment: no such committed window in this period")
	}
	d, err := p.windows[w].Disclose(secret, traceID)
	if err != nil {
		return Disclosure{}, err
	}
	upper, err := p.tree.Proof(p.index[w])
	if err != nil {
		return Disclosure{}, err
	}
	d.Proof = append(d.Proof, upper...)
	return d, nil
}
