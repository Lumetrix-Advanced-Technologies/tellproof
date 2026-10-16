package dsse

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testKID is the kid of spec/vectors/test-jwks.json, the key behind test-public-key.pem.
const testKID = "test-p256-2026-10"

func vectorPath(name string) string {
	return filepath.Join("..", "..", "spec", "vectors", name)
}

func testKeys(t testing.TB) map[string]crypto.PublicKey {
	t.Helper()
	b, err := os.ReadFile(vectorPath("test-public-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKeyPEM(b)
	if err != nil {
		t.Fatalf("ParsePublicKeyPEM: %v", err)
	}
	return map[string]crypto.PublicKey{testKID: pub}
}

func loadEnvelope(t testing.TB, name string) *Envelope {
	t.Helper()
	b, err := os.ReadFile(vectorPath(name))
	if err != nil {
		t.Fatal(err)
	}
	env, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode %s: %v", name, err)
	}
	return env
}

func TestPAE(t *testing.T) {
	tests := []struct {
		name, payloadType, body, want string
	}{
		// Known answer from the DSSE specification (protocol.md).
		{"spec example", "http://example.com/HelloWorld", "hello world", "DSSEv1 29 http://example.com/HelloWorld 11 hello world"},
		{"empty type and body", "", "", "DSSEv1 0  0 "},
		{"lengths are bytes not runes", "t", "é", "DSSEv1 1 t 2 é"},
		{"pack type", PayloadTypeInToto, "{}", "DSSEv1 28 application/vnd.in-toto+json 2 {}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(PAE(tt.payloadType, []byte(tt.body))); got != tt.want {
				t.Errorf("PAE = %q, want %q", got, tt.want)
			}
		})
	}
}

// Acceptance: the pass vectors verify with spec/vectors/test-public-key.pem.
func TestVectorsPassVerify(t *testing.T) {
	keys := testKeys(t)
	files, err := filepath.Glob(vectorPath(filepath.Join("pass", "*.dsse.json")))
	if err != nil || len(files) != 3 {
		t.Fatalf("want 3 pass vectors, got %d (err %v)", len(files), err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			env := loadEnvelope(t, filepath.Join("pass", filepath.Base(f)))
			payload, kid, err := Verify(env, PayloadTypeInToto, keys)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if kid != testKID {
				t.Errorf("verified with kid %q, want %q", kid, testKID)
			}
			if !bytes.Equal(payload, env.Payload) {
				t.Error("Verify returned a payload that differs from the envelope's")
			}
		})
	}
}

// Acceptance: fail/tampered-payload and fail/wrong-key fail. Both carry the
// trusted keyid, and with one key and one ECDSA signature the two cases are
// indistinguishable (the signature does not verify), so both are
// ErrBadSignature. The pack layer tells them apart: only tampered-payload also
// fails the subject digest (manifest.json). See D-014.
func TestVectorsBadSignature(t *testing.T) {
	keys := testKeys(t)
	for _, name := range []string{"fail/tampered-payload.dsse.json", "fail/wrong-key.dsse.json"} {
		t.Run(name, func(t *testing.T) {
			payload, kid, err := Verify(loadEnvelope(t, name), PayloadTypeInToto, keys)
			if !errors.Is(err, ErrBadSignature) {
				t.Fatalf("Verify = %d bytes, kid %q, err %v; want ErrBadSignature", len(payload), kid, err)
			}
			if payload != nil {
				t.Error("Verify returned a payload alongside an error")
			}
			if !strings.Contains(err.Error(), testKID) {
				t.Errorf("error %q does not name the key %q", err, testKID)
			}
		})
	}
}

