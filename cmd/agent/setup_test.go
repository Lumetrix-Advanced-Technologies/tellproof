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
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/demolog"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/enrol"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/policy"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func noTerminal(t *testing.T) {
	t.Helper()
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = old })
}

func TestDetectFindsTheProjectAndWhereInitGoes(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  project
	}{
		{"python by pyproject", map[string]string{"pyproject.toml": "", "main.py": "", "util.py": ""},
			project{python: true, pythonVia: "pyproject.toml", pyEntry: "main.py"}},
		{"the python sample", map[string]string{"app.py": ""}, project{python: true, pythonVia: "app.py", pyEntry: "app.py"}},
		{"node start script", map[string]string{"package.json": `{"scripts": {"start": "node --env-file=.env src/server.js"}}`},
			project{node: true, nodeVia: "package.json", nodeEntry: "src/server.js"}},
		{"node esm main", map[string]string{"package.json": `{"type": "module", "main": "index.js"}`, "index.js": ""},
			project{node: true, nodeVia: "package.json", nodeEntry: "index.js", nodeESM: true}},
		{"the node sample", map[string]string{"app.mjs": ""}, project{node: true, nodeVia: "app.mjs", nodeEntry: "app.mjs", nodeESM: true}},
		{"config files are not apps", map[string]string{"vite.config.js": ""}, project{}},
		{"both", map[string]string{"requirements.txt": "", "package.json": "{}", "worker.py": "", "server.ts": ""},
			project{python: true, pythonVia: "requirements.txt", pyEntry: "worker.py", node: true, nodeVia: "package.json", nodeEntry: "server.ts"}},
	}
	for _, c := range cases {
		dir := t.TempDir()
		for f, body := range c.files {
			_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755)
			if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got := detect(dir); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// Acceptance (P8-2): in the sample apps' layout, init shows the SDK line and
// where it goes, writes a valid policy, keeps the key out of git, changes no
// source file and, without a terminal, does not start (or block on) anything.
func TestInitSetsUpAPythonProject(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	noTerminal(t)
	dir := t.TempDir()
	t.Chdir(dir)
	app := []byte("print('hello')\n")
	_ = os.WriteFile("app.py", app, 0o644)
	if b, err := exec.Command("git", "init", "-q", ".").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, b)
	}
	out := &lockedBuffer{}
	if code := run(context.Background(), []string{"init", "--yes", "--name", "Claims API", "--timezone", "Australia/Sydney"}, envOf(nil), out, out); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"Python project (app.py)", `pip install "tellproof[agent]"`, "At the top of app.py", `tellproof.init(service="claims-api")`,
		"Added /.tellproof-agent/ to .gitignore", "Next: tellproof-agent start"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	p, err := policy.Load("tellproof.policy.json")
	if err != nil || p.AISystem != "Claims API" || p.Timezone != "Australia/Sydney" {
		t.Fatalf("policy %+v, %v", p, err)
	}
	if b, _ := os.ReadFile(".gitignore"); string(b) != "/.tellproof-agent/\n" {
		t.Errorf(".gitignore = %q", b)
	}
	if b, _ := os.ReadFile("app.py"); string(b) != string(app) {
		t.Error("init changed the app's source")
	}
	if _, err := os.Stat(".tellproof-agent"); err == nil {
		t.Error("init started the agent without a terminal")
	}

	// A second run keeps the policy and does not add the entry twice.
	_ = os.WriteFile("tellproof.policy.json", []byte(`{"ai_system": "Edited", "environment": "production", "timezone": "UTC", "declared_models": [], "declared_regions": [], "changes": [], "incidents": []}`), 0o644)
	out = &lockedBuffer{}
	if code := run(context.Background(), []string{"init", "--yes"}, envOf(nil), out, out); code != 0 || !strings.Contains(out.String(), "already exists; keeping it") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if p, _ := policy.Load("tellproof.policy.json"); p == nil || p.AISystem != "Edited" {
		t.Error("init replaced an existing policy without --force")
	}
	if b, _ := os.ReadFile(".gitignore"); strings.Count(string(b), ".tellproof-agent") != 1 {
		t.Errorf(".gitignore = %q", b)
	}
}

func TestInitShowsTheRightNodeImport(t *testing.T) {
	noTerminal(t)
	for _, c := range []struct{ pkg, want, not string }{
		{`{"main": "server.js"}`, "const tellproof = require('tellproof');", "await tellproof.ready()"},
		{`{"type": "module", "main": "server.js"}`, "import * as tellproof from 'tellproof';", "require("},
	} {
		dir := t.TempDir()
		t.Chdir(dir)
		_ = os.WriteFile("package.json", []byte(c.pkg), 0o644)
		_ = os.WriteFile("server.js", nil, 0o644)
		out := &lockedBuffer{}
		if code := run(context.Background(), []string{"init", "--yes", "--name", "Referrals"}, envOf(nil), out, out); code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		for _, want := range []string{"npm install tellproof", "At the top of server.js", c.want, "tellproof.init({ service: 'referrals' });"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s: output lacks %q:\n%s", c.pkg, want, out)
			}
		}
		if strings.Contains(out.String(), c.not) || strings.Contains(out.String(), "pip install") {
			t.Errorf("%s: output has %q or the Python line:\n%s", c.pkg, c.not, out)
		}
	}
}

