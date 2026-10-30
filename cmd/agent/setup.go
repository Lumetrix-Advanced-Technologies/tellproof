package main

// Getting started (P8-2, D-064): `init` sets up the project in the working
// directory, `doctor` diagnoses an installation, and `demo` shows a dashboard
// on a synthetic week.

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/agent"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/demolog"
)

const setupUsage = `usage:
  tellproof-agent init [--name <ai system>] [--timezone <tz>] [--yes] [--no-start] [--force]
  tellproof-agent doctor [--config <agent.json>] [--data-dir <dir>] [--policy <file>] [--json]
  tellproof-agent demo [--data-dir <new dir>] [--days 7] [--listen <host:port>]

init looks at the project in the working directory, shows the SDK line for it
and where it goes, writes tellproof.policy.json and starts the agent in local
mode. --yes takes every default; --no-start stops before running the agent.
It changes no source file; in a git repository it offers to add the data
directory to .gitignore, since it holds the agent's private key.
doctor checks the configuration, the data directory, the ports, what the
SDKs will reach, the evidence log and the policy, with a fix for each
problem. Exit status: 0 nothing failed, 1 a check failed, 2 usage error.
demo writes a synthetic, signed week of evidence (nothing from your system)
to a new directory, with a policy for it, and serves the dashboard on it.
`

// isSetup reports whether args start a getting-started command.
func isSetup(args []string) bool {
	return len(args) > 0 && (args[0] == "init" || args[0] == "doctor" || args[0] == "demo")
}

// stdin and stdinIsTerminal are init's questions' input; tests replace them.
var (
	stdin           io.Reader = os.Stdin
	stdinIsTerminal           = func() bool { return isTTY(os.Stdin) } // not /dev/null, a character device too
)

