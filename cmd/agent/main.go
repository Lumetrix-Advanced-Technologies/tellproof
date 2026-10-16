// Command plumbline-agent is the Plumbline evidence agent. It runs next to an
// AI product, turns its telemetry into signed hourly aggregates and sends only
// those (docs/architecture.md §3).
//
//	plumbline-agent local                         # no server: keep signed batches in the data directory
//	plumbline-agent report [--json]               # verify and summarise the local evidence log
//	plumbline-agent connect <token>               # enrol once, then run (the one-command install)
//	plumbline-agent enrol --config agent.json      # token in PLUMBLINE_ENROL_TOKEN
//	plumbline-agent [run] --config agent.json [--dry-run] [--replay export.jsonl]
//	plumbline-agent version
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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/agent"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/enrol"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/localreport"
)

// version is set at link time: go build -ldflags "-X main.version=v0.1.0".
var version = "dev"

const usage = `usage:
  plumbline-agent local [--config <agent.json>]
  plumbline-agent report [--config <agent.json>] [--data-dir <dir>] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--json]
  plumbline-agent connect [--config <agent.json>] <token>
  plumbline-agent enrol --config <agent.json> [--token <token>]
  plumbline-agent [run] --config <agent.json> [--dry-run] [--replay <export.jsonl>]
  plumbline-agent version

local runs with no server and no token: batches are signed with the agent's
own key and kept in <data-dir>/local, and report verifies and summarises them.
The enrolment token is read from PLUMBLINE_ENROL_TOKEN when --token is not
given. Secrets are read from the environment variables the config names.
Exit status: 0 ok, 1 runtime error, 2 usage or config error.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := "run"
	if len(args) > 0 && (args[0] == "run" || args[0] == "enrol" || args[0] == "connect" || args[0] == "version" || args[0] == "local" || args[0] == "report") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("plumbline-agent "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "agent config file")
	dryRun := fs.Bool("dry-run", false, "print what would be sent; send, queue and store nothing")
	replay := fs.String("replay", "", "feed an OTLP JSON export through the pipeline and exit")
	token := fs.String("token", "", "one-time enrolment token (default $PLUMBLINE_ENROL_TOKEN)")
	dataDir := fs.String("data-dir", "", "report: the data directory to read (default from the config)")
	since := fs.String("since", "", "report: first day to sum, YYYY-MM-DD (UTC)")
	until := fs.String("until", "", "report: last day to sum, YYYY-MM-DD (UTC)")
	asJSON := fs.Bool("json", false, "report: print JSON")
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
		fmt.Fprintf(stdout, "plumbline-agent %s\n", version)
		return 0
	case "connect":
		if *dryRun || *replay != "" || *token != "" {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return connect(ctx, *configPath, fs.Arg(0), getenv, stdout, stderr, log)
	case "local":
		if *dryRun || *replay != "" || *token != "" || *dataDir != "" || *since != "" || *until != "" || *asJSON {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return local(ctx, *configPath, getenv, stdout, stderr, log)
	case "report":
		if *dryRun || *replay != "" || *token != "" {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return report(*configPath, *dataDir, *since, *until, *asJSON, getenv, stdout, stderr)
	case "enrol":
		if *dryRun || *replay != "" {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return doEnrol(ctx, *configPath, *token, getenv, stdout, stderr)
	}
	if *token != "" || *dataDir != "" || *since != "" || *until != "" || *asJSON {
		fmt.Fprint(stderr, usage)
		return 2
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
		token = getenv("PLUMBLINE_ENROL_TOKEN")
	}
	if token == "" {
		fmt.Fprintln(stderr, "plumbline-agent: enrol needs a token: set PLUMBLINE_ENROL_TOKEN or pass --token")
		return 2
	}
	cfg, err := agent.LoadConfig(configPath, getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if cfg.Server == "" {
		fmt.Fprintln(stderr, "plumbline-agent: enrol needs a server in the config or PLUMBLINE_SERVER")
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
		token = getenv("PLUMBLINE_ENROL_TOKEN")
	}
	cfg, err := agent.LoadConfig(configPath, getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if cfg.Server == "" {
		fmt.Fprintln(stderr, "plumbline-agent: connect needs the server: set PLUMBLINE_SERVER")
		return 2
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, enrol.IdentityFile)); err == nil {
		log.Info("already enrolled; starting", "data_dir", cfg.DataDir)
	} else {
		if token == "" {
			fmt.Fprintln(stderr, "plumbline-agent: connect needs an enrolment token from the Plumbline app")
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
	a, err := agent.New(cfg, agent.Options{Local: true, Out: stdout, Log: log, Getenv: getenv, TickEvery: tickEvery})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	fmt.Fprintf(stdout, "plumbline-agent %s in local mode as %s: listening for spans on %s; signed batches are kept in %s and never sent.\nSee them with: plumbline-agent report\n",
		version, a.AgentID(), cfg.OTLPListen, filepath.Join(cfg.DataDir, agent.LocalDir))
	if err := a.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("agent stopped", "error", err.Error())
		return 1
	}
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
			fmt.Fprintf(stderr, "plumbline-agent: %q is not a date (YYYY-MM-DD)\n", d.in)
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
		return "", fmt.Errorf("plumbline-agent: salt secret: %w", err)
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
