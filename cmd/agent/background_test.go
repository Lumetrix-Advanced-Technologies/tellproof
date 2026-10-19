package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for the agent: `start` runs
// os.Executable() with `run --local`, and with this variable set that child
// runs main instead of the tests.
func TestMain(m *testing.M) {
	if os.Getenv("TELLPROOF_TEST_AGENT_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// localSetup is a config on free ports and a data directory, and the
// environment that points the commands at them.
func localSetup(t *testing.T) (cfg, dataDir string, getenv func(string) string) {
	t.Helper()
	dataDir = filepath.Join(t.TempDir(), "data")
	cfg = filepath.Join(t.TempDir(), "agent.json")
	_ = os.WriteFile(cfg, []byte(`{"otlp_listen": "`+freePort(t)+`", "dashboard_listen": "`+freePort(t)+`"}`), 0o600)
	env := map[string]string{"TELLPROOF_DATA_DIR": dataDir}
	return cfg, dataDir, func(k string) string { return env[k] }
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Acceptance (D-067): start returns with the agent listening in the
// background, says where everything is, and is idempotent; status reports it
// (exit 0) and a second agent on the same data directory is refused; stop
// ends it, keeps the evidence and is idempotent; status then exits 3; logs
// shows the background agent's log.
func TestStartStatusStopInTheBackground(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a process")
	}
	t.Setenv("TELLPROOF_TEST_AGENT_MAIN", "1")
	cfg, dataDir, getenv := localSetup(t)
	ctx := context.Background()
	cmd := func(args ...string) (int, string) {
		out := &lockedBuffer{}
		code := run(ctx, append(args, "--config", cfg), getenv, out, out)
		return code, out.String()
	}
	code, out := cmd("start")
	if code != 0 || !strings.Contains(out, "running in the background") || !strings.Contains(out, "Dashboard: http://") || !strings.Contains(out, "tellproof-agent stop") {
		t.Fatalf("start: exit %d\n%s", code, out)
	}
	st, err := readRunState(dataDir)
	if err != nil || st == nil || !st.Background || !processAlive(st.PID) || st.PID == os.Getpid() {
		t.Fatalf("run record %+v, %v", st, err)
	}
	t.Cleanup(func() {
		if processAlive(st.PID) {
			_ = killProcess(st.PID)
		}
	})
	if !probeReceiver(ctx, st.OTLP, func(string, string, string, string) {}) {
		t.Fatal("the agent does not answer on its receiver after start returned")
	}

	if code, out := cmd("start"); code != 0 || !strings.Contains(out, "already running") {
		t.Errorf("second start: exit %d\n%s", code, out)
	}
	if code, out := cmd("status"); code != 0 || !strings.Contains(out, "Running in the background") {
		t.Errorf("status: exit %d\n%s", code, out)
	}
	code, out = cmd("status", "--json")
	var js struct {
		Running bool      `json:"running"`
		Agent   *runState `json:"agent"`
	}
	if err := json.Unmarshal([]byte(out), &js); code != 0 || err != nil || !js.Running || js.Agent.PID != st.PID {
		t.Errorf("status --json: exit %d, %v\n%s", code, err, out)
	}
	if code, out := cmd("run", "--local"); code != 1 || !strings.Contains(out, "already runs") {
		t.Errorf("a second agent on the same data directory: exit %d\n%s", code, out)
	}

	code, out = cmd("stop")
	if code != 0 || !strings.Contains(out, "Stopped the Tellproof agent") {
		t.Fatalf("stop: exit %d\n%s", code, out)
	}
	waitUntil(t, "the process to exit", func() bool { return !processAlive(st.PID) })
	if _, err := os.Stat(filepath.Join(dataDir, runFile)); !os.IsNotExist(err) {
		t.Errorf("run record left behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "salt.secret")); err != nil {
		t.Errorf("the data directory lost its salt secret: %v", err)
	}
	if code, out := cmd("status"); code != 3 || !strings.Contains(out, "Not running") {
		t.Errorf("status after stop: exit %d\n%s", code, out)
	}
	if code, out := cmd("stop"); code != 0 || !strings.Contains(out, "No Tellproof agent is running") {
		t.Errorf("second stop: exit %d\n%s", code, out)
	}
	if code, out := cmd("logs"); code != 0 || !strings.Contains(out, "tellproof-agent start ---") || !strings.Contains(out, "agent started") {
		t.Errorf("logs: exit %d\n%s", code, out)
	}
}

// stop also ends an agent run in the foreground, from another terminal.
func TestStopEndsAForegroundAgent(t *testing.T) {
	cfg, dataDir, getenv := localSetup(t)
	done := make(chan int, 1)
	fg := &lockedBuffer{}
	go func() { done <- run(context.Background(), []string{"run", "--local", "--config", cfg}, getenv, fg, fg) }()
	waitUntil(t, "the run record", func() bool { st, _ := readRunState(dataDir); return st != nil })
	if st, _ := readRunState(dataDir); st.Background {
		t.Error("a foreground agent recorded as background")
	}
	out := &lockedBuffer{}
	if code := run(context.Background(), []string{"stop", "--config", cfg}, getenv, out, out); code != 0 {
		t.Fatalf("stop: exit %d\n%s", code, out)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("the agent exited %d:\n%s", code, fg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the foreground agent did not stop")
	}
	if _, err := os.Stat(filepath.Join(dataDir, stopFile)); !os.IsNotExist(err) {
		t.Errorf("stop request left behind: %v", err)
	}
}

// A record left by an agent that is gone (a crash, a reboot) does not block
// anything: status clears it and start runs.
func TestStaleRunRecordIsCleared(t *testing.T) {
	cfg, dataDir, getenv := localSetup(t)
	dead := exec.Command(os.Args[0], "-test.run=^$")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(dataDir, 0o700)
	if err := writeRunState(dataDir, runState{PID: dead.Process.Pid, OTLP: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	out := &lockedBuffer{}
	if code := run(context.Background(), []string{"status", "--config", cfg}, getenv, out, out); code != 3 {
		t.Errorf("status: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, runFile)); !os.IsNotExist(err) {
		t.Errorf("stale record kept: %v", err)
	}
}

// An agent that does not stop is reported, and ended only with --force.
func TestStopForce(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep command")
	}
	old := stopWait
	stopWait = 300 * time.Millisecond
	defer func() { stopWait = old }()
	cfg, dataDir, getenv := localSetup(t)
	p := exec.Command(sleep, "30")
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = p.Wait(); close(exited) }()
	defer func() { _ = p.Process.Kill() }()
	_ = os.MkdirAll(dataDir, 0o700)
	_ = writeRunState(dataDir, runState{PID: p.Process.Pid, Evidence: "x"})
	out := &lockedBuffer{}
	if code := run(context.Background(), []string{"stop", "--config", cfg}, getenv, out, out); code != 1 || !strings.Contains(out.String(), "stop --force") {
		t.Fatalf("stop: exit %d\n%s", code, out)
	}
	select {
	case <-exited:
		t.Fatal("stop without --force ended the process")
	default:
	}
	if _, err := os.Stat(filepath.Join(dataDir, stopFile)); !os.IsNotExist(err) {
		t.Error("a stop request that timed out was left for the next run")
	}
	out = &lockedBuffer{}
	if code := run(context.Background(), []string{"stop", "--force", "--config", cfg}, getenv, out, out); code != 0 || !strings.Contains(out.String(), "Ended") {
		t.Fatalf("stop --force: exit %d\n%s", code, out)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("stop --force did not end the process")
	}
}

// When the agent cannot start, start says why, from its log.
func TestStartReportsAFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a process")
	}
	t.Setenv("TELLPROOF_TEST_AGENT_MAIN", "1")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := filepath.Join(t.TempDir(), "agent.json")
	_ = os.WriteFile(cfg, []byte(`{"otlp_listen": "`+ln.Addr().String()+`", "dashboard_listen": "off"}`), 0o600)
	getenv := func(k string) string { return map[string]string{"TELLPROOF_DATA_DIR": dataDir}[k] }
	out := &lockedBuffer{}
	if code := run(context.Background(), []string{"start", "--config", cfg}, getenv, out, out); code != 1 ||
		!strings.Contains(out.String(), "stopped while starting") || !strings.Contains(out.String(), ln.Addr().String()) {
		t.Fatalf("start on a taken port: exit %d\n%s", code, out)
	}
}

func TestLifecycleFlags(t *testing.T) {
	for _, args := range [][]string{{"stop", "--open"}, {"status", "--force"}, {"logs", "--json"}, {"start", "extra"}} {
		out := &lockedBuffer{}
		if code := run(context.Background(), args, func(string) string { return "" }, out, out); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}