func TestInitAsksInATerminal(t *testing.T) {
	old, oldIn := stdinIsTerminal, stdin
	stdinIsTerminal = func() bool { return true }
	// name, time zone, start now? (no git repository here, so no .gitignore question)
	stdin = strings.NewReader("Fraud Screen\nEurope/Paris\nn\n")
	defer func() { stdinIsTerminal, stdin = old, oldIn }()
	t.Chdir(t.TempDir())
	_ = os.WriteFile("main.py", nil, 0o644)
	out := &lockedBuffer{}
	if code := run(context.Background(), []string{"init"}, envOf(nil), out, out); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"Name of the AI system [", "Its time zone (IANA, for daily figures) [UTC]: ", `service="fraud-screen"`, "Start the agent now, in the background (y/n) [y]: ", "Next: tellproof-agent start"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if p, err := policy.Load("tellproof.policy.json"); err != nil || p.AISystem != "Fraud Screen" || p.Timezone != "Europe/Paris" {
		t.Fatalf("policy %+v, %v", p, err)
	}
}

func TestSetupCommandsRefuseOtherCommandsFlags(t *testing.T) {
	for _, args := range [][]string{{"init", "--json"}, {"doctor", "--yes"}, {"demo", "--policy", "x"}, {"demo", "--days", "0"}, {"doctor", "extra"}} {
		out := &lockedBuffer{}
		if code := run(context.Background(), args, envOf(nil), out, out); code != 2 {
			t.Errorf("%v: exit %d, want 2: %s", args, code, out)
		}
	}
}

// demoDir writes a demo log ending at end and returns its data directory.
func demoDir(t *testing.T, end time.Time) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if _, err := demolog.Write(dir, 1, 7, end); err != nil {
		t.Fatal(err)
	}
	return dir
}

