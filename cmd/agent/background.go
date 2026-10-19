package main

// The local agent in the background (D-067): `start` runs `run --local` as a
// detached process and returns once it listens; `stop` asks it to finish its
// work and exit; `status`, `restart` and `logs` complete the set. A local
// agent, in the foreground or the background, records itself in
// <data-dir>/run.json while it runs, so the commands find it from the
// directory it was started in. `stop` writes <data-dir>/agent.stop, which the
// agent watches: no signal, no network endpoint, and so no way to stop an
// unrelated process that has reused the agent's pid.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/agent"
)

const lifecycleUsage = `usage:
  tellproof-agent start [--config <agent.json>] [--data-dir <dir>] [--open]
  tellproof-agent stop [--config <agent.json>] [--data-dir <dir>] [--force]
  tellproof-agent restart [--config <agent.json>] [--data-dir <dir>] [--open]
  tellproof-agent status [--config <agent.json>] [--data-dir <dir>] [--json]
  tellproof-agent logs [--config <agent.json>] [--data-dir <dir>] [-n <lines>] [-f]

start runs the agent in local mode in the background, with its dashboard, and
returns once it listens; its log goes to <data-dir>/agent.log. stop lets it
finish the current work and exit; --force ends it if it has not after 15
seconds. status says whether it runs, where, and since when (exit 0 running,
3 not running). logs prints the background agent's log; -f follows it.
run --local is the same agent in the foreground: Ctrl-C, or stop from another
terminal, ends it.
`

const (
	runFile  = "run.json"   // a running local agent: runState
	stopFile = "agent.stop" // a request to stop, written by `stop`
	logFile  = "agent.log"  // a background agent's output
)

// stopWait is how long stop waits for the agent to exit; tests shorten it.
var stopWait = 15 * time.Second

// runState is what a running local agent records about itself. Identifiers,
// addresses and paths only.
type runState struct {
	PID        int       `json:"pid"`
	Started    time.Time `json:"started"`
	Version    string    `json:"version"`
	AgentID    string    `json:"agent_id"`
	OTLP       string    `json:"otlp"`
	Dashboard  string    `json:"dashboard,omitempty"`
	Evidence   string    `json:"evidence"`
	Background bool      `json:"background,omitempty"`
}

// progName is the command name to suggest: "npx tellproof" or "tellproof"
// when an SDK's launcher started us (npm install tellproof, pip install
// "tellproof[agent]"), else tellproof-agent. All run the same agent.
func progName() string {
	if os.Getenv("TELLPROOF_COMMAND") == "npx tellproof" { // set by the npm SDK's launcher
		return "npx tellproof"
	}
	n := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if n == "tellproof" {
		return n
	}
	return "tellproof-agent"
}

func isLifecycle(args []string) bool {
	return len(args) > 0 && contains([]string{"start", "stop", "restart", "status", "logs"}, args[0])
}

func lifecycle(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet("tellproof-agent "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "agent config file")
	dataDir := fs.String("data-dir", "", "the data directory (default from the config, ./.tellproof-agent)")
	open := fs.Bool("open", false, "start: open the dashboard in the browser")
	force := fs.Bool("force", false, "stop: end the agent if it has not stopped after 15 seconds")
	asJSON := fs.Bool("json", false, "status: print JSON")
	lines := fs.Int("n", 40, "logs: lines to print")
	follow := fs.Bool("f", false, "logs: keep printing new lines")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(stderr, lifecycleUsage)
		return 2
	}
	allowed := map[string][]string{
		"start":   {"config", "data-dir", "open"},
		"restart": {"config", "data-dir", "open"},
		"stop":    {"config", "data-dir", "force"},
		"status":  {"config", "data-dir", "json"},
		"logs":    {"config", "data-dir", "n", "f"},
	}[cmd]
	bad := ""
	fs.Visit(func(f *flag.Flag) {
		if !contains(allowed, f.Name) {
			bad = f.Name
		}
	})
	if bad != "" {
		fmt.Fprintf(stderr, "tellproof-agent %s: --%s is not one of its flags\n%s", cmd, bad, lifecycleUsage)
		return 2
	}
	if *dataDir != "" {
		inner := getenv
		dir := *dataDir
		getenv = func(k string) string {
			if k == "TELLPROOF_DATA_DIR" {
				return dir
			}
			return inner(k)
		}
	}
	cfg, err := agent.LoadConfig(*configPath, getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	switch cmd {
	case "start":
		return startAgent(ctx, cfg, *configPath, *open, getenv, stdout, stderr)
	case "stop":
		return stopAgent(ctx, cfg, *force, stdout, stderr)
	case "restart":
		if code := stopAgent(ctx, cfg, false, stdout, stderr); code != 0 {
			return code
		}
		return startAgent(ctx, cfg, *configPath, *open, getenv, stdout, stderr)
	case "status":
		return statusAgent(ctx, cfg, *asJSON, stdout, stderr)
	default:
		return logsAgent(ctx, cfg, *lines, *follow, stdout, stderr)
	}
}