// Every manifest case's expected "signature" result matches Verify.
func TestVectorManifestSignature(t *testing.T) {
	b, err := os.ReadFile(vectorPath("manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Cases []struct {
			Name   string `json:"name"`
			File   string `json:"file"`
			Expect struct {
				Signature bool `json:"signature"`
			} `json:"expect"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Cases) == 0 {
		t.Fatal("manifest has no cases")
	}
	keys := testKeys(t)
	for _, c := range m.Cases {
		t.Run(c.Name, func(t *testing.T) {
			_, _, err := Verify(loadEnvelope(t, c.File), PayloadTypeInToto, keys)
			if got := err == nil; got != c.Expect.Signature {
				t.Errorf("signature verified = %v (err %v), manifest expects %v", got, err, c.Expect.Signature)
			}
		})
	}
}

func newSigners(t *testing.T) (ecSigner *ECDSAP256Signer, ecPub *ecdsa.PublicKey, edSigner *Ed25519Signer, edPub ed25519.PublicKey) {
	t.Helper()
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecSigner, err = NewECDSAP256Signer(ecKey, "pack-key-1")
	if err != nil {
		t.Fatal(err)
	}
	edPub, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edSigner, err = NewEd25519Signer(edKey, "agt_1")
	if err != nil {
		t.Fatal(err)
	}
	return ecSigner, &ecKey.PublicKey, edSigner, edPub
}

func TestSignVerifyRoundTrip(t *testing.T) {
	ecSigner, ecPub, edSigner, edPub := newSigners(t)
	keys := map[string]crypto.PublicKey{"pack-key-1": ecPub, "agt_1": edPub}
	tests := []struct {
		name        string
		signer      Signer
		payloadType string
		wantSigLen  func(int) bool
	}{
		{"ECDSA P-256 pack, DER signature", ecSigner, PayloadTypeInToto, func(n int) bool { return n >= 8 && n <= 72 }},
		{"Ed25519 batch, 64-byte signature", edSigner, PayloadTypeBatch, func(n int) bool { return n == ed25519.SignatureSize }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte(`{"hello":"world"}`)
			env, err := Sign(context.Background(), tt.signer, tt.payloadType, payload)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if n := len(env.Signatures[0].Sig); !tt.wantSigLen(n) {
				t.Errorf("signature length %d is wrong for this key type", n)
			}
			// Through the wire format and back.
			wire, err := env.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			got, err := Decode(wire)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			verified, _, err := Verify(got, tt.payloadType, keys)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if !bytes.Equal(verified, payload) {
				t.Errorf("verified payload = %q, want %q", verified, payload)
			}
		})
	}
}

func TestSignRespectsContext(t *testing.T) {
	ecSigner, _, edSigner, _ := newSigners(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, s := range []Signer{ecSigner, edSigner} {
		if _, err := Sign(ctx, s, PayloadTypeInToto, []byte("x")); !errors.Is(err, context.Canceled) {
			t.Errorf("%T: Sign with cancelled context = %v, want context.Canceled", s, err)
		}
	}
}

// Failures that can be told apart get distinct errors.
func TestVerifyErrors(t *testing.T) {
	ecSigner, ecPub, edSigner, edPub := newSigners(t)
	ctx := context.Background()
	payload := []byte(`{"n":1}`)
	pack, err := Sign(ctx, ecSigner, PayloadTypeInToto, payload)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := Sign(ctx, edSigner, PayloadTypeBatch, payload)
	if err != nil {
		t.Fatal(err)
	}
	clone := func(e *Envelope) *Envelope {
		c := *e
		c.Payload = bytes.Clone(e.Payload)
		c.Signatures = append([]Signature(nil), e.Signatures...)
		for i := range c.Signatures {
			c.Signatures[i].Sig = bytes.Clone(e.Signatures[i].Sig)
		}
		return &c
	}
	otherEC, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	good := map[string]crypto.PublicKey{"pack-key-1": ecPub, "agt_1": edPub}

	tests := []struct {
		name        string
		env         func() *Envelope
		payloadType string
		keys        map[string]crypto.PublicKey
		want        error
	}{
		{"payload changed", func() *Envelope { e := clone(pack); e.Payload[0] = '['; return e }, PayloadTypeInToto, good, ErrBadSignature},
		{"payload type changed and expected", func() *Envelope { e := clone(pack); e.PayloadType = PayloadTypeBatch; return e }, PayloadTypeBatch, good, ErrBadSignature},
		{"signature truncated", func() *Envelope { e := clone(pack); e.Signatures[0].Sig = e.Signatures[0].Sig[:10]; return e }, PayloadTypeInToto, good, ErrBadSignature},
		{"Ed25519 signature bit flipped", func() *Envelope { e := clone(batch); e.Signatures[0].Sig[0] ^= 1; return e }, PayloadTypeBatch, good, ErrBadSignature},
		{"signed by another key under the same keyid", func() *Envelope { return pack }, PayloadTypeInToto, map[string]crypto.PublicKey{"pack-key-1": &otherEC.PublicKey}, ErrBadSignature},
		{"keyid not trusted", func() *Envelope { return pack }, PayloadTypeInToto, map[string]crypto.PublicKey{"pack-key-2": ecPub}, ErrUnknownKey},
		{"empty keyid", func() *Envelope { e := clone(pack); e.Signatures[0].KeyID = ""; return e }, PayloadTypeInToto, good, ErrUnknownKey},
		{"no trusted keys", func() *Envelope { return pack }, PayloadTypeInToto, nil, ErrUnknownKey},
		{"wrong payload type for this caller", func() *Envelope { return batch }, PayloadTypeInToto, good, ErrPayloadType},
		{"no signatures", func() *Envelope { e := clone(pack); e.Signatures = nil; return e }, PayloadTypeInToto, good, ErrNoSignatures},
		{"nil envelope", func() *Envelope { return nil }, PayloadTypeInToto, good, ErrMalformed},
		{"ECDSA key on P-384", func() *Envelope { return pack }, PayloadTypeInToto, map[string]crypto.PublicKey{"pack-key-1": &p384.PublicKey}, ErrUnsupportedKey},
		{"RSA key", func() *Envelope { return pack }, PayloadTypeInToto, map[string]crypto.PublicKey{"pack-key-1": &rsaKey.PublicKey}, ErrUnsupportedKey},
		{"ECDSA signature checked with an Ed25519 key", func() *Envelope { return pack }, PayloadTypeInToto, map[string]crypto.PublicKey{"pack-key-1": edPub}, ErrBadSignature},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, _, err := Verify(tt.env(), tt.payloadType, tt.keys)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Verify error = %v, want %v", err, tt.want)
			}
			if payload != nil {
				t.Error("Verify returned a payload alongside an error")
			}
		})
	}

	// Distinct sentinels really are distinct.
	all := []error{ErrMalformed, ErrNoSignatures, ErrPayloadType, ErrUnknownKey, ErrUnsupportedKey, ErrBadSignature}
	for i, a := range all {
		for _, b := range all[i+1:] {
			if errors.Is(a, b) || errors.Is(b, a) {
				t.Errorf("%v and %v are not distinct", a, b)
			}
		}
	}
}

// With several signatures, one good signature from a trusted key is enough.
func TestVerifyAnyTrustedSignature(t *testing.T) {
	ecSigner, ecPub, _, _ := newSigners(t)
	env, err := Sign(context.Background(), ecSigner, PayloadTypeInToto, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	env.Signatures = append([]Signature{{KeyID: "someone-else", Sig: []byte{1, 2, 3}}}, env.Signatures...)
	if _, kid, err := Verify(env, PayloadTypeInToto, map[string]crypto.PublicKey{"pack-key-1": ecPub}); err != nil || kid != "pack-key-1" {
		t.Fatalf("Verify = kid %q, err %v; want pack-key-1, nil", kid, err)
	}
}

func TestDecodeRejects(t *testing.T) {
	ok := `{"payloadType":"application/vnd.in-toto+json","payload":"e30=","signatures":[{"keyid":"k","sig":"AAAA"}]}`
	if _, err := Decode([]byte(ok)); err != nil {
		t.Fatalf("Decode of a valid envelope: %v", err)
	}
	tests := []struct {
		name, input string
	}{
		{"unknown field", strings.Replace(ok, `"payload":`, `"extra":1,"payload":`, 1)},
		{"unknown signature field", strings.Replace(ok, `"keyid":"k"`, `"keyid":"k","cert":"x"`, 1)},
		{"missing payloadType", strings.Replace(ok, `"payloadType":"application/vnd.in-toto+json",`, ``, 1)},
		{"missing payload", strings.Replace(ok, `"payload":"e30=",`, ``, 1)},
		{"empty signatures", strings.Replace(ok, `[{"keyid":"k","sig":"AAAA"}]`, `[]`, 1)},
		{"empty sig", strings.Replace(ok, `"sig":"AAAA"`, `"sig":""`, 1)},
		{"payload not base64", strings.Replace(ok, `"e30="`, `"e30=!"`, 1)},
		{"payload URL-safe base64", strings.Replace(ok, `"e30="`, `"-_8="`, 1)},
		{"payload unpadded base64", strings.Replace(ok, `"e30="`, `"e30"`, 1)},
		{"trailing data", ok + `{}`},
		{"not an object", `[]`},
		{"empty input", ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if env, err := Decode([]byte(tt.input)); !errors.Is(err, ErrMalformed) {
				t.Errorf("Decode = %+v, %v; want ErrMalformed", env, err)
			}
		})
	}
}

func TestSignerConstructors(t *testing.T) {
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewECDSAP256Signer(p384, "k"); !errors.Is(err, ErrUnsupportedKey) {
		t.Errorf("P-384 signer: err %v, want ErrUnsupportedKey", err)
	}
	if _, err := NewECDSAP256Signer(nil, "k"); !errors.Is(err, ErrUnsupportedKey) {
		t.Errorf("nil ECDSA signer: err %v, want ErrUnsupportedKey", err)
	}
	if _, err := NewECDSAP256Signer(p256, ""); err == nil {
		t.Error("ECDSA signer with empty keyID: want error")
	}
	if _, err := NewEd25519Signer(edKey[:10], "k"); !errors.Is(err, ErrUnsupportedKey) {
		t.Errorf("short Ed25519 key: err %v, want ErrUnsupportedKey", err)
	}
	if _, err := NewEd25519Signer(edKey, ""); err == nil {
		t.Error("Ed25519 signer with empty keyID: want error")
	}
}

func TestParsePublicKeyPEM(t *testing.T) {
	if _, err := ParsePublicKeyPEM([]byte("not pem")); err == nil {
		t.Error("ParsePublicKeyPEM of junk: want error")
	}
	// A private key must not be accepted where a public key is expected.
	b, err := os.ReadFile(vectorPath("test-private-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePublicKeyPEM(b); err == nil {
		t.Error("ParsePublicKeyPEM of a PRIVATE KEY block: want error")
	}
}

func FuzzDecode(f *testing.F) {
	for _, name := range []string{"pass/KST-CTA-0001.dsse.json", "fail/wrong-key.dsse.json"} {
		b, err := os.ReadFile(vectorPath(name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte(`{"payloadType":"t","payload":"e30=","signatures":[{"keyid":"k","sig":"AA=="}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		env, err := Decode(data)
		if err != nil {
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("Decode error %v is not ErrMalformed", err)
			}
			return
		}
		wire, err := env.Encode()
		if err != nil {
			t.Fatalf("Encode of decoded envelope: %v", err)
		}
		again, err := Decode(wire)
		if err != nil {
			t.Fatalf("Decode of re-encoded envelope: %v", err)
		}
		if again.PayloadType != env.PayloadType || !bytes.Equal(again.Payload, env.Payload) || len(again.Signatures) != len(env.Signatures) {
			t.Fatalf("round trip changed the envelope")
		}
		// Verifying arbitrary input must not panic.
		_, _, _ = Verify(env, env.PayloadType, map[string]crypto.PublicKey{env.Signatures[0].KeyID: ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))})
	})
}
