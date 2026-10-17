package cievidence

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/dsse"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/jcs"
)

// NewKey makes an Ed25519 signing key, as PKCS #8 and PKIX PEM.
func NewKey() (private, public []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	pk, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	pb, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pb}), nil
}

// ParsePrivate reads a PKCS #8 PEM Ed25519 key.
func ParsePrivate(b []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("cievidence: the signing key is not a PKCS #8 PEM private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("cievidence: the signing key cannot be read")
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("cievidence: the signing key is not Ed25519")
	}
	return priv, nil
}

// ParsePublic reads a PKIX PEM Ed25519 key.
func ParsePublic(b []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("cievidence: the public key is not a PEM public key")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("cievidence: the public key is not Ed25519")
	}
	return pub, nil
}

// KeyID names a public key: "ci-" and the first 16 hex digits of its SHA-256.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "ci-" + hex.EncodeToString(sum[:8])
}

// Payload is the statement in canonical JSON (RFC 8785).
func Payload(s *Statement) ([]byte, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return jcs.Canonicalize(raw)
}

// Sign signs the statement into a DSSE envelope.
func Sign(ctx context.Context, s *Statement, priv ed25519.PrivateKey) (*dsse.Envelope, error) {
	payload, err := Payload(s)
	if err != nil {
		return nil, err
	}
	signer, err := dsse.NewEd25519Signer(priv, KeyID(priv.Public().(ed25519.PublicKey)))
	if err != nil {
		return nil, err
	}
	return dsse.Sign(ctx, signer, dsse.PayloadTypeInToto, payload)
}

// Open reads a signed envelope (checked with pub) or, with unsigned, a bare
// statement. It reports whether the statement was signed.
func Open(b []byte, pub ed25519.PublicKey, unsigned bool) (*Statement, bool, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if _, isEnvelope := probe["payloadType"]; !isEnvelope {
		if !unsigned {
			return nil, false, errors.New("cievidence: this is an unsigned statement; pass --unsigned to read it (and verify its GitHub attestation with gh attestation verify)")
		}
		s, err := Decode(b)
		return s, false, err
	}
	if pub == nil {
		return nil, true, errors.New("cievidence: a signed statement needs the public key (--pub)")
	}
	env, err := dsse.Decode(b)
	if err != nil {
		return nil, true, err
	}
	payload, _, err := dsse.Verify(env, dsse.PayloadTypeInToto, map[string]crypto.PublicKey{KeyID(pub): pub})
	if err != nil {
		return nil, true, err
	}
	s, err := Decode(payload)
	return s, true, err
}
