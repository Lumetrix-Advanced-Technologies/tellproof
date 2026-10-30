package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/batchsign"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/dsse"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/enrol"
)

var sample = filepath.Join("..", "..", "concierge", "testdata", "otlp-sample.jsonl")

func env(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestExampleConfigLoads(t *testing.T) {
	c, err := LoadConfig(filepath.Join("..", "..", "agent.example.json"), env())
	if err != nil {
		t.Fatalf("agent.example.json: %v", err)
	}
	if c.OTLPListen != "127.0.0.1:4318" || c.Aggregation.GraceMinutes != 5 || c.SaltSecretEnv != "TELLPROOF_SALT_SECRET" {
		t.Errorf("config = %+v", c)
	}
	c, err = LoadConfig(filepath.Join("..", "..", "agent.example.json"), env("TELLPROOF_SERVER", "https://override.example", "TELLPROOF_DATA_DIR", "/tmp/x"))
	if err != nil || c.Server != "https://override.example" || c.DataDir != "/tmp/x" {
		t.Errorf("env overrides: %+v %v", c, err)
	}
}

func TestConfigRefusals(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "c.json")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, c := range map[string]struct{ json, want string }{
		"unknown field":       {`{"server":"https://x","admin_key":"sk-oops"}`, "unknown field"},
		"plain http server":   {`{"server":"http://tellproof.example"}`, "https"},
		"listen on all hosts": {`{"otlp_listen":"0.0.0.0:4318"}`, "loopback"},
		"grace out of range":  {`{"aggregation":{"grace_minutes":90}}`, "grace_minutes"},
		"trailing data":       {`{} {}`, "unexpected data"},
		"plain http langfuse": {`{"langfuse":{"enabled":true,"base_url":"http://langfuse.example"}}`, "https"},
		"langfuse not a URL":  {`{"langfuse":{"enabled":true,"base_url":"langfuse.example"}}`, "not a URL"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(write(c.json), env()); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v; want %q", err, c.want)
			}
		})
	}
	if _, err := LoadConfig(write(`{"otlp_listen":"0.0.0.0:4318","otlp_allow_remote":true}`), env()); err != nil {
		t.Errorf("remote listen with otlp_allow_remote: %v", err)
	}
	if _, err := LoadConfig(write(`{"langfuse":{"enabled":true,"base_url":"http://127.0.0.1:3000"}}`), env()); err != nil {
		t.Errorf("http langfuse on localhost: %v", err)
	}
	if _, err := LoadConfig(write(`{"server":"http://localhost:8080"}`), env()); err != nil {
		t.Errorf("http to localhost: %v", err)
	}
}

func TestEnabledSourcesNeedTheirKeys(t *testing.T) {
	c, _ := LoadConfig("", env())
	c.Usage.OpenAI.Enabled, c.Usage.OpenAI.AdminKeyEnv = true, "OPENAI_ADMIN_KEY"
	_, err := New(c, Options{DryRun: true, Log: quiet(), Getenv: env("TELLPROOF_SALT_SECRET", "a-salt-secret-0123456")})
	if err == nil || !strings.Contains(err.Error(), "OPENAI_ADMIN_KEY is not set") {
		t.Errorf("err = %v", err)
	}
	c, _ = LoadConfig("", env())
	c.Usage.Bedrock.Enabled = true
	if _, err := New(c, Options{DryRun: true, Log: quiet(), Getenv: env("TELLPROOF_SALT_SECRET", "a-salt-secret-0123456")}); err == nil || !strings.Contains(err.Error(), "-tags bedrock") {
		t.Errorf("bedrock in a default build: %v", err)
	}
	c, _ = LoadConfig("", env())
	if _, err := New(c, Options{DryRun: true, Log: quiet(), Getenv: env()}); err == nil || !strings.Contains(err.Error(), "salt secret") {
		t.Errorf("no salt secret: %v", err)
	}
}

