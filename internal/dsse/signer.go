package dsse

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// Signer signs a DSSE PAE. Implementations: ECDSAP256Signer (tests and local
// dev), Ed25519Signer (agent), and the control plane's KMS signer (P4-5).
type Signer interface {
	Sign(ctx context.Context, pae []byte) (sig []byte, keyID string, err error)
}

// Sign builds an envelope over payload with one signature from s.
func Sign(ctx context.Context, s Signer, payloadType string, payload []byte) (*Envelope, error) {
	sig, keyID, err := s.Sign(ctx, PAE(payloadType, payload))
	if err != nil {
		return nil, fmt.Errorf("dsse: sign: %w", err)
	}
	if keyID == "" || len(sig) == 0 {
		return nil, errors.New("dsse: signer returned an empty keyID or signature")
	}
	return &Envelope{
		PayloadType: payloadType,
		Payload:     bytes.Clone(payload),
		Signatures:  []Signature{{KeyID: keyID, Sig: sig}},
	}, nil
}

// ECDSAP256Signer signs with a local P-256 key. Signatures are SHA-256 digests
// signed and encoded in ASN.1 DER, the same form AWS KMS returns.
type ECDSAP256Signer struct {
	key   *ecdsa.PrivateKey
	keyID string
}

// NewECDSAP256Signer returns a signer for key, which must be on P-256.
func NewECDSAP256Signer(key *ecdsa.PrivateKey, keyID string) (*ECDSAP256Signer, error) {
	if key == nil || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: ECDSA signing key must be P-256", ErrUnsupportedKey)
	}
	if keyID == "" {
		return nil, errors.New("dsse: keyID is required")
	}
	return &ECDSAP256Signer{key: key, keyID: keyID}, nil
}

// Sign implements Signer.
func (s *ECDSAP256Signer) Sign(ctx context.Context, pae []byte) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(pae)
	sig, err := ecdsa.SignASN1(rand.Reader, s.key, digest[:])
	if err != nil {
		return nil, "", fmt.Errorf("ecdsa sign: %w", err)
	}
	return sig, s.keyID, nil
}

// Ed25519Signer signs with an Ed25519 key, as the evidence agent does.
type Ed25519Signer struct {
	key   ed25519.PrivateKey
	keyID string
}

// NewEd25519Signer returns a signer for key.
func NewEd25519Signer(key ed25519.PrivateKey, keyID string) (*Ed25519Signer, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: Ed25519 private key is %d bytes", ErrUnsupportedKey, len(key))
	}
	if keyID == "" {
		return nil, errors.New("dsse: keyID is required")
	}
	return &Ed25519Signer{key: key, keyID: keyID}, nil
}

// Sign implements Signer.
func (s *Ed25519Signer) Sign(ctx context.Context, pae []byte) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return ed25519.Sign(s.key, pae), s.keyID, nil
}
