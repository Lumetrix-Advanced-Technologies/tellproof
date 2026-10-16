package pack

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"time"
)

type jwks struct {
	Keys    []jwk        `json:"keys"`
	Revoked []revocation `json:"revoked"`
}

type jwk struct {
	Kty string          `json:"kty"`
	Crv string          `json:"crv"`
	Kid string          `json:"kid"`
	Use string          `json:"use"`
	Alg string          `json:"alg"`
	X   string          `json:"x"`
	Y   string          `json:"y"`
	Nbf json.RawMessage `json:"nbf"`
	Exp json.RawMessage `json:"exp"`
}

type revocation struct {
	Kid       string          `json:"kid"`
	RevokedAt json.RawMessage `json:"revoked_at"`
	Reason    *string         `json:"reason"`
}

// Key is one issuer key and when it may vouch for a pack (D-042).
type Key struct {
	Public    crypto.PublicKey
	NotBefore *int64 // NumericDate seconds; nil: no lower bound
	Expires   *int64 // NumericDate seconds; nil: no upper bound
}

// KeySet is the issuer's JWKS: keys by kid, and revoked kids.
type KeySet struct {
	keys    map[string]Key
	revoked map[string]int64
}

// Errors explaining why a verified signature does not count (Result.SignatureErr).
var (
	ErrKeyRevoked     = errors.New("pack: the signing key has been revoked")
	ErrKeyNotYetValid = errors.New("pack: the pack was issued before its signing key's nbf")
	ErrKeyExpired     = errors.New("pack: the pack was issued at or after its signing key's exp")
	ErrIssuedAt       = errors.New("pack: issued_at is not an RFC 3339 date-time, so the key's validity cannot be checked")
)

// KeySetOf makes a key set of keys with no time bounds (tests, the issuer's own check).
func KeySetOf(keys map[string]crypto.PublicKey) *KeySet {
	ks := &KeySet{keys: map[string]Key{}, revoked: map[string]int64{}}
	for kid, pub := range keys {
		ks.keys[kid] = Key{Public: pub}
	}
	return ks
}

// PublicKeys returns the keys by kid for signature verification.
func (ks *KeySet) PublicKeys() map[string]crypto.PublicKey {
	out := make(map[string]crypto.PublicKey, len(ks.keys))
	for kid, k := range ks.keys {
		out[kid] = k.Public
	}
	return out
}

// numericDate reads a NumericDate: a JSON number (not a string: json.Number
// would take "1" too) whose value is a whole number of seconds between 0 and
// 2^53-1. (1e9 and 1000000000.0 are the same number, as every verifier's JSON
// parser sees it.) Absent: nil.
func numericDate(raw json.RawMessage, what string) (*int64, error) {
	if raw == nil {
		return nil, nil
	}
	bad := fmt.Errorf("%s must be a whole number of seconds (NumericDate)", what)
	if len(raw) == 0 || !(raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9')) {
		return nil, bad
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || f != math.Trunc(f) || f < 0 || f > 1<<53-1 {
		return nil, bad
	}
	v := int64(f)
	return &v, nil
}

// ParseJWKS reads the issuer's JWKS (D-042): ES256 keys (EC P-256) by kid, each
// with an optional nbf and exp, and an optional "revoked" list of {kid,
// revoked_at, reason}. Unknown members are rejected, so a field this verifier
// does not understand is never silently ignored.
func ParseJWKS(data []byte) (*KeySet, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var set jwks
	if err := dec.Decode(&set); err != nil {
		return nil, fmt.Errorf("pack: jwks: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("pack: jwks: unexpected data after the JSON document")
	}
	if len(set.Keys) == 0 {
		return nil, errors.New("pack: jwks: no keys")
	}
	ks := &KeySet{keys: make(map[string]Key, len(set.Keys)), revoked: map[string]int64{}}
	for i, k := range set.Keys {
		pub, err := k.publicKey()
		if err != nil {
			return nil, fmt.Errorf("pack: jwks: keys[%d] (kid %q): %w", i, k.Kid, err)
		}
		if _, dup := ks.keys[k.Kid]; dup {
			return nil, fmt.Errorf("pack: jwks: duplicate kid %q", k.Kid)
		}
		nbf, err := numericDate(k.Nbf, "nbf")
		if err != nil {
			return nil, fmt.Errorf("pack: jwks: keys[%d]: %w", i, err)
		}
		exp, err := numericDate(k.Exp, "exp")
		if err != nil {
			return nil, fmt.Errorf("pack: jwks: keys[%d]: %w", i, err)
		}
		if nbf != nil && exp != nil && *exp <= *nbf {
			return nil, fmt.Errorf("pack: jwks: keys[%d]: exp must be after nbf", i)
		}
		ks.keys[k.Kid] = Key{Public: pub, NotBefore: nbf, Expires: exp}
	}
	for i, r := range set.Revoked {
		at, err := numericDate(r.RevokedAt, "revoked_at")
		if err != nil || at == nil || r.Kid == "" {
			return nil, fmt.Errorf("pack: jwks: revoked[%d] needs a kid and a revoked_at NumericDate", i)
		}
		if _, dup := ks.revoked[r.Kid]; dup {
			return nil, fmt.Errorf("pack: jwks: kid %q is revoked twice", r.Kid)
		}
		ks.revoked[r.Kid] = *at
	}
	return ks, nil
}

var issuedAtRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)

// Vouches reports whether the key kid may vouch for a pack issued at issuedAt
// (RFC 3339): not revoked, and nbf <= issued_at < exp in whole seconds. A
// revoked key vouches for nothing: whoever holds it can backdate issued_at.
func (ks *KeySet) Vouches(kid, issuedAt string) error {
	if _, revoked := ks.revoked[kid]; revoked {
		return ErrKeyRevoked
	}
	k := ks.keys[kid]
	if k.NotBefore == nil && k.Expires == nil {
		return nil
	}
	if !issuedAtRE.MatchString(issuedAt) {
		return ErrIssuedAt
	}
	t, err := time.Parse(time.RFC3339Nano, issuedAt)
	if err != nil {
		return ErrIssuedAt
	}
	sec := t.Unix()
	if k.NotBefore != nil && sec < *k.NotBefore {
		return ErrKeyNotYetValid
	}
	if k.Expires != nil && sec >= *k.Expires {
		return ErrKeyExpired
	}
	return nil
}

func (k jwk) publicKey() (*ecdsa.PublicKey, error) {
	switch {
	case k.Kid == "":
		return nil, errors.New("kid is required")
	case k.Kty != "EC" || k.Crv != "P-256":
		return nil, fmt.Errorf("want kty EC and crv P-256, got %q %q", k.Kty, k.Crv)
	case k.Alg != "" && k.Alg != "ES256":
		return nil, fmt.Errorf("alg %q is not ES256", k.Alg)
	case k.Use != "" && k.Use != "sig":
		return nil, fmt.Errorf("use %q is not sig", k.Use)
	}
	x, errX := base64.RawURLEncoding.DecodeString(k.X)
	y, errY := base64.RawURLEncoding.DecodeString(k.Y)
	if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
		return nil, errors.New("x and y must be 32-byte unpadded base64url values")
	}
	point := append(append([]byte{0x04}, x...), y...)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
	if err != nil {
		return nil, fmt.Errorf("invalid P-256 point: %w", err)
	}
	return pub, nil
}
