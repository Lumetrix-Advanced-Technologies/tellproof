package enrol

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/dsse"
)

type fakeServer struct {
	got struct {
		PublicKey string `json:"public_key"`
		Hostname  string `json:"hostname"`
	}
	auth    string
	status  int
	agentID string
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/agents/enrol" {
		http.NotFound(w, r)
		return
	}
	f.auth = r.Header.Get("Authorization")
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &f.got)
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"error":"enrolment token already used"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"agent_id": f.agentID})
}

func TestEnrolCreatesKeyAndIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	fs := &fakeServer{agentID: "agt_0123456789abcdef"}
	srv := httptest.NewServer(fs)
	defer srv.Close()

	id, err := Enrol(context.Background(), Options{Server: srv.URL, Token: "enrol-TOKEN-1", DataDir: dir, Hostname: "app-1", HTTP: srv.Client()})
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if id.AgentID != "agt_0123456789abcdef" || id.Server != srv.URL {
		t.Errorf("identity = %+v", id)
	}
	if fs.auth != "Bearer enrol-TOKEN-1" || fs.got.Hostname != "app-1" {
		t.Errorf("request auth %q hostname %q", fs.auth, fs.got.Hostname)
	}
	// The server got a PEM public key that matches the private key on disk.
	pub, err := dsse.ParsePublicKeyPEM([]byte(fs.got.PublicKey))
	if err != nil {
		t.Fatalf("server got an unusable public key: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, KeyFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v (%v); want 0600", info.Mode().Perm(), err)
	}
	if d, _ := os.Stat(dir); d.Mode().Perm() != 0o700 {
		t.Errorf("data dir mode %v; want 0700", d.Mode().Perm())
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.AgentID != id.AgentID || !loaded.Key.Public().(ed25519.PublicKey).Equal(pub) {
		t.Error("loaded identity does not match the enrolled one")
	}
	// Enrolling again is refused rather than replacing the key.
	if _, err := Enrol(context.Background(), Options{Server: srv.URL, Token: "t2", DataDir: dir, HTTP: srv.Client()}); err == nil || !strings.Contains(err.Error(), "already enrolled") {
		t.Errorf("second enrol: %v", err)
	}
}

func TestEnrolFailuresLeaveNothingBehindAndNeverEchoTheToken(t *testing.T) {
	for name, fs := range map[string]*fakeServer{
		"rejected":     {status: http.StatusUnauthorized},
		"bad agent id": {agentID: "agent-1; rm -rf"},
		"server error": {status: http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			srv := httptest.NewServer(fs)
			defer srv.Close()
			_, err := Enrol(context.Background(), Options{Server: srv.URL, Token: "enrol-SECRET-TOKEN", DataDir: dir, HTTP: srv.Client()})
			if err == nil {
				t.Fatal("Enrol succeeded")
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error %q contains the token", err)
			}
			if _, err := os.Stat(filepath.Join(dir, KeyFile)); !os.IsNotExist(err) {
				t.Error("a key was left behind after a failed enrolment")
			}
		})
	}
}

func TestEnrolRequiresHTTPSExceptLocalhost(t *testing.T) {
	_, err := Enrol(context.Background(), Options{Server: "http://tellproof.example", Token: "t", DataDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("plain http to a remote host: %v", err)
	}
	if _, err := Enrol(context.Background(), Options{Server: "https://x", DataDir: t.TempDir()}); err == nil {
		t.Error("missing token accepted")
	}
}

func TestLoadRefusesLooseKeyFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	srv := httptest.NewServer(&fakeServer{agentID: "agt_0123456789abcdef"})
	defer srv.Close()
	if _, err := Enrol(context.Background(), Options{Server: srv.URL, Token: "t", DataDir: dir, HTTP: srv.Client()}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, KeyFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("Load with a world-readable key: %v", err)
	}
	if _, err := Load(t.TempDir()); err == nil || !strings.Contains(err.Error(), "not enrolled") {
		t.Errorf("Load of an empty dir: %v", err)
	}
}
