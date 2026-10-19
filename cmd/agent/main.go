// Command tellproof-agent is the Tellproof evidence agent. It runs next to an
// AI product, turns its telemetry into signed hourly aggregates and sends only
// those (docs/architecture.md §3).
//
//	tellproof-agent init                          # set up the project in the working directory
//	tellproof-agent start | stop | status          # the local agent in the background, with a dashboard
//	tellproof-agent run --local                   # the same in the foreground
//	tellproof-agent doctor                        # diagnose the installation, with a fix for each problem
//	tellproof-agent demo                          # the dashboard on a synthetic, signed week
//	tellproof-agent report [--json]               # verify and summarise the local evidence log
//	tellproof-agent dashboard [--data-dir <dir>]  # the dashboard alone, for a copied log
//	tellproof-agent connect <token>               # enrol once, then run (the one-command install)
//	tellproof-agent enrol --config agent.json      # token in TELLPROOF_ENROL_TOKEN
//	tellproof-agent [run] --config agent.json [--dry-run] [--replay export.jsonl]
//	tellproof-agent version
//
// --dry-run prints every batch it would send, with its payload decoded, and
// sends, queues and stores nothing. --replay feeds an OTLP JSON export through
// the same pipeline, emits every window and exits.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/agent"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/dashboard"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/enrol"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/envcompat"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/localreport"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/policy"
)

// version is set at link time: go build -ldflags "-X main.version=v0.1.0".
var version = "dev"

