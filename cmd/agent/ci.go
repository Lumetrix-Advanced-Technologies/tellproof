package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/cievidence"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/policy"
)

const ciUsage = `usage:
  tellproof-agent ci run --gate <name> [--junit <file>] [--dir <dir>] -- <command> [args...]
  tellproof-agent ci canary --gate <name> [--patch <file>] [--dir <dir>] -- <command> [args...]
  tellproof-agent ci attest [--dir <dir>] [--base <rev>] [--key <file>] [--unsigned] [--out <file>] [--predicate <file>]
  tellproof-agent ci verify [--policy <file>] [--require a,b] [--canary a,b] [--commit <sha>] [--pub <file>] [--unsigned] [--json] <statement>
  tellproof-agent ci keygen --out <file>

ci run runs a CI gate (tests, lint, a scan) with its output and exit status
passed straight through, and records only the gate's name, exit status,
duration, a digest of the command line and, with --junit, the report's test
counts. ci canary runs a must-fail check: the gate on known-bad code (--patch
is applied to a temporary worktree first); it exits 0 when the gate failed as
it should and 1 when it passed. Records go to --dir (default
$TELLPROOF_CI_DIR, else .tellproof-ci). ci attest writes the commit's
in-toto statement, signed with --key (default the PEM key in
$TELLPROOF_CI_KEY) or, with --unsigned, bare for GitHub artifact
attestations to sign (--predicate also writes the predicate alone, for
actions/attest). ci verify checks a statement against the policy's "ci"
section, or --require and --canary.
`

func isCI(args []string) bool { return len(args) > 0 && args[0] == "ci" }

func ciCommand(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprint(stderr, ciUsage)
		return 2
	}
	sub, args := args[1], args[2:]
	fs := flag.NewFlagSet("tellproof-agent ci "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "where gate runs are recorded")
	gate := fs.String("gate", "", "run, canary: the gate's name")
	junit := fs.String("junit", "", "run: a JUnit XML report the command writes")
	patch := fs.String("patch", "", "canary: a patch that breaks the code, applied to a temporary worktree")
	base := fs.String("base", "", "attest: count coding-agent commits in base..HEAD (default HEAD only)")
	key := fs.String("key", "", "attest: the Ed25519 signing key (PEM)")
	unsigned := fs.Bool("unsigned", false, "attest: write the bare statement; verify: accept one")
	out := fs.String("out", "", "attest: write here instead of standard output; keygen: the private key file")
	predicateOut := fs.String("predicate", "", "attest: also write the predicate alone here (for actions/attest)")
	policyPath := fs.String("policy", "", "verify: the policy file with a ci section")
	require := fs.String("require", "", "verify: gates that must have passed, comma-separated")
	canary := fs.String("canary", "", "verify: gates whose canary must have been caught, comma-separated")
	commit := fs.String("commit", "", "verify: the commit (default the checked-out one)")
	pub := fs.String("pub", "", "verify: the signer's public key (PEM)")
	asJSON := fs.Bool("json", false, "verify: print JSON")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(stderr, ciUsage)
		return 2
	}
	if *dir == "" {
		*dir = getenv("TELLPROOF_CI_DIR")
	}
	if *dir == "" {
		*dir = ".tellproof-ci"
	}
	git := cievidence.Git{}
	switch sub {
	case "run", "canary":
		if *gate == "" || fs.NArg() == 0 {
			fmt.Fprint(stderr, ciUsage)
			return 2
		}
		return ciRun(ctx, git, sub == "canary", *gate, *junit, *patch, *dir, fs.Args(), stderr)
	case "attest":
		if fs.NArg() > 0 {
			fmt.Fprint(stderr, ciUsage)
			return 2
		}
		return ciAttest(ctx, git, *dir, *base, *key, *unsigned, *out, *predicateOut, getenv, stdout, stderr)
	case "verify":
		if fs.NArg() != 1 {
			fmt.Fprint(stderr, ciUsage)
			return 2
		}
		return ciVerify(git, fs.Arg(0), *policyPath, *require, *canary, *commit, *pub, *unsigned, *asJSON, getenv, stdout, stderr)
	case "keygen":
		if *out == "" {
			fmt.Fprint(stderr, ciUsage)
			return 2
		}
		return ciKeygen(*out, stdout, stderr)
	}
	fmt.Fprint(stderr, ciUsage)
	return 2
}