// Acceptance: --dry-run prints exactly what would be sent and sends nothing.
func TestDryRunPrintsAndSendsNothing(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()
	c, _ := LoadConfig("", env())
	c.Server = srv.URL
	c.DataDir = filepath.Join(t.TempDir(), "never-created")
	var out bytes.Buffer
	a, err := New(c, Options{DryRun: true, Out: &out, Log: quiet(), Getenv: env("TELLPROOF_SALT_SECRET", "a-salt-secret-0123456")})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(sample)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := a.Replay(f); err != nil {
		t.Fatal(err)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Deliver(context.Background()); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&out)
	var seqs []int64
	for dec.More() {
		var item struct {
			Envelope json.RawMessage `json:"envelope"`
			Payload  batch.Batch     `json:"payload"`
		}
		if err := dec.Decode(&item); err != nil {
			t.Fatalf("dry-run output is not JSON objects: %v", err)
		}
		e, err := dsse.Decode(item.Envelope)
		if err != nil {
			t.Fatal(err)
		}
		var inEnvelope batch.Batch
		_ = json.Unmarshal(e.Payload, &inEnvelope)
		if inEnvelope.Seq != item.Payload.Seq || inEnvelope.Commit != item.Payload.Commit {
			t.Error("the decoded payload differs from the envelope's")
		}
		seqs = append(seqs, item.Payload.Seq)
		if item.Payload.AgentID != DryRunAgentID {
			t.Errorf("agent_id = %s", item.Payload.AgentID)
		}
	}
	if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Errorf("printed seqs %v", seqs)
	}
	text := out.String()
	for _, needle := range []string{"ZEBRA", "Jane Citizen", "c0ffee"} {
		if strings.Contains(text, needle) {
			t.Errorf("dry-run output contains %q", needle)
		}
	}
	if hits != 0 {
		t.Errorf("dry run made %d requests", hits)
	}
	if _, err := os.Stat(c.DataDir); !os.IsNotExist(err) {
		t.Error("dry run created the data directory")
	}
}

// ingest stores verified batches like P4-2 will.
type ingest struct {
	mu     sync.Mutex
	pub    ed25519.PublicKey
	stored []*batch.Batch
}