const usage = `usage:
  tellproof-agent                    (in a terminal: the welcome banner and first steps)
  tellproof-agent init | doctor | demo ...   (set up a project, diagnose, a dashboard on synthetic data)
  tellproof-agent start | stop | restart | status | logs ...   (the local agent in the background)
  tellproof-agent run --local [--config <agent.json>]
  tellproof-agent report [--config <agent.json>] [--data-dir <dir>] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--json]
  tellproof-agent dashboard [--config <agent.json>] [--data-dir <dir>] [--listen <host:port>]
  tellproof-agent policy init | check | aibom ...   (run one with --help for its flags)
  tellproof-agent ci run | canary | attest | verify | keygen ...   (CI evidence per commit)
  tellproof-agent connect [--config <agent.json>] <token>
  tellproof-agent enrol --config <agent.json> [--token <token>]
  tellproof-agent [run] --config <agent.json> [--dry-run] [--replay <export.jsonl>]
  tellproof-agent version

run --local (or just local) runs with no server and no token: batches are
signed with the agent's own key and kept in <data-dir>/local, a dashboard is
served on http://127.0.0.1:4319, and report verifies and summarises the log.
start runs the same in the background and stop ends it.
The enrolment token is read from TELLPROOF_ENROL_TOKEN when --token is not
given. Secrets are read from the environment variables the config names.
Exit status: 0 ok, 1 runtime error, 2 usage or config error.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	getenv = envcompat.Wrap(getenv) // TELLPROOF_*, else the PLUMBLINE_* names of v0.1 and v0.2
	if isGovernance(args) {
		return governance(args, getenv, stdout, stderr)
	}
	if isCI(args) {
		return ciCommand(ctx, args, getenv, stdout, stderr)
	}
	if isSetup(args) {
		return setupCommand(ctx, args, getenv, stdout, stderr)
	}
	if isLifecycle(args) {
		return lifecycle(ctx, args, getenv, stdout, stderr)
	}
	if len(args) == 0 && welcome(stdout, getenv) {
		return 0 // in a terminal, no arguments shows the welcome; elsewhere it still means run
	}
	cmd := "run"
	if len(args) > 0 && (args[0] == "run" || args[0] == "enrol" || args[0] == "connect" || args[0] == "version" || args[0] == "local" || args[0] == "report" || args[0] == "dashboard") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("tellproof-agent "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "agent config file")
	dryRun := fs.Bool("dry-run", false, "print what would be sent; send, queue and store nothing")
	replay := fs.String("replay", "", "feed an OTLP JSON export through the pipeline and exit")
	token := fs.String("token", "", "one-time enrolment token (default $TELLPROOF_ENROL_TOKEN)")
	dataDir := fs.String("data-dir", "", "report: the data directory to read (default from the config)")
	since := fs.String("since", "", "report: first day to sum, YYYY-MM-DD (UTC)")
	until := fs.String("until", "", "report: last day to sum, YYYY-MM-DD (UTC)")
	asJSON := fs.Bool("json", false, "report: print JSON")
	localMode := fs.Bool("local", false, "run: no server; keep signed batches in the data directory")
	listen := fs.String("listen", "", "dashboard: address to serve on (default from the config, 127.0.0.1:4319)")
	maxArgs := 0
	if cmd == "connect" {
		maxArgs = 1 // the token
	}
	if err := fs.Parse(args); err != nil || fs.NArg() > maxArgs {
		fmt.Fprint(stderr, usage)
		return 2
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	switch cmd {
	case "version":
		fmt.Fprintf(stdout, "tellproof-agent %s\n", version)
		return 0
	case "connect":
		if *dryRun || *replay != "" || *token != "" {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return connect(ctx, *configPath, fs.Arg(0), getenv, stdout, stderr, log)
	case "local":
		*localMode = true
		cmd = "run"
	case "report":
		if *dryRun || *replay != "" || *token != "" || *localMode || *listen != "" {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return report(*configPath, *dataDir, *since, *until, *asJSON, getenv, stdout, stderr)
	case "dashboard":
		if *dryRun || *replay != "" || *token != "" || *localMode || *since != "" || *until != "" || *asJSON {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return dashboardOnly(ctx, *configPath, *dataDir, *listen, getenv, stdout, stderr, log)
	case "enrol":
		if *dryRun || *replay != "" {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return doEnrol(ctx, *configPath, *token, getenv, stdout, stderr)
	}
	if *token != "" || *dataDir != "" || *since != "" || *until != "" || *asJSON || *listen != "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if *localMode {
		if *dryRun || *replay != "" {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return local(ctx, *configPath, getenv, stdout, stderr, log)
	}
	cfg, err := agent.LoadConfig(*configPath, getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	a, err := agent.New(cfg, agent.Options{DryRun: *dryRun, Out: stdout, Log: log, Getenv: getenv})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *replay != "" {
		if err := doReplay(ctx, a, *replay); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if err := a.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("agent stopped", "error", err.Error())
		return 1
	}
	return 0
}

func doReplay(ctx context.Context, a *agent.Agent, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := a.Replay(f); err != nil {
		return err
	}
	if err := a.Shutdown(ctx); err != nil {
		return err
	}
	return a.Deliver(ctx)
}

func doEnrol(ctx context.Context, configPath, token string, getenv func(string) string, stdout, stderr io.Writer) int {
	if token == "" {
		token = getenv("TELLPROOF_ENROL_TOKEN")
	}
	if token == "" {
		fmt.Fprintln(stderr, "tellproof-agent: enrol needs a token: set TELLPROOF_ENROL_TOKEN or pass --token")
		return 2
	}
	cfg, err := agent.LoadConfig(configPath, getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if cfg.Server == "" {
		fmt.Fprintln(stderr, "tellproof-agent: enrol needs a server in the config or TELLPROOF_SERVER")
		return 2
	}
	id, err := enrol.Enrol(ctx, enrol.Options{Server: cfg.Server, Token: token, DataDir: cfg.DataDir, HTTP: &http.Client{Timeout: 30 * time.Second}})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "enrolled as %s with %s; identity in %s\n", id.AgentID, id.Server, cfg.DataDir)
	return 0
}

// connect is the one-command install (S-5): enrol with the token unless this
// data directory is already enrolled (so a restarted container just runs),
// keep a salt secret in the data directory when none is configured, then run.
func connect(ctx context.Context, configPath, token string, getenv func(string) string, stdout, stderr io.Writer, log *slog.Logger) int {
	if token == "" {
		token = getenv("TELLPROOF_ENROL_TOKEN")
	}
	cfg, err := agent.LoadConfig(configPath, getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if cfg.Server == "" {
		fmt.Fprintln(stderr, "tellproof-agent: connect needs the server: set TELLPROOF_SERVER")
		return 2
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, enrol.IdentityFile)); err == nil {
		log.Info("already enrolled; starting", "data_dir", cfg.DataDir)
	} else {
		if token == "" {
			fmt.Fprintln(stderr, "tellproof-agent: connect needs an enrolment token from the Tellproof app")
			return 2
		}
		id, err := enrol.Enrol(ctx, enrol.Options{Server: cfg.Server, Token: token, DataDir: cfg.DataDir, HTTP: &http.Client{Timeout: 30 * time.Second}})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "enrolled as %s with %s\n", id.AgentID, id.Server)
	}
	if getenv(cfg.SaltSecretEnv) == "" && cfg.SaltSecretFile == "" {
		path, err := ensureSaltSecret(cfg.DataDir)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		cfg.SaltSecretFile = path
	}
	a, err := agent.New(cfg, agent.Options{Out: stdout, Log: log, Getenv: getenv, TickEvery: tickEvery})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := a.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("agent stopped", "error", err.Error())
		return 1
	}
	return 0
}

// local runs the agent with no server (D-052): batches are signed with the
// agent's own key and kept in the data directory, for `report` to read.
func local(ctx context.Context, configPath string, getenv func(string) string, stdout, stderr io.Writer, log *slog.Logger) int {
	if interactive, _, _ := terminal(stdout, getenv); interactive {
		// Under the banner, only warnings and errors, as text; scripts and
		// logs (no terminal, or TELLPROOF_NO_BANNER) keep the JSON log.
		log = slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	cfg, err := agent.LoadConfig(configPath, getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if cfg.Server != "" {
		log.Warn("local mode ignores the server; nothing is sent", "server", cfg.Server)
		cfg.Server = ""
	}
	if getenv(cfg.SaltSecretEnv) == "" && cfg.SaltSecretFile == "" {
		path, err := ensureSaltSecret(cfg.DataDir)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		cfg.SaltSecretFile = path
	}
	if other, err := running(cfg.DataDir); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	} else if other != nil && other.PID != os.Getpid() {
		fmt.Fprintf(stderr, "tellproof-agent: an agent already runs on %s (pid %d, since %s). Stop it with: %s stop\n",
			cfg.DataDir, other.PID, other.Started.Local().Format("15:04"), progName())
		return 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a, err := agent.New(cfg, agent.Options{Local: true, Out: stdout, Log: log, Getenv: getenv, TickEvery: tickEvery})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	stopDash, url, err := startDashboard(cfg.DashboardListen, cfg.DataDir, policy.Find(envcompat.Get, cfg.DataDir), log)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer stopDash()
	logDir := filepath.Join(cfg.DataDir, agent.LocalDir)
	// While it listens, the agent records itself for start, stop and status
	// (D-067) and watches for a stop request.
	release := func() {}
	defer func() { release() }()
	a.OnListening(func() {
		r, err := claimRun(cfg.DataDir, runState{AgentID: a.AgentID(), OTLP: cfg.OTLPListen, Dashboard: url, Evidence: logDir,
			Background: getenv("TELLPROOF_BACKGROUND") != ""})
		if err != nil {
			log.Warn("not recorded as running, so stop and status will not find this agent", "error", err.Error())
			return
		}
		release = r
		go watchStop(ctx, cfg.DataDir, cancel)
	})
	if showBanner(stdout, getenv, "Local mode") {
		localStatus(stdout, getenv, a.AgentID(), cfg.OTLPListen, url, logDir)
	} else {
		fmt.Fprintf(stdout, "tellproof-agent %s in local mode as %s: listening for spans on %s; signed batches are kept in %s and never sent.\n",
			version, a.AgentID(), cfg.OTLPListen, logDir)
		if url != "" {
			fmt.Fprintf(stdout, "Dashboard: %s\n", url)
		}
		fmt.Fprintf(stdout, "In a terminal: %s report. To stop: Ctrl-C, or %s stop\n", progName(), progName())
	}
	if err := a.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("agent stopped", "error", err.Error())
		return 1
	}
	return 0
}

// startDashboard serves the dashboard for dataDir, with the policy at
// policyPath, on addr ("" or "off": not at all) and returns a stop function
// and the URL to open.
func startDashboard(addr, dataDir, policyPath string, log *slog.Logger) (func(), string, error) {
	if addr == "" || addr == "off" {
		return func() {}, "", nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("tellproof-agent: dashboard: %w (set TELLPROOF_DASHBOARD_LISTEN, or off)", err)
	}
	srv := &http.Server{Handler: dashboard.Handler(dashboard.Options{DataDir: dataDir, Version: version, PolicyPath: policyPath}),
		ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() || ip.IsLoopback() {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/"
	log.Info("dashboard", "url", url)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, url, nil
}

// dashboardOnly serves the dashboard for a data directory (a copied log, or
// a local agent running elsewhere) until ctx ends.
func dashboardOnly(ctx context.Context, configPath, dataDir, listen string, getenv func(string) string, stdout, stderr io.Writer, log *slog.Logger) int {
	cfg, err := agent.LoadConfig(configPath, getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if dataDir == "" {
		dataDir = cfg.DataDir
	}
	if listen == "" {
		listen = cfg.DashboardListen
	}
	if listen == "off" || listen == "" {
		listen = "127.0.0.1:4319"
	}
	stop, url, err := startDashboard(listen, dataDir, policy.Find(envcompat.Get, dataDir), log)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer stop()
	fmt.Fprintf(stdout, "Dashboard for %s: %s\n", dataDir, url)
	<-ctx.Done()
	return 0
}

// report verifies the local evidence log and prints its summary. Exit 0 when
// it verifies, 1 when a batch fails, 2 for usage errors or no log.
func report(configPath, dataDir, since, until string, asJSON bool, getenv func(string) string, stdout, stderr io.Writer) int {
	if dataDir == "" {
		cfg, err := agent.LoadConfig(configPath, getenv)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		dataDir = cfg.DataDir
	}
	var r localreport.Range
	for _, d := range []struct {
		in  string
		out *time.Time
		add int
	}{{since, &r.From, 0}, {until, &r.To, 1}} {
		if d.in == "" {
			continue
		}
		t, err := time.Parse("2006-01-02", d.in)
		if err != nil {
			fmt.Fprintf(stderr, "tellproof-agent: %q is not a date (YYYY-MM-DD)\n", d.in)
			return 2
		}
		*d.out = t.AddDate(0, 0, d.add)
	}
	s, err := localreport.Read(dataDir, r)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if asJSON {
		err = localreport.WriteJSON(stdout, s)
	} else {
		err = localreport.WriteText(stdout, s)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !s.Verified {
		return 1
	}
	return 0
}

// tickEvery is the agent's tick; tests shorten it.
var tickEvery = time.Minute

// ensureSaltSecret keeps a random salt secret in the data directory (0600):
// the agent's trace commitments stay stable across restarts, and the vendor
// can disclose a trace later as long as the data directory is kept.
func ensureSaltSecret(dir string) (string, error) {
	path := filepath.Join(dir, "salt.secret")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("tellproof-agent: salt secret: %w", err)
	}
	if _, err := f.WriteString(hex.EncodeToString(b) + "\n"); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	return path, f.Close()
}
