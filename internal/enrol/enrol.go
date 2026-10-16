// Package enrol gives the evidence agent its identity (docs/architecture.md
// §3): an Ed25519 key generated on the customer's machine, registered with
// the control plane once with a one-time enrolment token, and kept in the
// data directory readable only by the agent's user. The private key never
// leaves the machine; the token is never stored, logged or echoed.
package enrol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Files in the data directory.
const (
	KeyFile      = "agent.key"  // PKCS#8 PEM, mode 0600
	IdentityFile = "agent.json" // agent ID and server
)

var agentIDPattern = regexp.MustCompile(`^agt_[A-Za-z0-9]{8,40}$`)

// Identity is an enrolled agent.
type Identity struct {
	AgentID string             `json:"agent_id"`
	Server  string             `json:"server"`
	Key     ed25519.PrivateKey `json:"-"`
}

// Options for Enrol.
type Options struct {
	Server   string // control plane base URL, https (http only for localhost)
	Token    string // one-time enrolment token from the web app
	DataDir  string
	Hostname string // reported for display; default os.Hostname()
	HTTP     *http.Client
}

// Enrol generates a key, registers its public half and writes the identity.
// It refuses to replace an existing enrolment; on failure it leaves nothing.
func Enrol(ctx context.Context, o Options) (*Identity, error) {
	if o.Token == "" {
		return nil, errors.New("enrol: an enrolment token is required")
	}
	if err := checkServer(o.Server); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(o.DataDir, IdentityFile)); err == nil {
		existing, _ := readIdentity(o.DataDir)
		return nil, fmt.Errorf("enrol: already enrolled as %s; remove %s to enrol again", existing.AgentID, o.DataDir)
	}
	if o.Hostname == "" {
		o.Hostname, _ = os.Hostname()
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("enrol: generating the key: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{
		"public_key": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})),
		"hostname":   o.Hostname,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.Server+"/v1/agents/enrol", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.Token)
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("enrol: contacting %s: %w", o.Server, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("enrol: the server refused the enrolment (HTTP %d); get a new token from the web app", resp.StatusCode)
	}
	var out struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || !agentIDPattern.MatchString(out.AgentID) {
		return nil, errors.New("enrol: the server's reply has no valid agent ID")
	}
	id := &Identity{AgentID: out.AgentID, Server: o.Server, Key: priv}
	if err := write(o.DataDir, id); err != nil {
		return nil, err
	}
	return id, nil
}

func checkServer(server string) error {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return errors.New("enrol: the server must be a URL such as https://api.plumbline.example")
	}
	host := u.Hostname()
	local := host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return errors.New("enrol: the server must use https (plain http is allowed only for localhost)")
	}
	return nil
}

// write stores the key (0600) then the identity, in a 0700 directory. The
// identity file is written last, so its presence means the enrolment finished.
func write(dir string, id *Identity) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("enrol: data directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(id.Key)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, KeyFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(id, "", "  ")
	if err := writeFile(filepath.Join(dir, IdentityFile), append(b, '\n')); err != nil {
		_ = os.Remove(filepath.Join(dir, KeyFile))
		return err
	}
	return nil
}

// writeFile writes atomically: a temp file in the same directory, fsync, rename.
func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readIdentity(dir string) (*Identity, error) {
	b, err := os.ReadFile(filepath.Join(dir, IdentityFile))
	if err != nil {
		return nil, err
	}
	var id Identity
	if err := json.Unmarshal(b, &id); err != nil || !agentIDPattern.MatchString(id.AgentID) {
		return nil, errors.New("enrol: the identity file is damaged")
	}
	return &id, nil
}

// Load reads an enrolled identity, refusing a key file others can read.
func Load(dir string) (*Identity, error) {
	id, err := readIdentity(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("enrol: not enrolled: run `plumbline-agent enrol --token <token>` first (data dir %s)", dir)
	}
	if err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, KeyFile)
	info, err := os.Stat(keyPath)
	if err != nil {
		return nil, fmt.Errorf("enrol: key file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("enrol: %s must be readable only by the agent's user (chmod 600)", keyPath)
	}
	b, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("enrol: the key file is not a PEM private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("enrol: the key file: %w", err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("enrol: the key is not Ed25519")
	}
	id.Key = priv
	return id, nil
}
