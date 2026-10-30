package enrol

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PublicKeyFile holds the agent's public key (PKIX PEM, readable by anyone),
// so a copy of a local evidence log can be checked without the private key.
const PublicKeyFile = "agent.pub"

// LocalPrefix starts the ID of an agent in local mode.
const LocalPrefix = "agt_local"

// IsLocal reports whether the identity is a local-mode one: no server ever
// registered it, so its batches stay on this machine.
func (id *Identity) IsLocal() bool {
	return id.Server == "" && strings.HasPrefix(id.AgentID, LocalPrefix)
}

// ErrNotLocal is a data directory already enrolled with a server.
var ErrNotLocal = errors.New("enrol: this data directory is enrolled with a Tellproof server; use another data directory for local mode")

// LocalIdentity returns the data directory's local-mode identity, creating
// one (a fresh Ed25519 key, no server, no token) the first time. Its agent ID
// is derived from the public key, so it names this key and no other.
func LocalIdentity(dir string) (*Identity, error) {
	if _, err := os.Stat(filepath.Join(dir, IdentityFile)); err == nil {
		id, err := Load(dir)
		if err != nil {
			return nil, err
		}
		if !id.IsLocal() {
			return nil, ErrNotLocal
		}
		return id, writePublic(dir, id.Key.Public().(ed25519.PublicKey))
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(pub)
	id := &Identity{AgentID: LocalPrefix + hex.EncodeToString(sum[:12]), Key: priv}
	if err := write(dir, id); err != nil {
		return nil, err
	}
	return id, writePublic(dir, pub)
}

func writePublic(dir string, pub ed25519.PublicKey) error {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return err
	}
	b := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	path := filepath.Join(dir, PublicKeyFile)
	if old, err := os.ReadFile(path); err == nil && string(old) == string(b) {
		return nil
	}
	if err := writeFile(path, b); err != nil {
		return err
	}
	return os.Chmod(path, 0o644)
}

// ErrKeyMismatch: agent.pub is not the key of the agent named in agent.json.
// The log cannot be trusted relative to that key: a verification failure,
// not a configuration error.
var ErrKeyMismatch = errors.New("agent.pub does not belong to the agent in agent.json (the key or the agent ID was replaced)")

// LoadPublic reads the public key a local evidence log was signed with and
// the agent ID it belongs to (from agent.json), without the private key.
// A key that is not that agent's returns the agent ID and ErrKeyMismatch.
func LoadPublic(dir string) (agentID string, pub ed25519.PublicKey, err error) {
	id, err := readIdentity(dir)
	if err != nil {
		return "", nil, fmt.Errorf("enrol: %s: %w", filepath.Join(dir, IdentityFile), err)
	}
	b, err := os.ReadFile(filepath.Join(dir, PublicKeyFile))
	if err != nil {
		return "", nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PUBLIC KEY" {
		return "", nil, errors.New("enrol: agent.pub is not a PEM public key")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", nil, err
	}
	p, ok := k.(ed25519.PublicKey)
	if !ok {
		return "", nil, errors.New("enrol: agent.pub is not an Ed25519 key")
	}
	if sum := sha256.Sum256(p); id.AgentID != LocalPrefix+hex.EncodeToString(sum[:12]) {
		return id.AgentID, nil, ErrKeyMismatch
	}
	return id.AgentID, p, nil
}