func (s *ingest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/agents/enrol":
		var req struct {
			PublicKey string `json:"public_key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		k, _ := dsse.ParsePublicKeyPEM([]byte(req.PublicKey))
		s.pub = k.(ed25519.PublicKey)
		_ = json.NewEncoder(w).Encode(map[string]string{"agent_id": "agt_endtoend01"})
	case "/v1/batches":
		b, _ := io.ReadAll(r.Body)
		e, err := dsse.Decode(b)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		bt, err := batchsign.Verify(e, s.pub, "agt_endtoend01")
		if err != nil {
			w.WriteHeader(401)
			return
		}
		s.mu.Lock()
		s.stored = append(s.stored, bt)
		s.mu.Unlock()
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]string{"digest": batchsign.Digest(e)})
	default:
		http.NotFound(w, r)
	}
}

// rewrite sends requests for the OpenAI API to a test server; everything else
// (the Tellproof server) goes where it was addressed.
type rewrite struct{ to *url.URL }

func (rw rewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "api.openai.com" {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = rw.to.Scheme, rw.to.Host
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestEndToEndReceiveTickDeliver(t *testing.T) {
	ing := &ingest{}
	srv := httptest.NewServer(ing)
	defer srv.Close()
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/completions") {
			_, _ = w.Write([]byte(`{"data":[],"has_more":false,"next_page":null}`))
			return
		}
		start := r.URL.Query().Get("start_time")
		_, _ = w.Write([]byte(`{"data":[{"start_time":` + start + `,"results":[{"input_tokens":3700,"output_tokens":170,"num_model_requests":40,"model":"gpt-4.1-mini"}]}],"has_more":false,"next_page":null}`))
	}))
	defer openai.Close()
	ou, _ := url.Parse(openai.URL)

	dir := filepath.Join(t.TempDir(), "agent")
	if _, err := enrol.Enrol(context.Background(), enrol.Options{Server: srv.URL, Token: "one-time", DataDir: dir, HTTP: srv.Client()}); err != nil {
		t.Fatal(err)
	}
	c, _ := LoadConfig("", env())
	c.Server, c.DataDir = srv.URL, dir
	c.Usage.OpenAI.Enabled, c.Usage.OpenAI.AdminKeyEnv = true, "OPENAI_ADMIN_KEY"
	clock := time.Date(2026, 9, 1, 1, 30, 0, 0, time.UTC)
	a, err := New(c, Options{Log: quiet(), Now: func() time.Time { return clock }, HTTP: &http.Client{Transport: rewrite{ou}},
		Getenv: env("TELLPROOF_SALT_SECRET", "a-salt-secret-0123456", "OPENAI_ADMIN_KEY", "sk-admin-test")})
	if err != nil {
		t.Fatal(err)
	}
	// The app's collector posts its spans to the receiver.
	b, _ := os.ReadFile(sample)
	recv := a.Receiver()
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(line))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		recv.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("receiver: %d %s", rec.Code, rec.Body)
		}
	}
	if err := a.Tick(context.Background()); err != nil { // 01:30: the 00:00 window closed at 01:05
		t.Fatal(err)
	}
	clock = time.Date(2026, 9, 1, 2, 6, 0, 0, time.UTC)
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Deliver(context.Background()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(ing.stored) != 2 {
		t.Fatalf("server stored %d batches", len(ing.stored))
	}
	for i, bt := range ing.stored {
		if bt.Seq != int64(i+1) || len(bt.Usage) != 1 || bt.Usage[0].Provider != "openai" || bt.Usage[0].InputTokens != 3700 {
			t.Errorf("batch %d: seq %d usage %+v", i, bt.Seq, bt.Usage)
		}
		leaves, err := os.ReadFile(filepath.Join(dir, "leaves", strings.Repeat("0", 19)+string(rune('1'+i))+".json"))
		var hexes []string
		if err != nil || json.Unmarshal(leaves, &hexes) != nil || int64(len(hexes)) != bt.Commit.Leaves {
			t.Errorf("batch %d: leaves file (%v) has %d leaves, commit says %d", i, err, len(hexes), bt.Commit.Leaves)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "leaves")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("leaves dir: %v %v", info, err)
	}
}

// A new agent's first spans reach the server within first_batch_seconds, not
// after the hour; the rest of the hour follows as a second batch for the same
// window (windows are additive, D-032).
func TestEarlyFirstBatch(t *testing.T) {
	ing := &ingest{}
	srv := httptest.NewServer(ing)
	defer srv.Close()
	dir := filepath.Join(t.TempDir(), "agent")
	if _, err := enrol.Enrol(context.Background(), enrol.Options{Server: srv.URL, Token: "t", DataDir: dir, HTTP: srv.Client()}); err != nil {
		t.Fatal(err)
	}
	c, _ := LoadConfig("", env())
	c.Server, c.DataDir = srv.URL, dir
	clock := time.Date(2026, 9, 1, 10, 5, 0, 0, time.UTC)
	a, err := New(c, Options{Log: quiet(), Now: func() time.Time { return clock }, HTTP: srv.Client(), Getenv: env("TELLPROOF_SALT_SECRET", "a-salt-secret-0123456")})
	if err != nil {
		t.Fatal(err)
	}
	post := func(traceID string, at time.Time) {
		body := fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[{"spans":[{"traceId":"%s","spanId":"0000000000000001","startTimeUnixNano":"%d",
			"attributes":[{"key":"gen_ai.operation.name","value":{"stringValue":"chat"}},{"key":"gen_ai.provider.name","value":{"stringValue":"openai"}},
			{"key":"gen_ai.request.model","value":{"stringValue":"gpt-4.1-mini"}}]}]}]}]}`, traceID, at.UnixNano())
		req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		a.Receiver().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("receiver: %d %s", rec.Code, rec.Body)
		}
	}
	post(strings.Repeat("a", 32), clock)
	tick := func(at time.Time) {
		clock = at
		if err := a.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := a.Deliver(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	tick(clock.Add(30 * time.Second))
	if len(ing.stored) != 0 {
		t.Fatalf("sent before first_batch_seconds: %d", len(ing.stored))
	}
	tick(time.Date(2026, 9, 1, 10, 6, 0, 0, time.UTC))
	if len(ing.stored) != 1 || ing.stored[0].Window.Start != "2026-09-01T10:00:00Z" || ing.stored[0].Metrics.Guardrails.LLMCalls != 1 {
		t.Fatalf("early batch: %d stored", len(ing.stored))
	}
	post(strings.Repeat("b", 32), time.Date(2026, 9, 1, 10, 20, 0, 0, time.UTC))
	tick(time.Date(2026, 9, 1, 10, 30, 0, 0, time.UTC)) // only once per start
	tick(time.Date(2026, 9, 1, 11, 6, 0, 0, time.UTC))
	if len(ing.stored) != 2 || ing.stored[1].Window.Start != "2026-09-01T10:00:00Z" || ing.stored[1].Metrics.LateSpans != 0 || ing.stored[1].Commit.Leaves != 1 {
		t.Errorf("rest of the hour: %d stored", len(ing.stored))
	}
}

// An agent set up before the rename keeps its data directory (D-056).
func TestDefaultDataDirKeepsTheLegacyDirectory(t *testing.T) {
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	_ = os.Chdir(t.TempDir())
	if got := DefaultDataDir(); got != "./.tellproof-agent" {
		t.Errorf("fresh: %s", got)
	}
	_ = os.Mkdir(".plumbline-agent", 0o700)
	if got := DefaultDataDir(); got != "./.plumbline-agent" {
		t.Errorf("legacy only: %s", got)
	}
	_ = os.Mkdir(".tellproof-agent", 0o700)
	if got := DefaultDataDir(); got != "./.tellproof-agent" {
		t.Errorf("both: %s", got)
	}
}

// A receiver on loopback answers only under local host names, so a web page
// that rebinds its domain to 127.0.0.1 cannot post spans into the evidence;
// with otlp_allow_remote (containers, sidecars) any Host is accepted.
func TestReceiverRefusesNonLocalHostsOnLoopback(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := LocalHostOnly(ok)
	for host, want := range map[string]int{
		"127.0.0.1:4318": 204, "localhost:4318": 204, "LOCALHOST": 204, "app.localhost:4318": 204, "[::1]:4318": 204, "127.0.1.1:4318": 204,
		"evil.example:4318": 403, "attacker.com": 403, "192.168.1.5:4318": 403, "localhost.evil.example": 403, "": 403,
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader("{}"))
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q: %d, want %d", host, rec.Code, want)
		}
	}

	// Run wires it in, and leaves it out with otlp_allow_remote.
	for _, remote := range []bool{false, true} {
		c, _ := LoadConfig("", env())
		c.DataDir, c.OTLPAllowRemote = t.TempDir(), remote
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		c.OTLPListen = ln.Addr().String()
		ln.Close()
		a, err := New(c, Options{Local: true, Log: quiet(), Getenv: env("TELLPROOF_SALT_SECRET", "a-salt-secret-0123456")})
		if err != nil {
			t.Fatal(err)
		}
		listening := make(chan struct{})
		a.OnListening(func() { close(listening) })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		<-listening
		req, _ := http.NewRequest(http.MethodPost, "http://"+c.OTLPListen+"/v1/traces", strings.NewReader(`{"resourceSpans":[]}`))
		req.Host = "rebound.example:4318"
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if want := map[bool]int{false: 403, true: 200}[remote]; resp.StatusCode != want {
			t.Errorf("allow_remote=%v: a foreign Host got %d, want %d", remote, resp.StatusCode, want)
		}
		cancel()
		<-done
	}
}

