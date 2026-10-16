// Package dsse implements Dead Simple Signing Envelopes as used by Plumbline
// packs and agent batches (docs/architecture.md §2.2).
//
// Packs are signed with ECDSA P-256 over SHA-256, signatures in ASN.1 DER (what
// AWS KMS returns). Agent batches are signed with Ed25519.
package dsse

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// Payload types Plumbline signs.
const (
	PayloadTypeInToto = "application/vnd.in-toto+json"
	PayloadTypeBatch  = "application/vnd.lumetrix.plumbline.batch+json"
)

// Distinct failures. A signature that does not verify is ErrBadSignature
// whether the payload was changed or another key signed it: with one key and
// one signature the two cannot be told apart (D-014).
var (
	ErrMalformed      = errors.New("dsse: malformed envelope")
	ErrNoSignatures   = errors.New("dsse: envelope has no signatures")
	ErrPayloadType    = errors.New("dsse: unexpected payload type")
	ErrUnknownKey     = errors.New("dsse: no signature from a trusted key")
	ErrUnsupportedKey = errors.New("dsse: unsupported key")
	ErrBadSignature   = errors.New("dsse: signature does not verify")
)

// Envelope is a DSSE envelope. In JSON, Payload and Sig are standard base64.
type Envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     []byte      `json:"payload"`
	Signatures  []Signature `json:"signatures"`
}

// Signature is one signature over the envelope's PAE.
type Signature struct {
	KeyID string `json:"keyid"`
	Sig   []byte `json:"sig"`
}

// PAE is the DSSE pre-authentication encoding, the bytes that are signed:
// "DSSEv1" SP len(type) SP type SP len(body) SP body, lengths in ASCII decimal bytes.
func PAE(payloadType string, payload []byte) []byte {
	b := make([]byte, 0, len("DSSEv1   ")+len(payloadType)+len(payload)+40)
	b = append(b, "DSSEv1 "...)
	b = strconv.AppendInt(b, int64(len(payloadType)), 10)
	b = append(b, ' ')
	b = append(b, payloadType...)
	b = append(b, ' ')
	b = strconv.AppendInt(b, int64(len(payload)), 10)
	b = append(b, ' ')
	return append(b, payload...)
}

// Decode parses an envelope strictly: no unknown fields, no trailing data,
// standard padded base64, a payload type, a payload and at least one signature.
// Callers bound the input size.
func Decode(data []byte) (*Envelope, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var e Envelope
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: unexpected data after the envelope", ErrMalformed)
	}
	switch {
	case e.PayloadType == "":
		return nil, fmt.Errorf("%w: payloadType is missing", ErrMalformed)
	case len(e.Payload) == 0:
		return nil, fmt.Errorf("%w: payload is missing", ErrMalformed)
	case len(e.Signatures) == 0:
		return nil, fmt.Errorf("%w: signatures is empty", ErrMalformed)
	}
	for i, s := range e.Signatures {
		if len(s.Sig) == 0 {
			return nil, fmt.Errorf("%w: signatures[%d].sig is empty", ErrMalformed, i)
		}
	}
	return &e, nil
}

// Encode returns the envelope's JSON form.
func (e *Envelope) Encode() ([]byte, error) {
	return json.Marshal(e)
}

// Verify checks that at least one signature verifies under the trusted key
// named by its keyid, and that the envelope carries payloadType. It returns the
// payload, which callers must use instead of re-reading the envelope, and the
// keyid that verified it.
func Verify(env *Envelope, payloadType string, keys map[string]crypto.PublicKey) (payload []byte, keyID string, err error) {
	if env == nil {
		return nil, "", fmt.Errorf("%w: nil envelope", ErrMalformed)
	}
	if env.PayloadType != payloadType {
		return nil, "", fmt.Errorf("%w %q, want %q", ErrPayloadType, env.PayloadType, payloadType)
	}
	if len(env.Signatures) == 0 {
		return nil, "", ErrNoSignatures
	}
	pae := PAE(env.PayloadType, env.Payload)
	var firstErr error
	for _, s := range env.Signatures {
		pub, ok := keys[s.KeyID]
		if s.KeyID == "" || !ok {
			continue
		}
		if err := verifyOne(pub, pae, s.Sig); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%w (keyid %q)", err, s.KeyID)
			}
			continue
		}
		return bytes.Clone(env.Payload), s.KeyID, nil
	}
	if firstErr != nil {
		return nil, "", firstErr
	}
	ids := make([]string, len(env.Signatures))
	for i, s := range env.Signatures {
		ids[i] = strconv.Quote(s.KeyID)
	}
	return nil, "", fmt.Errorf("%w: envelope keyids %v", ErrUnknownKey, ids)
}

func verifyOne(pub crypto.PublicKey, pae, sig []byte) error {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if k == nil || k.Curve != elliptic.P256() {
			return fmt.Errorf("%w: ECDSA key is not P-256", ErrUnsupportedKey)
		}
		digest := sha256.Sum256(pae)
		if !ecdsa.VerifyASN1(k, digest[:], sig) {
			return ErrBadSignature
		}
		return nil
	case ed25519.PublicKey:
		if len(k) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: Ed25519 key is %d bytes", ErrUnsupportedKey, len(k))
		}
		if !ed25519.Verify(k, pae, sig) {
			return ErrBadSignature
		}
		return nil
	}
	return fmt.Errorf("%w: %T", ErrUnsupportedKey, pub)
}

// ParsePublicKeyPEM reads a "PUBLIC KEY" PEM block (SubjectPublicKeyInfo) holding
// an ECDSA P-256 or Ed25519 key.
func ParsePublicKeyPEM(data []byte) (crypto.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("dsse: no PUBLIC KEY PEM block")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("dsse: parse public key: %w", err)
	}
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() {
			return nil, fmt.Errorf("%w: ECDSA key is not P-256", ErrUnsupportedKey)
		}
		return k, nil
	case ed25519.PublicKey:
		return k, nil
	}
	return nil, fmt.Errorf("%w: %T", ErrUnsupportedKey, pub)
}
