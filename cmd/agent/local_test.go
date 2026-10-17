package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/localreport"
)

// Acceptance (D-052): with no server and no token, `tellproof-agent local`
// plus the one-line SDK init keeps a sample app's calls as signed, chained
// batches in the data directory; nothing is sent anywhere; `report` verifies
// the log and summarises it, and fails when a batch is edited or missing.
func TestLocalModeKeepsASignedLogThatReportVerifies(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	var contacted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { contacted.Add(1) }))
	defer server.Close()

	tickEvery = 200 * time.Millisecond
	defer func() { tickEvery = time.Minute }()
	listen, dash := freePort(t), freePort(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := filepath.Join(t.TempDir(), "agent.json")
	_ = os.WriteFile(cfg, []byte(`{"otlp_listen": "`+listen+`", "dashboard_listen": "`+dash+`", "aggregation": {"grace_minutes": 5, "first_batch_seconds": 1}}`), 0o600)
	// A configured server is ignored in local mode: the test fails if it is contacted.
	getenv := func(k string) string {
		return map[string]string{"TELLPROOF_SERVER": server.URL, "TELLPROOF_DATA_DIR": dataDir}[k]
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	out := &lockedBuffer{}
	go func() { done <- run(ctx, []string{"run", "--local", "--config", cfg}, getenv, out, out) }()
	for i := 0; ; i++ {
		if c, err := net.Dial("tcp", listen); err == nil {
			c.Close()
			break
		}
		if i == 100 {
			t.Fatalf("the agent did not start: %s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	app := `import tellproof
tellproof.init(agent_url="http://` + listen + `", service="local-app")
with tellproof.guardrail() as g, tellproof.prompt("support", "3"):
    with tellproof.llm_call("acme-ai", "acme-7b", region="ap-southeast-2") as c:
        c.usage(200, 50)
    g.allow()
with tellproof.llm_call("openai", "gpt-4.1-mini") as c:
    c.usage(10, 5)
assert tellproof.flush(5)`
	cmd := exec.Command(py, "-c", app)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join("..", "..", "sdk", "python", "src"))
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sample app: %v %s", err, b)
	}
	logDir := filepath.Join(dataDir, "local")
	for i := 0; ; i++ {
		if m, _ := filepath.Glob(filepath.Join(logDir, "*.batch.json")); len(m) > 0 {
			break
		}
		if i == 100 {
			t.Fatalf("no local batch: %s", out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The dashboard serves the verified log while the agent runs.
	resp, err := http.Get("http://" + dash + "/api/log")
	if err != nil {
		t.Fatal(err)
	}
	var api struct {
		Log *localreport.Summary `json:"log"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&api)
	resp.Body.Close()
	if api.Log == nil || !api.Log.Verified || api.Log.LLMCalls != 2 || len(api.Log.Series) != 1 {
		t.Fatalf("dashboard API: %+v", api.Log)
	}
	if page, err := http.Get("http://" + dash + "/"); err != nil || page.StatusCode != 200 {
		t.Fatalf("dashboard page: %v %v", page, err)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("local exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "Dashboard: http://"+dash+"/") {
		t.Errorf("no dashboard URL in the banner: %s", out.String())
	}
	// The old spelling, `tellproof-agent local`, still starts local mode.
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan int, 1)
	out2 := &lockedBuffer{}
	go func() { done2 <- run(ctx2, []string{"local", "--config", cfg}, getenv, out2, out2) }()
	for i := 0; i < 100 && !strings.Contains(out2.String(), "local mode"); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	cancel2()
	if code := <-done2; code != 0 || !strings.Contains(out2.String(), "local mode as agt_local") {
		t.Errorf("local alias: %d %s", code, out2.String())
	}
	if contacted.Load() != 0 {
		t.Fatal("local mode contacted the server")
	}
	if !strings.Contains(out.String(), "local mode as agt_local") || !strings.Contains(out.String(), "never sent") {
		t.Errorf("start banner: %s", out.String())
	}
	for name, mode := range map[string]os.FileMode{"agent.key": 0o600, "agent.pub": 0o644, "salt.secret": 0o600} {
		if info, err := os.Stat(filepath.Join(dataDir, name)); err != nil || info.Mode().Perm() != mode {
			t.Errorf("%s: %v %v", name, info, err)
		}
	}

	// The report: verified, with the calls, the guardrail and the model vendor's own model.
	rep := &lockedBuffer{}
	if code := run(context.Background(), []string{"report", "--data-dir", dataDir}, getenv, rep, rep); code != 0 {
		t.Fatalf("report exit %d: %s", code, rep.String())
	}
	for _, want := range []string{"Verified: 1 batches", "acme-ai", "acme-7b", "ap-southeast-2", "gpt-4.1-mini",
		"1 of 2 model calls had a guardrail decision (50.0%)", "acme-7b @ prompt 3", localreport.EarlyAccess} {
		if !strings.Contains(rep.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, rep.String())
		}
	}
	js := &lockedBuffer{}
	if code := run(context.Background(), []string{"report", "--json", "--data-dir", dataDir}, getenv, js, js); code != 0 {
		t.Fatalf("report --json exit %d", code)
	}
	var s localreport.Summary
	if err := json.Unmarshal([]byte(js.String()), &s); err != nil || !s.Verified || s.LLMCalls != 2 || s.Traces < 1 {
		t.Fatalf("report --json: %v %+v", err, s)
	}
	// A date range with no windows still verifies the whole log.
	empty := &lockedBuffer{}
	if code := run(context.Background(), []string{"report", "--since", "2001-01-01", "--until", "2001-01-31", "--data-dir", dataDir}, getenv, empty, empty); code != 0 ||
		!strings.Contains(empty.String(), "No windows in range") {
		t.Errorf("empty range: %d %s", code, empty.String())
	}

	// connect refuses a local-mode data directory instead of sending its batches with a local identity.
	conn := &lockedBuffer{}
	if code := run(context.Background(), []string{"connect", "--config", cfg, "a-token"}, getenv, conn, conn); code == 0 ||
		!strings.Contains(conn.String(), "local-mode agent") {
		t.Errorf("connect on a local data directory: %d %s", code, conn.String())
	}

	// An edited batch fails, and so does a missing one.
	files, _ := filepath.Glob(filepath.Join(logDir, "*.batch.json"))
	orig, _ := os.ReadFile(files[0])
	var e struct {
		State    json.RawMessage `json:"state"`
		Envelope struct {
			PayloadType string            `json:"payloadType"`
			Payload     string            `json:"payload"`
			Signatures  []json.RawMessage `json:"signatures"`
		} `json:"envelope"`
	}
	_ = json.Unmarshal(orig, &e)
	other, _ := json.Marshal(map[string]any{"state": e.State, "envelope": map[string]any{
		"payloadType": e.Envelope.PayloadType, "payload": "e30=", "signatures": e.Envelope.Signatures}})
	_ = os.WriteFile(files[0], other, 0o600)
	bad := &lockedBuffer{}
	if code := run(context.Background(), []string{"report", "--data-dir", dataDir}, getenv, bad, bad); code != 1 || !strings.Contains(bad.String(), "FAILED: batch 1") {
		t.Errorf("edited batch: %d %s", code, bad.String())
	}
	_ = os.WriteFile(files[0], orig, 0o600)
	_ = os.Remove(files[0])
	gone := &lockedBuffer{}
	if code := run(context.Background(), []string{"report", "--data-dir", dataDir}, getenv, gone, gone); code != 1 || !strings.Contains(gone.String(), "batch 1 is missing") {
		t.Errorf("missing batch: %d %s", code, gone.String())
	}
	// Not a local-mode directory at all.
	none := &lockedBuffer{}
	if code := run(context.Background(), []string{"report", "--data-dir", t.TempDir()}, getenv, none, none); code != 2 {
		t.Errorf("report on an empty directory: %d %s", code, none.String())
	}
}