// Acceptance (D-074): a model and prompt swapped while the agent was stopped
// (a deploy restarts both) is a change in the first batch after the restart,
// from the history the agent keeps in its data directory.
func TestChangeAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	span := func(trace, model, version string) string {
		start := time.Now().Add(-time.Minute).UnixNano()
		return fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claims"}}]},"scopeSpans":[{"spans":[`+
			`{"traceId":"%032s","spanId":"00000000000000a1","name":"chat","startTimeUnixNano":"%d","endTimeUnixNano":"%d","attributes":[`+
			`{"key":"gen_ai.operation.name","value":{"stringValue":"chat"}},{"key":"gen_ai.request.model","value":{"stringValue":"%s"}},`+
			`{"key":"tellproof.prompt.name","value":{"stringValue":"triage"}},{"key":"tellproof.prompt.version","value":{"stringValue":"%s"}}]}]}]}]}`,
			trace, start, start+1000, model, version)
	}
	run := func(trace, model, version string) {
		c, _ := LoadConfig("", env())
		c.DataDir = dir
		a, err := New(c, Options{Local: true, Log: quiet(), Getenv: env("TELLPROOF_SALT_SECRET", "a-salt-secret-0123456")})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Replay(strings.NewReader(span(trace, model, version))); err != nil {
			t.Fatal(err)
		}
		if err := a.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	run("1", "gpt-4.1-mini", "14")
	if _, err := os.Stat(filepath.Join(dir, HistoryFile)); err != nil {
		t.Fatalf("no history saved: %v", err)
	}
	run("2", "gpt-4.1", "15") // after the "restart"
	files, _ := filepath.Glob(filepath.Join(dir, LocalDir, "*.batch.json"))
	if len(files) != 2 {
		t.Fatalf("%d batches", len(files))
	}
	raw, _ := os.ReadFile(files[1])
	var doc struct {
		Envelope struct {
			Payload []byte `json:"payload"`
		} `json:"envelope"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var p struct {
		Metrics struct {
			Transitions []struct{ Kind, From, To string } `json:"transitions"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(doc.Envelope.Payload, &p); err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(p.Metrics.Transitions)
	if want := "[{model gpt-4.1-mini gpt-4.1} {prompt 14 15}]"; got != want {
		t.Fatalf("transitions after restart = %s, want %s", got, want)
	}
}