// readRunState returns the recorded agent of dataDir, or nil when none is.
func readRunState(dataDir string) (*runState, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, runFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st runState
	if err := json.Unmarshal(b, &st); err != nil || st.PID <= 0 {
		return nil, fmt.Errorf("tellproof-agent: %s is not a run record; delete it if no agent runs", filepath.Join(dataDir, runFile))
	}
	return &st, nil
}

// running returns the live agent of dataDir, clearing a record left by one
// that is gone (a crash, a reboot).
func running(dataDir string) (*runState, error) {
	st, err := readRunState(dataDir)
	if err != nil || st == nil {
		return nil, err
	}
	if st.PID != os.Getpid() && !processAlive(st.PID) {
		_ = os.Remove(filepath.Join(dataDir, runFile))
		return nil, nil
	}
	return st, nil
}

func writeRunState(dataDir string, st runState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dataDir, runFile+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dataDir, runFile))
}

// claimRun records this process as dataDir's local agent, or says which
// agent already is. The returned function removes the record.
func claimRun(dataDir string, st runState) (func(), error) {
	if other, err := running(dataDir); err != nil {
		return nil, err
	} else if other != nil && other.PID != os.Getpid() {
		return nil, fmt.Errorf("tellproof-agent: an agent already runs on %s (pid %d, since %s); stop it with: %s stop",
			dataDir, other.PID, other.Started.Local().Format("15:04"), progName())
	}
	_ = os.Remove(filepath.Join(dataDir, stopFile)) // an old request must not stop this run
	st.PID, st.Started, st.Version = os.Getpid(), time.Now().UTC().Truncate(time.Second), version
	if err := writeRunState(dataDir, st); err != nil {
		return nil, err
	}
	return func() {
		if cur, _ := readRunState(dataDir); cur != nil && cur.PID == os.Getpid() {
			_ = os.Remove(filepath.Join(dataDir, runFile))
		}
	}, nil
}

// watchStop cancels when `stop` asks this agent to stop.
func watchStop(ctx context.Context, dataDir string, cancel context.CancelFunc) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	path := filepath.Join(dataDir, stopFile)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := os.Stat(path); err == nil {
				_ = os.Remove(path)
				cancel()
				return
			}
		}
	}
}