// doctorRun runs doctor --json and returns its exit status and checks by name.
func doctorRun(t *testing.T, env map[string]string, args ...string) (int, map[string]finding) {
	t.Helper()
	out := &lockedBuffer{}
	code := run(context.Background(), append([]string{"doctor", "--json"}, args...), envOf(env), out, out)
	var res struct {
		OK     bool      `json:"ok"`
		Checks []finding `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out.String()), &res); err != nil {
		t.Fatalf("doctor output: %v\n%s", err, out)
	}
	byName := map[string]finding{}
	for _, f := range res.Checks {
		byName[f.Check] = f
	}
	if res.OK != (code == 0) {
		t.Errorf("ok %v but exit %d", res.OK, code)
	}
	return code, byName
}

func expect(t *testing.T, fs map[string]finding, check, status, detail string) {
	t.Helper()
	f, ok := fs[check]
	if !ok || f.Status != status || !strings.Contains(f.Detail+" "+f.Fix, detail) {
		names := make([]string, 0, len(fs))
		for n, f := range fs {
			names = append(names, n+"="+f.Status+": "+f.Detail)
		}
		sort.Strings(names)
		t.Errorf("%s: want %s containing %q, got %+v\n%s", check, status, detail, f, strings.Join(names, "\n"))
	}
}

// Acceptance (P8-2): doctor diagnoses each agent-side failure in the
// troubleshooting table, with the fix.
func TestDoctorOnAFreshMachine(t *testing.T) {
	t.Chdir(t.TempDir())
	env := map[string]string{"TELLPROOF_OTLP_LISTEN": freePort(t), "TELLPROOF_DASHBOARD_LISTEN": freePort(t)}
	code, fs := doctorRun(t, env)
	if code != 0 {
		t.Errorf("exit %d", code)
	}
	expect(t, fs, "config", "ok", "defaults")
	expect(t, fs, "data directory", "info", "does not exist yet")
	expect(t, fs, "receiver", "warn", "tellproof-agent start")
	expect(t, fs, "SDK target", "warn", "TELLPROOF_AGENT_URL=http://"+env["TELLPROOF_OTLP_LISTEN"])
	expect(t, fs, "dashboard", "info", "not running")
	expect(t, fs, "evidence log", "info", "no batches yet")
	expect(t, fs, "policy", "info", "tellproof-agent policy init")
}

func TestDoctorWithTheAgentRunning(t *testing.T) {
	t.Chdir(t.TempDir())
	listen, dash := freePort(t), freePort(t)
	env := map[string]string{"TELLPROOF_OTLP_LISTEN": listen, "TELLPROOF_DASHBOARD_LISTEN": dash, "TELLPROOF_AGENT_URL": "http://localhost:" + strings.Split(listen, ":")[1]}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	out := &lockedBuffer{}
	go func() { done <- run(ctx, []string{"run", "--local"}, envOf(env), out, out) }()
	defer func() { cancel(); <-done }()
	waitFor(t, dash, out)

	code, fs := doctorRun(t, env)
	if code != 0 {
		t.Errorf("exit %d", code)
	}
	expect(t, fs, "receiver", "ok", "the agent answers")
	expect(t, fs, "SDK target", "ok", "is this agent's receiver")
	expect(t, fs, "dashboard", "ok", "http://"+dash+"/")
	expect(t, fs, "identity", "ok", "local agent agt_local")
	expect(t, fs, "salt secret", "ok", "salt.secret")
	expect(t, fs, "data directory", "ok", "private to this user")

	// The probe sent no spans: no batch has anything in it.
	if _, err := os.Stat(filepath.Join(".tellproof-agent", "local")); err == nil {
		if m, _ := filepath.Glob(filepath.Join(".tellproof-agent", "local", "*.batch.json")); len(m) > 0 {
			t.Errorf("doctor's probe made a batch: %v", m)
		}
	}

	// The SDK pointed at the wrong place.
	env["TELLPROOF_AGENT_URL"] = "http://" + listen + "/v1/traces"
	_, fs = doctorRun(t, env)
	expect(t, fs, "SDK target", "fail", "TELLPROOF_AGENT_URL=http://"+listen)
	env["TELLPROOF_AGENT_URL"] = "http://127.0.0.1:9"
	_, fs = doctorRun(t, env)
	expect(t, fs, "SDK target", "warn", "TELLPROOF_AGENT_URL=http://"+listen)
	env["TELLPROOF_AGENT_URL"] = "http://user:hunter2@127.0.0.1:9"
	_, fs = doctorRun(t, env)
	if f := fs["SDK target"]; strings.Contains(f.Detail+f.Fix, "hunter2") {
		t.Errorf("doctor printed the URL's password: %+v", f)
	}
	env["TELLPROOF_AGENT_URL"] = "ftp://user:hunter2@host"
	_, fs = doctorRun(t, env)
	if f := fs["SDK target"]; f.Status != "fail" || strings.Contains(f.Detail+f.Fix, "hunter2") {
		t.Errorf("not an http URL: %+v", f)
	}
}

func waitFor(t *testing.T, addr string, out *lockedBuffer) {
	t.Helper()
	for i := 0; ; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return
		}
		if i == 100 {
			t.Fatalf("nothing listens on %s: %s", addr, out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestDoctorFindsPortsTakenBySomethingElse(t *testing.T) {
	t.Chdir(t.TempDir())
	other := httptest.NewServer(http.NotFoundHandler()) // an HTTP server that is not the agent
	defer other.Close()
	raw, err := net.Listen("tcp", "127.0.0.1:0") // a port held by something that is not HTTP
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	go func() {
		for {
			c, err := raw.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	otherAddr := strings.TrimPrefix(other.URL, "http://")
	code, fs := doctorRun(t, map[string]string{"TELLPROOF_OTLP_LISTEN": otherAddr, "TELLPROOF_DASHBOARD_LISTEN": raw.Addr().String()})
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	expect(t, fs, "receiver", "fail", "something else answers")
	expect(t, fs, "dashboard", "fail", "TELLPROOF_DASHBOARD_LISTEN=127.0.0.1:4329")

	_, fs = doctorRun(t, map[string]string{"TELLPROOF_OTLP_LISTEN": raw.Addr().String(), "TELLPROOF_DASHBOARD_LISTEN": otherAddr})
	expect(t, fs, "receiver", "fail", "not by an HTTP server")
	expect(t, fs, "dashboard", "fail", "something else answers")
}

func TestDoctorChecksTheEvidenceLog(t *testing.T) {
	t.Chdir(t.TempDir())
	ports := map[string]string{"TELLPROOF_OTLP_LISTEN": freePort(t), "TELLPROOF_DASHBOARD_LISTEN": freePort(t)}
	env := func(dir string) map[string]string {
		m := map[string]string{"TELLPROOF_DATA_DIR": dir}
		for k, v := range ports {
			m[k] = v
		}
		return m
	}
	now := time.Date(2026, 10, 19, 12, 20, 0, 0, time.UTC)
	doctorNow = func() time.Time { return now }
	defer func() { doctorNow = time.Now }()

	fresh := demoDir(t, now)
	code, fs := doctorRun(t, env(fresh))
	expect(t, fs, "evidence log", "ok", "24 batches verify")
	if _, ok := fs["spans"]; ok || code != 0 {
		t.Errorf("a current log is fine: exit %d, %+v", code, fs["spans"])
	}

	stale := demoDir(t, now.Add(-5*time.Hour))
	_, fs = doctorRun(t, env(stale))
	expect(t, fs, "spans", "warn", "no batch for 5h")

	ahead := demoDir(t, now.Add(3*time.Hour))
	code, fs = doctorRun(t, env(ahead))
	expect(t, fs, "clock", "fail", "NTP")
	if code != 1 {
		t.Errorf("clock: exit %d", code)
	}

	broken := demoDir(t, now)
	files, _ := filepath.Glob(filepath.Join(broken, "local", "*.batch.json"))
	sort.Strings(files)
	_ = os.Remove(files[3])
	code, fs = doctorRun(t, env(broken))
	expect(t, fs, "evidence log", "fail", "restore the data directory")
	if code != 1 {
		t.Errorf("broken log: exit %d", code)
	}

	// A policy that declares nothing although the log has models, then a bad one.
	_ = os.WriteFile("tellproof.policy.json", []byte(`{"ai_system": "X", "environment": "production", "timezone": "UTC", "declared_models": [], "declared_regions": [], "changes": [], "incidents": []}`), 0o644)
	_, fs = doctorRun(t, env(fresh))
	expect(t, fs, "policy", "warn", "policy init --force")
	_ = os.WriteFile("tellproof.policy.json", []byte(`{"ai_system": "X", "timezone": "Mars/Olympus"}`), 0o644)
	code, fs = doctorRun(t, env(fresh))
	expect(t, fs, "policy", "fail", "policy validate")
	if code != 1 {
		t.Errorf("bad policy: exit %d", code)
	}
}

func TestDoctorChecksTheDataDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	ports := map[string]string{"TELLPROOF_OTLP_LISTEN": freePort(t), "TELLPROOF_DASHBOARD_LISTEN": freePort(t)}
	dir := filepath.Join(t.TempDir(), "data")
	if _, err := enrol.LocalIdentity(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSaltSecret(dir); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"TELLPROOF_DATA_DIR": dir}
	for k, v := range ports {
		env[k] = v
	}
	_, fs := doctorRun(t, env)
	expect(t, fs, "identity", "ok", "local agent")

	// A local-mode directory used for connected mode.
	env["TELLPROOF_SERVER"] = "https://tellproof.example"
	code, fs := doctorRun(t, env)
	expect(t, fs, "identity", "fail", "new data directory")
	if code != 1 {
		t.Errorf("exit %d", code)
	}
	delete(env, "TELLPROOF_SERVER")

	if runtime.GOOS == "windows" {
		return
	}
	_ = os.Chmod(dir, 0o755)
	_ = os.Chmod(filepath.Join(dir, enrol.KeyFile), 0o644)
	_, fs = doctorRun(t, env)
	expect(t, fs, "data directory", "warn", "chmod 700")
	expect(t, fs, enrol.KeyFile, "warn", "chmod 600")

	// A configuration error stops doctor with the message.
	env["TELLPROOF_OTLP_LISTEN"] = "0.0.0.0:4318"
	code, fs = doctorRun(t, env)
	expect(t, fs, "config", "fail", "")
	if code != 1 || len(fs) != 1 {
		t.Errorf("exit %d, %d checks", code, len(fs))
	}
}

func TestDemoServesASyntheticWeekWithItsPolicy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo")
	dash := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	out := &lockedBuffer{}
	go func() {
		done <- run(ctx, []string{"demo", "--data-dir", dir, "--days", "2", "--listen", dash}, envOf(nil), out, out)
	}()
	waitFor(t, dash, out)
	resp, err := http.Get("http://" + dash + "/api/check")
	if err != nil {
		t.Fatal(err)
	}
	var check struct {
		Policy string `json:"policy"`
		Report *struct {
			AISystem string         `json:"ai_system"`
			Summary  map[string]int `json:"summary"`
		} `json:"report"`
	}
	err = json.NewDecoder(resp.Body).Decode(&check)
	resp.Body.Close()
	if err != nil || check.Report == nil || check.Report.AISystem != "Claims assistant (demo)" || check.Report.Summary["fail"] == 0 || check.Report.Summary["pass"] == 0 {
		t.Fatalf("check: %+v %v", check, err)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit %d", code)
	}
	for _, want := range []string{"Synthetic data", "48 hourly batches", "check --self-test --data-dir " + dir} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// It never writes over a directory in use.
	out = &lockedBuffer{}
	if code := run(context.Background(), []string{"demo", "--data-dir", dir}, envOf(nil), out, out); code != 2 || !strings.Contains(out.String(), "not empty") {
		t.Errorf("exit %d: %s", code, out)
	}
}