func ciRun(ctx context.Context, git cievidence.Git, isCanary bool, gate, junit, patch, dir string, argv []string, stderr io.Writer) int {
	if !cievidence.GateRE.MatchString(gate) {
		fmt.Fprintf(stderr, "tellproof-agent: %q is not a gate name (lower-case letters, digits, '.', '_' or '-', at most 64)\n", gate)
		return 2
	}
	head, err := git.Head()
	if err != nil {
		fmt.Fprintf(stderr, "tellproof-agent: ci needs a git checkout: %v\n", err)
		return 2
	}
	g := cievidence.Gate{Name: gate, Canary: isCanary, CommandDigest: cievidence.CommandDigest(argv)}
	workDir := ""
	if isCanary && patch != "" {
		if g.PatchDigest, err = cievidence.FileDigest(patch); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		var cleanup func()
		if workDir, cleanup, err = git.PatchedWorktree(patch); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		defer cleanup()
	}
	if junit != "" {
		_ = os.Remove(junit) // a stale report from an earlier run must not be counted
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = workDir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	start := time.Now()
	runErr := cmd.Run()
	g.DurationMS = time.Since(start).Milliseconds()
	g.Started = start.UTC().Format(time.RFC3339Nano)
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		g.ExitCode = 0
	case errors.As(runErr, &exitErr):
		g.ExitCode = exitErr.ExitCode()
	default:
		g.ExitCode = 127 // could not start
		fmt.Fprintf(stderr, "tellproof-agent: %v\n", runErr)
	}
	if isCanary {
		g.Passed = g.ExitCode != 0
	} else {
		g.Passed = g.ExitCode == 0
	}
	if junit != "" && !isCanary {
		if g.JUnit, err = cievidence.ReadJUnit(junit); err != nil {
			fmt.Fprintf(stderr, "tellproof-agent: %v (recorded without test counts)\n", err)
		}
	}
	if _, err := cievidence.Save(dir, head, g); err != nil {
		fmt.Fprintf(stderr, "tellproof-agent: could not record the gate: %v\n", err)
	}
	if isCanary {
		if g.Passed {
			fmt.Fprintf(stderr, "tellproof-agent: canary %s caught: the gate failed on known-bad code (exit %d), as it should.\n", gate, g.ExitCode)
			return 0
		}
		fmt.Fprintf(stderr, "tellproof-agent: canary %s MISSED: the gate passed on known-bad code, so it cannot tell broken code from working code.\n", gate)
		return 1
	}
	return g.ExitCode
}

func ciContext(git cievidence.Git, getenv func(string) string) (repo, ref string, run cievidence.Run) {
	run = cievidence.Run{Provider: "local"}
	switch {
	case getenv("GITHUB_ACTIONS") == "true":
		server := getenv("GITHUB_SERVER_URL")
		if server == "" {
			server = "https://github.com"
		}
		repo = cievidence.RepoName(server + "/" + getenv("GITHUB_REPOSITORY"))
		ref = getenv("GITHUB_REF")
		run = cievidence.Run{Provider: "github", ID: getenv("GITHUB_RUN_ID"), Attempt: getenv("GITHUB_RUN_ATTEMPT"),
			URL: strings.TrimSuffix(server, "/") + "/" + getenv("GITHUB_REPOSITORY") + "/actions/runs/" + getenv("GITHUB_RUN_ID")}
	case getenv("GITLAB_CI") == "true":
		repo = cievidence.RepoName(getenv("CI_PROJECT_URL"))
		ref = getenv("CI_COMMIT_REF_NAME")
		run = cievidence.Run{Provider: "gitlab", ID: getenv("CI_PIPELINE_ID"), URL: getenv("CI_PIPELINE_URL")}
	}
	if repo == "" {
		repo = git.Remote()
	}
	if repo == "" {
		repo = "unknown"
	}
	if ref == "" {
		ref = git.Ref()
	}
	return repo, ref, run
}

