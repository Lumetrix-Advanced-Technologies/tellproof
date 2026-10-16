package merkle

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// sampleSaltSecret is the vendor salt secret make_vectors.py used for the
// synthetic Kestrel Assist packs. It is sample data, not a real secret.
var sampleSaltSecret = []byte("vendor-held-salt-secret-sample")

func h(t testing.TB, s string) Hash {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("bad hash hex %q", s)
	}
	return Hash(b)
}

func leafN(i int) Hash { return sha256.Sum256([]byte(fmt.Sprintf("leaf-%d", i))) }

type commitments struct {
	Root      string `json:"trace_merkle_root"`
	LeafCount int    `json:"leaf_count"`
	Disclosed struct {
		TraceID   string `json:"trace_id"`
		LeafIndex int    `json:"leaf_index"`
		Salt      string `json:"salt"`
		Proof     []struct {
			Side string `json:"side"`
			Hash string `json:"hash"`
		} `json:"proof"`
	} `json:"disclosed"`
}

func loadCommitments(t testing.TB, file string) commitments {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "spec", "vectors", file))
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Predicate struct {
			Commitments commitments `json:"commitments"`
		} `json:"predicate"`
	}
	if err := json.Unmarshal(payload, &st); err != nil {
		t.Fatal(err)
	}
	return st.Predicate.Commitments
}

func (c commitments) leafAndProof(t testing.TB) (Hash, []Step) {
	t.Helper()
	saltBytes, err := hex.DecodeString(c.Disclosed.Salt)
	if err != nil || len(saltBytes) != 16 {
		t.Fatalf("bad salt %q", c.Disclosed.Salt)
	}
	proof := make([]Step, len(c.Disclosed.Proof))
	for i, s := range c.Disclosed.Proof {
		if len(s.Side) != 1 {
			t.Fatalf("bad side %q", s.Side)
		}
		proof[i] = Step{Side: Side(s.Side[0]), Hash: h(t, s.Hash)}
	}
	return LeafHash([16]byte(saltBytes), c.Disclosed.TraceID), proof
}

