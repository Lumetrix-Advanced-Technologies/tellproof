package pack

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/dsse"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/jcs"
)

func vectorPath(name string) string {
	return filepath.Join("..", "..", "spec", "vectors", name)
}

func loadEnvelope(t testing.TB, name string) *dsse.Envelope {
	t.Helper()
	b, err := os.ReadFile(vectorPath(name))
	if err != nil {
		t.Fatal(err)
	}
	env, err := dsse.Decode(b)
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return env
}

func testJWKS(t testing.TB) *KeySet {
	t.Helper()
	b, err := os.ReadFile(vectorPath("test-jwks.json"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := ParseJWKS(b)
	if err != nil {
		t.Fatalf("ParseJWKS: %v", err)
	}
	return keys
}

type manifestCase struct {
	Name   string          `json:"name"`
	File   string          `json:"file"`
	Prev   *string         `json:"prev"`
	Expect json.RawMessage `json:"expect"`
}

func loadManifest(t testing.TB) []manifestCase {
	t.Helper()
	b, err := os.ReadFile(vectorPath("manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Cases []manifestCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Cases) != 8 {
		t.Fatalf("manifest has %d cases, want 8", len(m.Cases))
	}
	return m.Cases
}

// checks is the manifest's expected-result shape.
type checks struct {
	Signature     bool  `json:"signature"`
	SubjectDigest bool  `json:"subject_digest"`
	Chain         *bool `json:"chain"`
	Merkle        *bool `json:"merkle"`
	Verified      bool  `json:"verified"`
}

func checksOf(r Result) checks {
	return checks{r.Signature, r.SubjectDigest, r.Chain, r.Merkle, r.Verified}
}

func (c checks) String() string {
	b, _ := json.Marshal(c)
	return string(b)
}

// Acceptance: results for every file in spec/vectors/manifest.json match its
// expected result exactly.
func TestVectorManifest(t *testing.T) {
	keys := testJWKS(t)
	for _, c := range loadManifest(t) {
		t.Run(c.Name, func(t *testing.T) {
			dec := json.NewDecoder(bytes.NewReader(c.Expect))
			dec.DisallowUnknownFields()
			var want checks
			if err := dec.Decode(&want); err != nil {
				t.Fatalf("manifest expect: %v", err)
			}
			var prev *dsse.Envelope
			if c.Prev != nil {
				prev = loadEnvelope(t, *c.Prev)
			}
			got, err := Verify(loadEnvelope(t, c.File), keys, prev)
			if err != nil {
				t.Fatalf("Verify error: %v", err)
			}
			if checksOf(got).String() != want.String() {
				t.Errorf("Verify = %s\nmanifest  %s", checksOf(got), want)
			}
		})
	}
}

// The two signature failures that dsse cannot tell apart (D-014) differ here:
// only a changed payload also breaks the subject digest.
func TestTamperedVersusWrongKey(t *testing.T) {
	keys := testJWKS(t)
	tampered, err := Verify(loadEnvelope(t, "fail/tampered-payload.dsse.json"), keys, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := Verify(loadEnvelope(t, "fail/wrong-key.dsse.json"), keys, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tampered.Signature || wrongKey.Signature {
		t.Fatal("both must fail the signature check")
	}
	if tampered.SubjectDigest || !wrongKey.SubjectDigest {
		t.Errorf("subject_digest: tampered=%v wrong-key=%v; want false, true", tampered.SubjectDigest, wrongKey.SubjectDigest)
	}
	if !errors.Is(tampered.SignatureErr, dsse.ErrBadSignature) || !errors.Is(wrongKey.SignatureErr, dsse.ErrBadSignature) {
		t.Errorf("SignatureErr = %v, %v; want ErrBadSignature", tampered.SignatureErr, wrongKey.SignatureErr)
	}
}

func TestDigestAndChain(t *testing.T) {
	p1 := loadEnvelope(t, "pass/KST-CTA-0001.dsse.json")
	p2 := loadEnvelope(t, "pass/KST-CTA-0002.dsse.json")
	p3 := loadEnvelope(t, "pass/KST-CTA-0003.dsse.json")
	if got, want := Digest(p1), "sha256:4e9e3cd383c7d979e54864e1e2eef2a58c6590e20ce706b38f3dd8b7b8ecabb6"; got != want {
		t.Errorf("Digest(pass 1) = %s, want %s", got, want)
	}
	st := func(e *dsse.Envelope) *Statement {
		s, err := DecodeStatement(e.Payload)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	tests := []struct {
		name       string
		prev, next *dsse.Envelope
		want       bool
	}{
		{"1 -> 2", p1, p2, true},
		{"2 -> 3", p2, p3, true},
		{"skipping a pack", p1, p3, false},
		{"reversed", p2, p1, false},
		{"first pack has no previous digest", p3, p1, false},
		{"self", p2, p2, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CheckChain(tt.prev, st(tt.next)); got != tt.want {
				t.Errorf("CheckChain = %v, want %v", got, tt.want)
			}
		})
	}
}

// The typed Predicate loses nothing: it re-encodes to the same canonical bytes
// the issuer signed, so P4-5 can build packs from it.
func TestPredicateRoundTrip(t *testing.T) {
	for _, name := range []string{"pass/KST-CTA-0001.dsse.json", "pass/KST-CTA-0002.dsse.json", "pass/KST-CTA-0003.dsse.json"} {
		t.Run(name, func(t *testing.T) {
			st, err := DecodeStatement(loadEnvelope(t, name).Payload)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(st.Predicate)
			if err != nil {
				t.Fatal(err)
			}
			got, err := jcs.Canonicalize(b)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, st.canonicalPredicate) {
				t.Errorf("typed predicate re-encodes differently (%d vs %d bytes)", len(got), len(st.canonicalPredicate))
			}
		})
	}
}

// statementMap returns pass vector 1's statement as a generic value to edit.
func statementMap(t *testing.T) map[string]any {
	t.Helper()
	v, err := jcs.Parse(loadEnvelope(t, "pass/KST-CTA-0001.dsse.json").Payload)
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}

func predicateOf(st map[string]any) map[string]any { return st["predicate"].(map[string]any) }

// resign fixes the subject digest of st, signs it with a fresh key and returns
// the envelope and the trusted keys.
func resign(t *testing.T, st map[string]any) (*dsse.Envelope, map[string]crypto.PublicKey) {
	t.Helper()
	pred, err := jcs.Marshal(st["predicate"])
	if err != nil {
		t.Fatal(err)
	}
	st["subject"].([]any)[0].(map[string]any)["digest"].(map[string]any)["sha256"] = sha256Hex(pred)
	payload, err := jcs.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return signPayload(t, payload)
}

func signPayload(t *testing.T, payload []byte) (*dsse.Envelope, map[string]crypto.PublicKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := dsse.NewECDSAP256Signer(key, "test-kid")
	if err != nil {
		t.Fatal(err)
	}
	env, err := dsse.Sign(context.Background(), s, dsse.PayloadTypeInToto, payload)
	if err != nil {
		t.Fatal(err)
	}
	return env, map[string]crypto.PublicKey{"test-kid": &key.PublicKey}
}

// A pack may disclose no trace (schema: disclosed is null). Then the Merkle
// check is not run, like the chain check without a previous pack (D-016).
func TestVerifyWithoutDisclosure(t *testing.T) {
	st := statementMap(t)
	predicateOf(st)["commitments"].(map[string]any)["disclosed"] = nil
	env, keys := resign(t, st)
	r, err := Verify(env, KeySetOf(keys), nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Merkle != nil || r.Chain != nil || !r.Signature || !r.SubjectDigest || !r.Verified {
		t.Errorf("Verify = %s; want merkle and chain null, verified", checksOf(r))
	}
}

func TestVerifyResultDetails(t *testing.T) {
	r, err := Verify(loadEnvelope(t, "pass/KST-CTA-0002.dsse.json"), testJWKS(t), loadEnvelope(t, "pass/KST-CTA-0001.dsse.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.PackID != "KST-CTA-0002" || r.SeqNo != 2 || r.KeyID != "test-p256-2026-10" || r.SignatureErr != nil {
		t.Errorf("details = pack %q seq %d kid %q sigErr %v", r.PackID, r.SeqNo, r.KeyID, r.SignatureErr)
	}
	if want := Digest(loadEnvelope(t, "pass/KST-CTA-0002.dsse.json")); r.Digest != want {
		t.Errorf("Digest = %s, want %s", r.Digest, want)
	}
	if b, err := json.Marshal(r); err != nil || !strings.Contains(string(b), `"subject_digest":true`) || strings.Contains(string(b), "SignatureErr") {
		t.Errorf("JSON form = %s (err %v)", b, err)
	}
}

func TestVerifySignatureDependsOnKeys(t *testing.T) {
	env := loadEnvelope(t, "pass/KST-CTA-0001.dsse.json")
	_, otherKeys := signPayload(t, []byte("{}"))
	for name, keys := range map[string]map[string]crypto.PublicKey{
		"kid not in JWKS":           {"other-kid": testJWKS(t).PublicKeys()["test-p256-2026-10"]},
		"another key under the kid": {"test-p256-2026-10": otherKeys["test-kid"]},
		"empty JWKS":                nil,
	} {
		t.Run(name, func(t *testing.T) {
			r, err := Verify(env, KeySetOf(keys), nil)
			if err != nil {
				t.Fatal(err)
			}
			if r.Signature || r.Verified || !r.SubjectDigest || r.SignatureErr == nil {
				t.Errorf("Verify = %s, sigErr %v; want signature false, other checks unaffected", checksOf(r), r.SignatureErr)
			}
		})
	}
}

// Payloads that are not a v0.1 pack statement cannot be evaluated: Verify
// returns an error and never reports verified.
func TestVerifyMalformedStatement(t *testing.T) {
	edit := func(f func(st map[string]any)) func(t *testing.T) []byte {
		return func(t *testing.T) []byte {
			st := statementMap(t)
			f(st)
			b, err := jcs.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
	}
	raw := func(s string) func(t *testing.T) []byte { return func(*testing.T) []byte { return []byte(s) } }
	tests := []struct {
		name    string
		payload func(t *testing.T) []byte
		wantErr string
	}{
		{"not JSON", raw(`not json`), "statement"},
		{"duplicate keys", raw(`{"_type":"a","_type":"b"}`), "duplicate"},
		{"wrong _type", edit(func(st map[string]any) { st["_type"] = "https://in-toto.io/Statement/v0.1" }), "_type"},
		{"wrong predicateType", edit(func(st map[string]any) { st["predicateType"] = "https://example.com/other" }), "predicateType"},
		{"two subjects", edit(func(st map[string]any) { st["subject"] = append(st["subject"].([]any), st["subject"].([]any)[0]) }), "one subject"},
		{"subject without sha256", edit(func(st map[string]any) {
			st["subject"].([]any)[0].(map[string]any)["digest"] = map[string]any{"sha512": "00"}
		}), "sha256"},
		{"subject digest not hex", edit(func(st map[string]any) {
			st["subject"].([]any)[0].(map[string]any)["digest"].(map[string]any)["sha256"] = strings.Repeat("Z", 64)
		}), "sha256"},
		{"unknown predicate field", edit(func(st map[string]any) { predicateOf(st)["owner"] = "x" }), "owner"},
		{"unknown statement field", edit(func(st map[string]any) { st["extra"] = 1.0 }), "extra"},
		{"key differing only in case", edit(func(st map[string]any) {
			p := predicateOf(st)
			p["PACK_ID"] = p["pack_id"]
			delete(p, "pack_id")
		}), "exactly"},
		{"two keys differing only in case", edit(func(st map[string]any) { predicateOf(st)["Prev_Pack_Digest"] = "sha256:" + strings.Repeat("0", 64) }), "exactly"},
		{"wrong pack_schema", edit(func(st map[string]any) { predicateOf(st)["pack_schema"] = "assurance-pack/v9" }), "pack_schema"},
		{"integer beyond 2^53", edit(func(st map[string]any) {
			predicateOf(st)["commitments"].(map[string]any)["leaf_count"] = float64(1 << 54)
		}), "safe integer"},
		{"merkle root not hex", edit(func(st map[string]any) {
			predicateOf(st)["commitments"].(map[string]any)["trace_merkle_root"] = "xyz"
		}), "trace_merkle_root"},
		{"salt wrong length", edit(func(st map[string]any) {
			predicateOf(st)["commitments"].(map[string]any)["disclosed"].(map[string]any)["salt"] = "00"
		}), "salt"},
		{"proof side not L or R", edit(func(st map[string]any) {
			d := predicateOf(st)["commitments"].(map[string]any)["disclosed"].(map[string]any)
			d["proof"].([]any)[0].(map[string]any)["side"] = "X"
		}), "side"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, keys := signPayload(t, tt.payload(t))
			r, err := Verify(env, KeySetOf(keys), nil)
			if err == nil {
				t.Fatalf("Verify = %s with no error; want an error mentioning %q", checksOf(r), tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
			if r.Verified {
				t.Error("malformed pack reported as verified")
			}
		})
	}
}

func TestParseJWKS(t *testing.T) {
	keys := testJWKS(t)
	pemBytes, err := os.ReadFile(vectorPath("test-public-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pemKey, err := dsse.ParsePublicKeyPEM(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := keys.PublicKeys()["test-p256-2026-10"].(*ecdsa.PublicKey)
	if !ok || len(keys.PublicKeys()) != 1 || !got.Equal(pemKey) {
		t.Fatalf("test-jwks.json key does not equal test-public-key.pem (%d keys)", len(keys.PublicKeys()))
	}

	const x, y = "19PL3tWYOlQq5YwaFlplvZmNT45RiGERIv0sZz8uzo4", "KjLsfFbqYn6OHELPVMQHn3eTeTqo-xcxMwNxYxLpM_w"
	jwk := func(fields string) string { return `{"keys":[{` + fields + `}]}` }
	base := `"kty":"EC","crv":"P-256","kid":"k1","x":"` + x + `","y":"` + y + `"`
	if _, err := ParseJWKS([]byte(jwk(base))); err != nil {
		t.Fatalf("minimal JWK: %v", err)
	}
	offCurveY := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	tests := []struct {
		name, input string
	}{
		{"no keys", `{"keys":[]}`},
		{"not JSON", `{`},
		{"unknown top-level field", `{"keys":[{` + base + `}],"expires":1}`},
		{"unknown key field", jwk(base + `,"iat":1`)},
		{"nbf as a string", jwk(base + `,"nbf":"1"`)},
		{"negative exp", jwk(base + `,"exp":-1`)},
		{"exp not after nbf", jwk(base + `,"nbf":5,"exp":5`)},
		{"null nbf", jwk(base + `,"nbf":null`)},
		{"revocation without kid", `{"keys":[{` + base + `}],"revoked":[{"revoked_at":1}]}`},
		{"kid revoked twice", `{"keys":[{` + base + `}],"revoked":[{"kid":"k1","revoked_at":1},{"kid":"k1","revoked_at":2}]}`},
		{"missing kid", jwk(strings.Replace(base, `"kid":"k1",`, ``, 1))},
		{"duplicate kid", `{"keys":[{` + base + `},{` + base + `}]}`},
		{"RSA", jwk(strings.Replace(base, `"kty":"EC"`, `"kty":"RSA"`, 1))},
		{"P-384", jwk(strings.Replace(base, `"P-256"`, `"P-384"`, 1))},
		{"alg not ES256", jwk(base + `,"alg":"ES384"`)},
		{"use not sig", jwk(base + `,"use":"enc"`)},
		{"x padded base64", jwk(strings.Replace(base, x, x+"=", 1))},
		{"x too short", jwk(strings.Replace(base, x, x[:40], 1))},
		{"point not on curve", jwk(strings.Replace(base, y, offCurveY, 1))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if keys, err := ParseJWKS([]byte(tt.input)); err == nil {
				t.Errorf("ParseJWKS accepted it: %v", keys)
			}
		})
	}
}

// spec/vectors/extra-manifest.json: cases every verifier must agree on beyond
// the core manifest, including packs that must be refused ("error").
func TestExtraVectors(t *testing.T) {
	b, err := os.ReadFile(vectorPath("extra-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Cases []struct {
			Name   string          `json:"name"`
			File   string          `json:"file"`
			Expect json.RawMessage `json:"expect"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Cases) == 0 {
		t.Fatal("extra manifest has no cases")
	}
	keys := testJWKS(t)
	for _, c := range m.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got, err := Verify(loadEnvelope(t, c.File), keys, nil)
			if string(c.Expect) == `"error"` {
				if err == nil {
					t.Fatalf("Verify = %s with no error; the pack must be refused", checksOf(got))
				}
				if got.Verified {
					t.Error("refused pack reported as verified")
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify error: %v", err)
			}
			var want checks
			if err := json.Unmarshal(c.Expect, &want); err != nil {
				t.Fatal(err)
			}
			if checksOf(got).String() != want.String() {
				t.Errorf("Verify = %s\nexpected  %s", checksOf(got), want)
			}
		})
	}
}

// spec/vectors/key-manifest.json (D-042): which JWKS keys may vouch for a pack.
func TestKeyVectors(t *testing.T) {
	b, err := os.ReadFile(vectorPath("key-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Cases []struct {
			Name   string          `json:"name"`
			File   string          `json:"file"`
			JWKS   string          `json:"jwks"`
			Expect json.RawMessage `json:"expect"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Cases) == 0 {
		t.Fatal("no key cases")
	}
	for _, c := range m.Cases {
		t.Run(c.Name, func(t *testing.T) {
			jb, err := os.ReadFile(vectorPath(c.JWKS))
			if err != nil {
				t.Fatal(err)
			}
			keys, err := ParseJWKS(jb)
			if string(c.Expect) == `"jwks_error"` {
				if err == nil {
					t.Fatal("a malformed JWKS was loaded")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseJWKS: %v", err)
			}
			got, err := Verify(loadEnvelope(t, c.File), keys, nil)
			if err != nil {
				t.Fatal(err)
			}
			var want map[string]any
			_ = json.Unmarshal(c.Expect, &want)
			if g := checksOf(got); !reflect.DeepEqual(toMapOf(t, g.String()), want) {
				t.Errorf("checks = %s, want %s (signature error: %v)", g, c.Expect, got.SignatureErr)
			}
		})
	}
}

func toMapOf(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestVouchesExplainsWhy(t *testing.T) {
	ks, err := ParseJWKS([]byte(`{"keys":[{"kty":"EC","crv":"P-256","kid":"k","x":"19PL3tWYOlQq5YwaFlplvZmNT45RiGERIv0sZz8uzo4","y":"KjLsfFbqYn6OHELPVMQHn3eTeTqo-xcxMwNxYxLpM_w","nbf":100,"exp":200}],"revoked":[{"kid":"gone","revoked_at":1,"reason":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for at, want := range map[string]error{
		"1970-01-01T00:01:40Z":      nil,
		"1970-01-01T00:01:39.9Z":    ErrKeyNotYetValid,
		"1970-01-01T00:03:20Z":      ErrKeyExpired,
		"1970-01-01T10:03:19+10:00": nil,
		"1970-01-01 00:01:40":       ErrIssuedAt,
	} {
		if got := ks.Vouches("k", at); !errors.Is(got, want) && got != want {
			t.Errorf("Vouches(%s) = %v, want %v", at, got, want)
		}
	}
	if !errors.Is(ks.Vouches("gone", "1970-01-01T00:01:40Z"), ErrKeyRevoked) {
		t.Error("a revoked kid vouched")
	}
}
