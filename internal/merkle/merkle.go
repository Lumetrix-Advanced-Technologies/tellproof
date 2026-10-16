// Package merkle builds and checks the trace commitment of docs/architecture.md
// §2.4: salted leaves, domain-separated nodes, and an odd node at the end of a
// level carried up unchanged (not duplicated).
package merkle

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
)

// Hash is a SHA-256 digest: a leaf, a node or a root.
type Hash [32]byte

// Side says where a proof step's sibling sits.
type Side byte

const (
	Left  Side = 'L' // sibling is on the left: parent = NodeHash(sibling, current)
	Right Side = 'R' // sibling is on the right: parent = NodeHash(current, sibling)
)

// Step is one entry of an inclusion proof, from the leaf towards the root.
type Step struct {
	Side Side
	Hash Hash
}

// Salt derives a trace's salt: the first 16 bytes of HMAC-SHA256(secret, traceID).
// The secret never leaves the customer.
func Salt(secret []byte, traceID string) [16]byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(traceID))
	return [16]byte(m.Sum(nil))
}

// LeafHash is sha256(0x00 || salt || utf8(traceID)).
func LeafHash(salt [16]byte, traceID string) Hash {
	d := sha256.New()
	d.Write([]byte{0x00})
	d.Write(salt[:])
	d.Write([]byte(traceID))
	return Hash(d.Sum(nil))
}

// NodeHash is sha256(0x01 || left || right).
func NodeHash(left, right Hash) Hash {
	var b [1 + 2*len(Hash{})]byte
	b[0] = 0x01
	copy(b[1:], left[:])
	copy(b[1+len(left):], right[:])
	return sha256.Sum256(b[:])
}

// Tree holds every level of a Merkle tree, leaves first, so that proofs need no
// rehashing.
type Tree struct {
	levels [][]Hash
}

// NewTree builds the tree over leaves (already leaf-hashed), in order.
func NewTree(leaves []Hash) (*Tree, error) {
	if len(leaves) == 0 {
		return nil, errors.New("merkle: no leaves")
	}
	level := slices.Clone(leaves)
	levels := [][]Hash{level}
	for len(level) > 1 {
		next := make([]Hash, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 < len(level) {
				next = append(next, NodeHash(level[i], level[i+1]))
			} else {
				next = append(next, level[i]) // odd node carried up unchanged
			}
		}
		levels = append(levels, next)
		level = next
	}
	return &Tree{levels: levels}, nil
}

// Root returns the tree's root.
func (t *Tree) Root() Hash {
	return t.levels[len(t.levels)-1][0]
}

// Len returns the number of leaves.
func (t *Tree) Len() int {
	return len(t.levels[0])
}

// Proof returns the inclusion proof for the leaf at index. Levels where the
// node was carried up contribute no step.
func (t *Tree) Proof(index int) ([]Step, error) {
	if index < 0 || index >= t.Len() {
		return nil, fmt.Errorf("merkle: leaf index %d out of range [0, %d)", index, t.Len())
	}
	var proof []Step
	for _, level := range t.levels[:len(t.levels)-1] {
		sib := index ^ 1
		if sib < len(level) {
			side := Right
			if sib < index {
				side = Left
			}
			proof = append(proof, Step{Side: side, Hash: level[sib]})
		}
		index /= 2
	}
	return proof, nil
}

// VerifyProof reports whether leaf, combined with proof, hashes to root.
func VerifyProof(leaf Hash, proof []Step, root Hash) bool {
	h := leaf
	for _, s := range proof {
		switch s.Side {
		case Left:
			h = NodeHash(s.Hash, h)
		case Right:
			h = NodeHash(h, s.Hash)
		default:
			return false
		}
	}
	return h == root
}