// startAgent runs `run --local` detached and waits until it has recorded
// itself (it listens by then) or failed.
func startAgent(ctx context.Context, cfg agent.Config, configPath string, open bool, getenv func(string) string, stdout, stderr io.Writer) int {
	name := progName()
	if st, err := running(cfg.DataDir); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	} else if st != nil {
		fmt.Fprintf(stdout, "The Tellproof agent is already running (pid %d, since %s).\n", st.PID, st.Started.Local().Format("15:04"))
		printRun(stdout, st, cfg.DataDir, name)
		return 0
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	logPath := filepath.Join(cfg.DataDir, logFile)
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > 5<<20 {
		_ = os.Rename(logPath, logPath+".1")
	}
	logf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer logf.Close()
	start, _ := logf.Seek(0, io.SeekEnd)
	fmt.Fprintf(logf, "--- %s: %s start ---\n", time.Now().UTC().Format(time.RFC3339), name)

	args := []string{"run", "--local"}
	if configPath != "" {
		if abs, err := filepath.Abs(configPath); err == nil {
			configPath = abs
		}
		args = append(args, "--config", configPath)
	}
	dir, _ := filepath.Abs(cfg.DataDir)
	child := exec.Command(exe, args...)
	child.Args[0] = os.Args[0] // keep the name the commands were run as
	// The child reads the same settings this command resolved, whatever
	// set them: the config file, the environment or --data-dir.
	child.Env = append(os.Environ(), "TELLPROOF_NO_BANNER=1", "TELLPROOF_BACKGROUND=1", "TELLPROOF_DATA_DIR="+dir,
		"TELLPROOF_OTLP_LISTEN="+cfg.OTLPListen, "TELLPROOF_DASHBOARD_LISTEN="+orOff(cfg.DashboardListen))
	if v := getenv(cfg.SaltSecretEnv); v != "" && os.Getenv(cfg.SaltSecretEnv) == "" {
		child.Env = append(child.Env, cfg.SaltSecretEnv+"="+v)
	}
	if wd, err := os.Getwd(); err == nil {
		child.Dir = wd
	}
	child.Stdin, child.Stdout, child.Stderr = nil, logf, logf
	detach(child)
	if child, err = startDetached(child); err != nil {
		fmt.Fprintf(stderr, "tellproof-agent: could not start the agent: %v\n", err)
		return 1
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	deadline := time.After(20 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(stdout, "Still starting (pid %d); check with: %s status\n", child.Process.Pid, name)
			return 1
		case err := <-exited:
			fmt.Fprintf(stderr, "The agent stopped while starting (%v). Its log:\n\n", err)
			printTail(stderr, logPath, start, 20)
			fmt.Fprintf(stderr, "\nDiagnose with: %s doctor\n", name)
			return 1
		case <-deadline:
			fmt.Fprintf(stderr, "The agent (pid %d) has not reported in after 20 seconds. Its log: %s\nCheck with: %s status, or %s doctor\n", child.Process.Pid, logPath, name, name)
			return 1
		case <-tick.C:
			st, _ := readRunState(cfg.DataDir)
			if st == nil || st.PID != child.Process.Pid {
				continue
			}
			fmt.Fprintf(stdout, "The Tellproof agent is running in the background (local mode, pid %d).\n", st.PID)
			printRun(stdout, st, cfg.DataDir, name)
			if open && st.Dashboard != "" {
				openBrowser(st.Dashboard)
			}
			return 0
		}
	}
}

func orOff(s string) string {
	if s == "" {
		return "off"
	}
	return s
}

func printRun(w io.Writer, st *runState, dataDir, name string) {
	spans := "http://" + dialAddr(st.OTLP)
	note := "the SDKs send here by default"
	if dialAddr(st.OTLP) != "127.0.0.1:4318" {
		note = "in the app: TELLPROOF_AGENT_URL=" + spans
	}
	fmt.Fprintf(w, "  Spans:     %s   %s\n", spans, note)
	if st.Dashboard != "" {
		fmt.Fprintf(w, "  Dashboard: %s\n", st.Dashboard)
	}
	fmt.Fprintf(w, "  Evidence:  %s   signed by this agent, never sent\n", shortPath(st.Evidence))
	if st.Background {
		fmt.Fprintf(w, "  Log:       %s\n", shortPath(filepath.Join(dataDir, logFile)))
	}
	fmt.Fprintf(w, "Stop it with: %s stop   ·   Status: %s status   ·   Problems: %s doctor\n", name, name, name)
}

// shortPath is path relative to the working directory when it is inside it.
func shortPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	wd, err := os.Getwd()
	if err != nil {
		return path
	}
	if rel, err := filepath.Rel(wd, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "." + string(filepath.Separator) + rel
	}
	return abs
}

