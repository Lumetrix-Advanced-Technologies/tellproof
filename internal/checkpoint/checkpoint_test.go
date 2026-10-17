package checkpoint

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/dsse"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/pack"
)

func digests(n int) []string {
	out := make([]string, n)
	for i := range out {
		s := sha256.Sum256([]byte(fmt.Sprint("pack", i)))
		out[i] = "sha256:" + hex.EncodeToString(s[:])
	}
	return out
}

func sign(t *testing.T, key *ecdsa.PrivateKey, p Predicate) *dsse.Envelope {
	t.Helper()
	payload, err := Statement(p)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := dsse.NewECDSAP256Signer(key, "k")
	env, err := dsse.Sign(context.Background(), s, dsse.PayloadTypeInToto, payload)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

var issued = time.Date(2026, 10, 2, 0, 10, 0, 0, time.FixedZone("AEST", 10*3600))

func TestEveryPackProvesForEverySize(t *testing.T) {
	for n := 1; n <= 33; n++ {
		ds := digests(n)
		p, err := Build("2026-10-01", "Australia/Sydney", issued, ds, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range ds {
			if _, err := Prove(&p, d); err != nil {
				t.Fatalf("n=%d %s: %v", n, d, err)
			}
		}
	}
	p, _ := Build("2026-10-01", "Australia/Sydney", issued, digests(3), nil)
	if _, err := Prove(&p, digests(4)[3]); err == nil {
		t.Error("a pack not in the checkpoint proved")
	}
	empty, _ := Build("2026-10-02", "Australia/Sydney", issued, nil, nil)
	if empty.Root != strings.Repeat("0", 64) || empty.PackCount != 0 || empty.PackDigests == nil {
		t.Errorf("empty day: %+v", empty)
	}
}

func TestVerifyChecksSignatureRootChainAndKey(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	keys := pack.KeySetOf(map[string]crypto.PublicKey{"k": &key.PublicKey})
	d1, _ := Build("2026-10-01", "Australia/Sydney", issued, digests(5), nil)
	e1 := sign(t, key, d1)
	if _, err := Verify(e1, keys, nil); err != nil {
		t.Fatal(err)
	}
	prev := Digest(e1)
	d2, _ := Build("2026-10-02", "Australia/Sydney", issued.Add(24*time.Hour), digests(2), &prev)
	e2 := sign(t, key, d2)
	if _, err := Verify(e2, keys, e1); err != nil {
		t.Errorf("day 2 after day 1: %v", err)
	}
	if _, err := Verify(e2, keys, e2); err == nil {
		t.Error("a checkpoint that does not name prev was accepted")
	}

	// Tampering: a dropped pack, a reordered list, a wrong count, a duplicate, another statement type.
	for name, edit := range map[string]func(p *Predicate){
		"dropped pack":   func(p *Predicate) { p.PackDigests = p.PackDigests[1:]; p.PackCount-- },
		"reordered":      func(p *Predicate) { p.PackDigests[0], p.PackDigests[1] = p.PackDigests[1], p.PackDigests[0] },
		"wrong count":    func(p *Predicate) { p.PackCount++ },
		"duplicate":      func(p *Predicate) { p.PackDigests[1] = p.PackDigests[0] },
		"malformed":      func(p *Predicate) { p.PackDigests[0] = "sha256:XYZ" },
		"another schema": func(p *Predicate) { p.CheckpointSchema = "other" },
	} {
		p, _ := Build("2026-10-01", "Australia/Sydney", issued, digests(5), nil)
		root := p.Root
		edit(&p)
		p.Root = root // the issuer keeps the old root: the list no longer matches it
		if _, err := Verify(sign(t, key, p), keys, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, err := Verify(sign(t, other, d1), keys, nil); err == nil {
		t.Error("another key's signature was accepted")
	}
	pub, _ := key.PublicKey.ECDH()
	xy := pub.Bytes()[1:] // uncompressed point: 0x04 || x || y
	b64 := base64.RawURLEncoding.EncodeToString
	retired, _ := pack.ParseJWKS(fmt.Appendf(nil, `{"keys":[{"kty":"EC","crv":"P-256","kid":"k","x":"%s","y":"%s","exp":%d}]}`, b64(xy[:32]), b64(xy[32:]), issued.Unix()))
	if _, err := Verify(e1, retired, nil); err == nil || !strings.Contains(err.Error(), "exp") {
		t.Errorf("a checkpoint signed when its key had expired: %v", err)
	}
	// A pack statement is not a checkpoint.
	if _, err := Decode([]byte(`{"_type":"https://in-toto.io/Statement/v1","predicate":{},"predicateType":"` + pack.PredicateType + `","subject":[]}`)); err == nil {
		t.Error("a pack statement decoded as a checkpoint")
	}
}