func setupCommand(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet("tellproof-agent "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "agent config file")
	dataDir := fs.String("data-dir", "", "the data directory")
	policyPath := fs.String("policy", "", "doctor: the policy file")
	asJSON := fs.Bool("json", false, "doctor: print JSON")
	name := fs.String("name", "", "init: the AI system's name (default the directory's)")
	tz := fs.String("timezone", "", "init: IANA time zone (default $TZ, else UTC)")
	yes := fs.Bool("yes", false, "init: take every default, ask nothing")
	noStart := fs.Bool("no-start", false, "init: do not start the agent")
	force := fs.Bool("force", false, "init: replace an existing tellproof.policy.json")
	days := fs.Int("days", 7, "demo: days of hourly windows, ending now")
	listen := fs.String("listen", "", "demo: the dashboard's address (default 127.0.0.1:4319)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(stderr, setupUsage)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	allowed := map[string][]string{
		"init":   {"config", "name", "timezone", "yes", "no-start", "force"},
		"doctor": {"config", "data-dir", "policy", "json"},
		"demo":   {"data-dir", "days", "listen"},
	}[cmd]
	for f := range set {
		if !contains(allowed, f) {
			fmt.Fprintf(stderr, "tellproof-agent %s: --%s is not one of its flags\n%s", cmd, f, setupUsage)
			return 2
		}
	}
	switch cmd {
	case "init":
		return initProject(ctx, initOptions{config: *configPath, name: *name, tz: *tz, yes: *yes, noStart: *noStart, force: *force}, getenv, stdout, stderr)
	case "doctor":
		return doctor(ctx, *configPath, *dataDir, *policyPath, *asJSON, getenv, stdout, stderr)
	default:
		return demo(ctx, *dataDir, *days, *listen, getenv, stdout, stderr)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// demo writes the synthetic week and its policy, then serves the dashboard
// on them until ctx ends.
func demo(ctx context.Context, dir string, days int, listen string, getenv func(string) string, stdout, stderr io.Writer) int {
	if days < 1 || days > 90 {
		fmt.Fprintln(stderr, "tellproof-agent demo: --days is 1 to 90")
		return 2
	}
	if dir == "" {
		d, err := os.MkdirTemp("", "tellproof-demo-")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		dir = d
	}
	r, err := demolog.Write(dir, days, 7, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	pol := filepath.Join(dir, "tellproof.policy.json")
	if err := demolog.Policy(r).Write(pol); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if listen == "" {
		listen = "127.0.0.1:4319"
	}
	stop, url, err := startDashboard(listen, dir, pol, slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer stop()
	showBanner(stdout, getenv, "Demo")
	fmt.Fprintf(stdout, "Synthetic data: %d days of an imaginary claims assistant, signed and chained like a real log. Nothing here comes from your system.\n", days)
	fmt.Fprintf(stdout, "  Dashboard: %s\n  Evidence:  %s (%d hourly batches)\n  Policy:    %s\n\n", url, dir, r.Batches, pol)
	fmt.Fprintln(stdout, "Look for the night deploy without the guardrail (coverage dropped, two canaries got through and no incident was opened) and the prompt change nobody recorded: the checks that fail are the ones that caught them. Then, in another terminal:")
	fmt.Fprintf(stdout, "  tellproof-agent report --data-dir %s\n", dir)
	fmt.Fprintf(stdout, "  tellproof-agent check --data-dir %s --policy %s\n", dir, pol)
	fmt.Fprintf(stdout, "  tellproof-agent check --self-test --data-dir %s --policy %s\n", dir, pol)
	fmt.Fprintln(stdout, "Ctrl-C to stop. For your own app: tellproof-agent init")
	<-ctx.Done()
	return 0
}

type initOptions struct {
	config, name, tz    string
	yes, noStart, force bool
	ask                 func(question, def string) string // nil: defaults
}

// project is what init found in the working directory.
type project struct {
	python, node bool
	pythonVia    string // the file that says so: pyproject.toml, requirements.txt, ...
	nodeVia      string
	pyEntry      string // the file that starts the app, when there is a clear one
	nodeEntry    string
	nodeESM      bool
}

// detect looks at dir's top level only.
func detect(dir string) project {
	var p project
	exists := func(n string) bool { _, err := os.Stat(filepath.Join(dir, n)); return err == nil }
	for _, f := range []string{"pyproject.toml", "requirements.txt", "setup.py", "setup.cfg", "Pipfile"} {
		if exists(f) {
			p.python, p.pythonVia = true, f
			break
		}
	}
	if exists("package.json") {
		p.node, p.nodeVia = true, "package.json"
	}
	entries, _ := os.ReadDir(dir)
	var py, js []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch ext := filepath.Ext(e.Name()); ext {
		case ".py":
			py = append(py, e.Name())
		case ".js", ".mjs", ".cjs", ".ts", ".mts", ".cts":
			if !strings.HasSuffix(e.Name(), ".config.js") && !strings.HasSuffix(e.Name(), ".config.mjs") && !strings.HasSuffix(e.Name(), ".config.ts") {
				js = append(js, e.Name())
			}
		}
	}
	if !p.python && len(py) > 0 {
		p.python, p.pythonVia = true, py[0]
	}
	if !p.node && len(js) > 0 {
		p.node, p.nodeVia = true, js[0]
	}
	p.pyEntry = pickEntry(py, "main.py", "app.py", "server.py", "manage.py", "wsgi.py", "asgi.py", "run.py")
	if p.node {
		var pkg struct {
			Type    string            `json:"type"`
			Main    string            `json:"main"`
			Scripts map[string]string `json:"scripts"`
		}
		if b, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
			_ = json.Unmarshal(b, &pkg)
		}
		p.nodeESM = pkg.Type == "module"
		if m := regexp.MustCompile(`\b(?:node|tsx|ts-node|bun)\s+(?:--\S+\s+)*([\w./-]+\.[cm]?[jt]s)\b`).FindStringSubmatch(pkg.Scripts["start"]); m != nil {
			p.nodeEntry = m[1]
		} else if pkg.Main != "" && exists(pkg.Main) {
			p.nodeEntry = pkg.Main
		} else {
			p.nodeEntry = pickEntry(js, "index.js", "index.mjs", "index.ts", "server.js", "server.mjs", "server.ts", "app.js", "app.mjs", "app.ts", "main.js", "main.ts")
		}
		if strings.HasSuffix(p.nodeEntry, ".mjs") || strings.HasSuffix(p.nodeEntry, ".mts") {
			p.nodeESM = true
		}
	}
	return p
}

// pickEntry returns the first of names among files, else the only file.
func pickEntry(files []string, names ...string) string {
	for _, n := range names {
		if contains(files, n) {
			return n
		}
	}
	if len(files) == 1 {
		return files[0]
	}
	return ""
}

var serviceRE = regexp.MustCompile(`[^a-z0-9._-]+`)

func initProject(ctx context.Context, o initOptions, getenv func(string) string, stdout, stderr io.Writer) int {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if o.ask == nil && !o.yes && stdinIsTerminal() {
		in := bufio.NewReader(stdin)
		o.ask = func(question, def string) string {
			fmt.Fprintf(stdout, "%s [%s]: ", question, def)
			line, err := in.ReadString('\n')
			if line = strings.TrimSpace(line); line == "" || err != nil && err != io.EOF {
				return def
			}
			return line
		}
	}
	ask := func(q, def string) string {
		if o.ask == nil {
			return def
		}
		return o.ask(q, def)
	}
	yesNo := func(q string) bool {
		a := strings.ToLower(ask(q+" (y/n)", "y"))
		return a == "y" || a == "yes"
	}

	showBanner(stdout, getenv, "Set up")
	p := detect(wd)
	base := filepath.Base(wd)
	if o.name == "" {
		o.name = ask("Name of the AI system", base)
	}
	if o.tz == "" {
		def := getenv("TZ")
		if _, err := time.LoadLocation(def); def == "" || err != nil {
			def = "UTC"
		}
		o.tz = ask("Its time zone (IANA, for daily figures)", def)
	}
	service := strings.Trim(serviceRE.ReplaceAllString(strings.ToLower(o.name), "-"), "-.")
	if service == "" {
		service = "my-ai-app"
	}

	fmt.Fprintln(stdout)
	switch {
	case p.python && p.node:
		fmt.Fprintf(stdout, "This looks like a Python project (%s) and a Node.js one (%s). Add the SDK to the one that calls the model:\n\n", p.pythonVia, p.nodeVia)
	case p.python:
		fmt.Fprintf(stdout, "This looks like a Python project (%s).\n\n", p.pythonVia)
	case p.node:
		fmt.Fprintf(stdout, "This looks like a Node.js project (%s).\n\n", p.nodeVia)
	default:
		fmt.Fprintln(stdout, "No Python or Node.js files here. Run init in your app's directory, or add the SDK by hand:")
		fmt.Fprintln(stdout)
		p.python, p.node = true, true
	}
	step := 1
	if p.python {
		where := "the module that starts your app"
		if p.pyEntry != "" {
			where = p.pyEntry
		}
		fmt.Fprintf(stdout, "  %d. pip install \"tellproof[agent]\"   (the SDK and the agent; your app's dependencies need only tellproof)\n", step)
		fmt.Fprintf(stdout, "  %d. At the top of %s, before the first model call:\n\n", step+1, where)
		fmt.Fprintf(stdout, "         import tellproof\n         tellproof.init(service=%q)\n\n", service)
		step += 2
	}
	if p.node {
		where := "the file that starts your app"
		if p.nodeEntry != "" {
			where = p.nodeEntry
		}
		fmt.Fprintf(stdout, "  %d. npm install tellproof            (the SDK, with the agent; then run its commands as npx tellproof ...)\n", step)
		fmt.Fprintf(stdout, "  %d. At the top of %s, before the first model call:\n\n", step+1, where)
		if p.nodeESM {
			fmt.Fprintf(stdout, "         import * as tellproof from 'tellproof';\n         tellproof.init({ service: '%s' });\n         await tellproof.ready();   // when a model is called straight away\n\n", service)
		} else {
			fmt.Fprintf(stdout, "         const tellproof = require('tellproof');\n         tellproof.init({ service: '%s' });\n\n", service)
		}
	}
	fmt.Fprintln(stdout, "  With the openai, anthropic or AWS Bedrock client libraries, every call is then recorded, without its content,")
	fmt.Fprintln(stdout, "  and each web request is its own trace. Have a guardrail check or a redaction step? Mark it once:")
	if p.python {
		fmt.Fprintln(stdout, "      @tellproof.guardrail_check   on the check     @tellproof.redactor   on the redaction")
	}
	if p.node {
		fmt.Fprintln(stdout, "      tellproof.guardrailCheck(check)                 tellproof.redactor(redact)")
	}
	fmt.Fprintln(stdout, "  The SDKs send to http://127.0.0.1:4318; set TELLPROOF_AGENT_URL in the app if the agent listens elsewhere.")
	fmt.Fprintln(stdout)

	cfg, err := agent.LoadConfig(o.config, getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	policyPath := filepath.Join(wd, "tellproof.policy.json")
	if _, err := os.Stat(policyPath); err == nil && !o.force {
		fmt.Fprintf(stdout, "%s already exists; keeping it (--force replaces it).\n", policyPath)
	} else if code := policyInit(policyPath, cfg.DataDir, o.name, o.tz, true, getenv, io.Discard, stderr); code != 0 {
		return code
	} else {
		fmt.Fprintf(stdout, "Wrote %s for %s. Once the agent has seen traffic, review the models and regions it declares: tellproof-agent policy init --force fills them in from the log.\n", filepath.Base(policyPath), o.name)
	}

	if ignored, inRepo := gitIgnored(wd, cfg.DataDir); inRepo && !ignored {
		entry := filepath.ToSlash(cfg.DataDir)
		if abs, err := filepath.Abs(cfg.DataDir); err == nil {
			if rel, err := filepath.Rel(wd, abs); err == nil && !strings.HasPrefix(rel, "..") {
				entry = "/" + filepath.ToSlash(rel) + "/"
			}
		}
		if (o.yes || o.ask != nil) && yesNo("Add "+entry+" to .gitignore? It will hold the agent's private key") {
			if err := appendLine(filepath.Join(wd, ".gitignore"), entry); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			fmt.Fprintf(stdout, "Added %s to .gitignore.\n", entry)
		} else {
			fmt.Fprintf(stdout, "Add %s to .gitignore: it will hold the agent's private key and evidence log.\n", entry)
		}
	}

	// Never block a script: without a terminal, --yes stops here.
	start := !o.noStart && (o.ask != nil || o.yes && stdinIsTerminal())
	if start && o.ask != nil {
		start = yesNo("Start the agent now, in the background")
	}
	n := progName()
	if !start {
		fmt.Fprintf(stdout, "\nNext: %s start, then start your app and open http://127.0.0.1:4319. Something not arriving? %s doctor\n", n, n)
		return 0
	}
	fmt.Fprintln(stdout)
	if code := startAgent(ctx, cfg, o.config, false, getenv, stdout, stderr); code != 0 {
		return code
	}
	fmt.Fprintln(stdout, "\nNow start your app: the first batch shows on the dashboard about a minute after the first model call.")
	return 0
}

// gitIgnored reports whether path is ignored in the git work tree that holds
// dir, and whether there is one. Without git, it reports no repository.
func gitIgnored(dir, path string) (ignored, inRepo bool) {
	if _, err := exec.LookPath("git"); err != nil {
		return false, false
	}
	if err := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		return false, false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, true
	}
	if rel, err := filepath.Rel(dir, abs); err != nil || strings.HasPrefix(rel, "..") {
		return true, true // outside the work tree: nothing to ignore
	}
	// --no-index: also when the path does not exist yet; the trailing slash
	// says it is a directory, so a "dir/" pattern matches.
	err = exec.Command("git", "-C", dir, "check-ignore", "-q", "--no-index", abs+string(filepath.Separator)).Run()
	return err == nil, true
}

func appendLine(path, line string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		line = "\n" + line
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