func ciAttest(ctx context.Context, git cievidence.Git, dir, base, keyPath string, unsigned bool, out, predicateOut string, getenv func(string) string, stdout, stderr io.Writer) int {
	head, err := git.Head()
	if err != nil {
		fmt.Fprintf(stderr, "tellproof-agent: ci needs a git checkout: %v\n", err)
		return 2
	}
	gates, canaries, err := cievidence.Load(dir, head)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(gates)+len(canaries) == 0 {
		fmt.Fprintf(stderr, "tellproof-agent: no gate runs are recorded for %s in %s (run gates with: tellproof-agent ci run --gate <name> -- <command>)\n", head, dir)
		return 1
	}
	agents, err := git.AgentCommitsIn(base, head)
	if err != nil {
		fmt.Fprintf(stderr, "tellproof-agent: %v (is the history fetched? use fetch-depth: 0)\n", err)
		return 1
	}
	repo, ref, run := ciContext(git, getenv)
	st := cievidence.NewStatement(repo, head, ref, run, gates, canaries, agents, "tellproof-agent "+version, time.Now())
	var b []byte
	if unsigned {
		b, err = cievidence.Payload(st)
	} else {
		var pemKey []byte
		switch {
		case keyPath != "":
			pemKey, err = os.ReadFile(keyPath)
		case getenv("TELLPROOF_CI_KEY") != "":
			pemKey = []byte(getenv("TELLPROOF_CI_KEY"))
		default:
			err = errors.New("no signing key: pass --key, set TELLPROOF_CI_KEY, or use --unsigned and sign with GitHub artifact attestations")
		}
		if err == nil {
			priv, perr := cievidence.ParsePrivate(pemKey)
			if perr != nil {
				err = perr
			} else if env, serr := cievidence.Sign(ctx, st, priv); serr != nil {
				err = serr
			} else {
				b, err = json.MarshalIndent(env, "", "  ")
			}
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "tellproof-agent: %v\n", err)
		return 2
	}
	b = append(b, '\n')
	if predicateOut != "" {
		pb, perr := json.MarshalIndent(st.Predicate, "", "  ")
		if perr == nil {
			perr = os.WriteFile(predicateOut, append(pb, '\n'), 0o644)
		}
		if perr != nil {
			fmt.Fprintln(stderr, perr)
			return 1
		}
	}
	if out == "" {
		_, err = stdout.Write(b)
	} else {
		err = os.WriteFile(out, b, 0o644)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stderr, "tellproof-agent: CI evidence for %s: %d gates, %d canaries, %d of %d commits name a coding agent.\n",
		head[:12], len(gates), len(canaries), agents.WithAgent, agents.Commits)
	return 0
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func ciVerify(git cievidence.Git, path, policyPath, require, canary, commit, pubPath string, unsigned, asJSON bool, getenv func(string) string, stdout, stderr io.Writer) int {
	req := cievidence.Requirements{Commit: commit, Required: splitList(require), Canaries: splitList(canary)}
	if len(req.Required)+len(req.Canaries) == 0 {
		if policyPath == "" {
			policyPath = policy.Find(getenv, "")
		}
		p, err := policy.Load(policyPath)
		if err != nil || p.CI == nil {
			fmt.Fprintf(stderr, "tellproof-agent: nothing to require: add a \"ci\" section to %s, or pass --require and --canary\n", policyPath)
			return 2
		}
		req.Required, req.Canaries = p.CI.RequiredGates, p.CI.CanaryGates
	}
	if req.Commit == "" {
		req.Commit, _ = git.Head()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var pub []byte
	if pubPath != "" {
		if pub, err = os.ReadFile(pubPath); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	return verifyCIStatement(b, pub, unsigned, req, asJSON, "tellproof-agent", stdout, stderr)
}

// verifyCIStatement opens, checks and reports a statement.
func verifyCIStatement(b, pubPEM []byte, unsigned bool, req cievidence.Requirements, asJSON bool, prog string, stdout, stderr io.Writer) int {
	code, err := cievidence.VerifyAndReport(stdout, b, pubPEM, unsigned, req, asJSON)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
	}
	return code
}

func ciKeygen(out string, stdout, stderr io.Writer) int {
	priv, pub, err := cievidence.NewKey()
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
			_, err = f.Write(priv)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err == nil {
		err = os.WriteFile(out+".pub", pub, 0o644)
	}
	if err != nil {
		fmt.Fprintf(stderr, "tellproof-agent: %v\n", err)
		return 1
	}
	k, _ := cievidence.ParsePublic(pub)
	fmt.Fprintf(stdout, "Wrote the signing key to %s (keep it secret: store it as the CI secret TELLPROOF_CI_KEY) and the public key to %s.pub (key ID %s):\n%s",
		out, out, cievidence.KeyID(k), pub)
	return 0
}