// Acceptance: the disclosed proof in every pass vector verifies and
// fail/bad-merkle-proof does not; every manifest case matches its "merkle" result.
func TestVectorDisclosedProofs(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "spec", "vectors", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Cases []struct {
			Name   string `json:"name"`
			File   string `json:"file"`
			Expect struct {
				Merkle bool `json:"merkle"`
			} `json:"expect"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var sawFail bool
	for _, c := range m.Cases {
		t.Run(c.Name, func(t *testing.T) {
			com := loadCommitments(t, c.File)
			leaf, proof := com.leafAndProof(t)
			if got := VerifyProof(leaf, proof, h(t, com.Root)); got != c.Expect.Merkle {
				t.Errorf("VerifyProof = %v, manifest expects %v", got, c.Expect.Merkle)
			}
		})
		sawFail = sawFail || !c.Expect.Merkle
	}
	if !sawFail {
		t.Error("manifest has no merkle failure case")
	}
}

// Rebuild each pass vector's whole tree the way make_vectors.py did, and check
// the root, the salt and the exact disclosed proof (Python and Go agree).
func TestVectorTreesRebuild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three trees of ~350k leaves")
	}
	for seq, file := range []string{"pass/KST-CTA-0001.dsse.json", "pass/KST-CTA-0002.dsse.json", "pass/KST-CTA-0003.dsse.json"} {
		t.Run(file, func(t *testing.T) {
			com := loadCommitments(t, file)
			leaves := make([]Hash, com.LeafCount)
			for i := range leaves {
				id := fmt.Sprintf("tr_%d_%07d", seq+1, i)
				leaves[i] = LeafHash(Salt(sampleSaltSecret, id), id)
			}
			tree, err := NewTree(leaves)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := tree.Root(), h(t, com.Root); got != want {
				t.Fatalf("root = %x, want %s", got, com.Root)
			}
			if got := Salt(sampleSaltSecret, com.Disclosed.TraceID); hex.EncodeToString(got[:]) != com.Disclosed.Salt {
				t.Errorf("Salt(%q) = %x, want %s", com.Disclosed.TraceID, got, com.Disclosed.Salt)
			}
			_, wantProof := com.leafAndProof(t)
			gotProof, err := tree.Proof(com.Disclosed.LeafIndex)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(gotProof, wantProof) {
				t.Errorf("Proof(%d) differs from the vector's disclosed proof:\n got %v\nwant %v", com.Disclosed.LeafIndex, gotProof, wantProof)
			}
		})
	}
}

// Small trees spelled out by hand, including odd nodes carried up unchanged.
func TestKnownTrees(t *testing.T) {
	a, b, c, d, e := leafN(0), leafN(1), leafN(2), leafN(3), leafN(4)
	ab, cd := NodeHash(a, b), NodeHash(c, d)
	tests := []struct {
		name   string
		leaves []Hash
		root   Hash
		proofs map[int][]Step
	}{
		{"one leaf is its own root", []Hash{a}, a, map[int][]Step{0: nil}},
		{"two leaves", []Hash{a, b}, ab, map[int][]Step{
			0: {{Right, b}},
			1: {{Left, a}},
		}},
		{"three leaves: c carried up", []Hash{a, b, c}, NodeHash(ab, c), map[int][]Step{
			0: {{Right, b}, {Right, c}},
			2: {{Left, ab}}, // no entry for the level where c had no sibling
		}},
		{"four leaves", []Hash{a, b, c, d}, NodeHash(ab, cd), map[int][]Step{
			2: {{Right, d}, {Left, ab}},
		}},
		{"five leaves: e carried up twice", []Hash{a, b, c, d, e}, NodeHash(NodeHash(ab, cd), e), map[int][]Step{
			4: {{Left, NodeHash(ab, cd)}},
			1: {{Left, a}, {Right, cd}, {Right, e}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tree, err := NewTree(tt.leaves)
			if err != nil {
				t.Fatal(err)
			}
			if tree.Root() != tt.root {
				t.Errorf("root = %x, want %x", tree.Root(), tt.root)
			}
			if tree.Len() != len(tt.leaves) {
				t.Errorf("Len = %d, want %d", tree.Len(), len(tt.leaves))
			}
			for i, want := range tt.proofs {
				got, err := tree.Proof(i)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got, want) {
					t.Errorf("Proof(%d) = %v, want %v", i, got, want)
				}
				if !VerifyProof(tt.leaves[i], got, tt.root) {
					t.Errorf("Proof(%d) does not verify", i)
				}
			}
		})
	}
}

func TestHashConstruction(t *testing.T) {
	salt := [16]byte{0: 0xaa, 15: 0xbb}
	if got, want := LeafHash(salt, "tr_é"), Hash(sha256.Sum256(append(append([]byte{0x00}, salt[:]...), "tr_é"...))); got != want {
		t.Errorf("LeafHash = %x, want sha256(0x00 || salt || utf8(id)) = %x", got, want)
	}
	l, r := leafN(1), leafN(2)
	if got, want := NodeHash(l, r), Hash(sha256.Sum256(append(append([]byte{0x01}, l[:]...), r[:]...))); got != want {
		t.Errorf("NodeHash = %x, want sha256(0x01 || l || r) = %x", got, want)
	}
	if NodeHash(l, r) == NodeHash(r, l) {
		t.Error("NodeHash is symmetric; order must matter")
	}
}

// Acceptance: every leaf of random trees of size 1 to 1,000 proves to the root,
// and a proof stops verifying when any part of it is changed.
func TestPropertyEveryLeafProves(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for n := 1; n <= 1000; n++ {
		leaves := make([]Hash, n)
		for i := range leaves {
			for j := range leaves[i] {
				leaves[i][j] = byte(rng.UintN(256))
			}
		}
		tree, err := NewTree(leaves)
		if err != nil {
			t.Fatal(err)
		}
		root := tree.Root()
		for i, leaf := range leaves {
			proof, err := tree.Proof(i)
			if err != nil {
				t.Fatalf("n=%d Proof(%d): %v", n, i, err)
			}
			if !VerifyProof(leaf, proof, root) {
				t.Fatalf("n=%d leaf %d does not prove to the root", n, i)
			}
			if n == 1 || i%37 != 0 {
				continue // tamper checks on a sample
			}
			other := leaves[(i+1)%n]
			if VerifyProof(other, proof, root) {
				t.Fatalf("n=%d leaf %d's proof verifies leaf %d", n, i, (i+1)%n)
			}
			step := rng.IntN(len(proof))
			flipped := slices.Clone(proof)
			flipped[step].Hash[rng.IntN(32)] ^= 1 << rng.UintN(8)
			if VerifyProof(leaf, flipped, root) {
				t.Fatalf("n=%d leaf %d: proof with a flipped bit verifies", n, i)
			}
			swapped := slices.Clone(proof)
			swapped[step].Side = Left + Right - swapped[step].Side
			if VerifyProof(leaf, swapped, root) {
				t.Fatalf("n=%d leaf %d: proof with a swapped side verifies", n, i)
			}
			if VerifyProof(leaf, proof[:len(proof)-1], root) {
				t.Fatalf("n=%d leaf %d: truncated proof verifies", n, i)
			}
		}
	}
}

func TestErrors(t *testing.T) {
	if _, err := NewTree(nil); err == nil {
		t.Error("NewTree(nil): want error")
	}
	tree, err := NewTree([]Hash{leafN(0), leafN(1), leafN(2)})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{-1, 3, 100} {
		if _, err := tree.Proof(i); err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Errorf("Proof(%d) error = %v, want out of range", i, err)
		}
	}
	a, b := leafN(0), leafN(1)
	if VerifyProof(a, []Step{{Side: 'X', Hash: b}}, NodeHash(a, b)) {
		t.Error("VerifyProof accepted an invalid side")
	}
	if VerifyProof(a, []Step{{Side: 'r', Hash: b}}, NodeHash(a, b)) {
		t.Error("VerifyProof accepted a lowercase side")
	}
}

// NewTree keeps its own copy of the leaves.
func TestTreeCopiesLeaves(t *testing.T) {
	leaves := []Hash{leafN(0), leafN(1)}
	tree, err := NewTree(leaves)
	if err != nil {
		t.Fatal(err)
	}
	root := tree.Root()
	leaves[0] = leafN(9)
	if tree.Root() != root {
		t.Error("changing the caller's slice changed the tree")
	}
	if p, _ := tree.Proof(1); !VerifyProof(leafN(1), p, root) {
		t.Error("proof no longer verifies after the caller's slice changed")
	}
}