// stopAgent asks dataDir's agent to stop and waits for it.
func stopAgent(ctx context.Context, cfg agent.Config, force bool, stdout, stderr io.Writer) int {
	st, err := running(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if st == nil {
		fmt.Fprintf(stdout, "No Tellproof agent is running from %s.\n", cfg.DataDir)
		elsewhere(ctx, stdout, cfg)
		return 0
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, stopFile), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	gone := func() bool {
		cur, _ := readRunState(cfg.DataDir)
		return cur == nil || cur.PID != st.PID || !processAlive(st.PID)
	}
	deadline := time.Now().Add(stopWait)
	for !gone() {
		if time.Now().After(deadline) || ctx.Err() != nil {
			if !force {
				_ = os.Remove(filepath.Join(cfg.DataDir, stopFile))
				fmt.Fprintf(stderr, "The agent (pid %d) has not stopped after %s. To end it now: %s stop --force\n", st.PID, stopWait, progName())
				return 1
			}
			if err := killProcess(st.PID); err != nil {
				fmt.Fprintf(stderr, "tellproof-agent: could not end pid %d: %v\n", st.PID, err)
				return 1
			}
			_ = os.Remove(filepath.Join(cfg.DataDir, runFile))
			_ = os.Remove(filepath.Join(cfg.DataDir, stopFile))
			fmt.Fprintf(stdout, "Ended the Tellproof agent (pid %d). The evidence it signed is kept in %s.\n", st.PID, shortPath(st.Evidence))
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintf(stdout, "Stopped the Tellproof agent (pid %d). The evidence it signed is kept in %s.\n", st.PID, shortPath(st.Evidence))
	return 0
}

// elsewhere says when an agent answers on the configured port although none
// is recorded in this data directory: one started from another directory.
func elsewhere(ctx context.Context, w io.Writer, cfg agent.Config) {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if probeReceiver(c, cfg.OTLPListen, func(string, string, string, string) {}) {
		fmt.Fprintf(w, "An agent does answer on %s: it was started from another directory or data directory. Run the command there, or pass --data-dir.\n", dialAddr(cfg.OTLPListen))
	}
}

// statusAgent reports dataDir's agent. Exit 0 running, 3 not running.
func statusAgent(ctx context.Context, cfg agent.Config, asJSON bool, stdout, stderr io.Writer) int {
	st, err := running(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if asJSON {
		out := struct {
			Running bool      `json:"running"`
			DataDir string    `json:"data_dir"`
			Agent   *runState `json:"agent,omitempty"`
		}{st != nil, cfg.DataDir, st}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	} else if st != nil {
		mode := "in the foreground"
		if st.Background {
			mode = "in the background"
		}
		fmt.Fprintf(stdout, "Running %s: pid %d, since %s, tellproof-agent %s, agent %s.\n",
			mode, st.PID, st.Started.Local().Format("2006-01-02 15:04"), st.Version, st.AgentID)
		printRun(stdout, st, cfg.DataDir, progName())
		if st.Version != version {
			fmt.Fprintf(stdout, "It runs %s, but this command is %s: %s restart runs the one you installed.\n", st.Version, version, progName())
		}
	} else {
		fmt.Fprintf(stdout, "Not running (data directory %s). Start it with: %s start\n", cfg.DataDir, progName())
		elsewhere(ctx, stdout, cfg)
	}
	if st == nil {
		return 3
	}
	return 0
}

// logsAgent prints the background agent's log, and follows it with -f.
func logsAgent(ctx context.Context, cfg agent.Config, n int, follow bool, stdout, stderr io.Writer) int {
	path := filepath.Join(cfg.DataDir, logFile)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		if st, _ := running(cfg.DataDir); st != nil && !st.Background {
			fmt.Fprintf(stdout, "The agent (pid %d) runs in the foreground: its output is in the terminal that started it.\n", st.PID)
			return 0
		}
		fmt.Fprintf(stdout, "No log yet in %s: %s start runs the agent in the background and logs there.\n", cfg.DataDir, progName())
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer f.Close()
	end := printTail(stdout, path, 0, n)
	if !follow {
		return 0
	}
	if _, err := f.Seek(end, io.SeekStart); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			fmt.Fprint(stdout, line)
		}
		if err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// printTail prints the last n lines of path written at or after offset from,
// and returns the file's size.
func printTail(w io.Writer, path string, from int64, n int) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	size := int64(len(b))
	if from > size {
		from = 0
	}
	lines := strings.SplitAfter(string(b[from:]), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if n >= 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	fmt.Fprint(w, strings.Join(lines, ""))
	return size
}

// openBrowser opens url in the default browser, quietly doing nothing when
// it cannot.
func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	if c.Start() == nil {
		go func() { _ = c.Wait() }()
	}
}
