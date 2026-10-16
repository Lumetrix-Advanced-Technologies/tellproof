package enrol

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	id, err := LocalIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !id.IsLocal() || !strings.HasPrefix(id.AgentID, LocalPrefix) || !agentIDPattern.MatchString(id.AgentID) {
		t.Fatalf("identity %+v", id)
	}
	again, err := LocalIdentity(dir)
	if err != nil || again.AgentID != id.AgentID || !again.Key.Equal(id.Key) {
		t.Fatalf("a second start made a new identity: %v", err)
	}
	gotID, pub, err := LoadPublic(dir)
	if err != nil || gotID != id.AgentID || !pub.Equal(id.Key.Public()) {
		t.Fatalf("LoadPublic = %s, %v", gotID, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, KeyFile)); info.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", info.Mode())
	}
}

func TestLocalIdentityRefusesAnEnrolledDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := LocalIdentity(dir); err != nil {
		t.Fatal(err)
	}
	// Pretend a server enrolled this directory.
	_ = os.WriteFile(filepath.Join(dir, IdentityFile), []byte(`{"agent_id": "agt_0123456789abcdef", "server": "https://plumbline.example"}`), 0o600)
	if _, err := LocalIdentity(dir); !errors.Is(err, ErrNotLocal) {
		t.Fatalf("enrolled directory: %v", err)
	}
}
