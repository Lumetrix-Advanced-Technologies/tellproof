package main

// Governance as code (D-055): `policy init` writes tellproof.policy.json from
// what the log has seen, `check` evaluates the local log against the evidence
// objectives with the hosted packs' evaluator, and `aibom` writes a CycloneDX
// AI bill of materials.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/agent"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/aibom"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/evaluate"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/localcheck"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/localreport"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/policy"
)

const governanceUsage = `usage:
  tellproof-agent policy init [--policy <file>] [--data-dir <dir>] [--name <ai system>] [--timezone <tz>] [--force]
  tellproof-agent policy validate [--policy <file>]
  tellproof-agent check [--policy <file>] [--data-dir <dir>] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--json] [--junit <file>] [--strict]
  tellproof-agent check --self-test [--policy <file>] [--data-dir <dir>] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--json]
  tellproof-agent aibom [--policy <file>] [--data-dir <dir>] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--out <file>]

The policy file is --policy, else $TELLPROOF_POLICY, else tellproof.policy.json in
the working directory, else in the data directory.
check exit status: 0 no objective failed (with --strict: none needs attention
either), 1 one did or the log failed verification, 2 usage or policy error.
check --self-test adds a violation of each objective to a copy of the evidence
and requires it to fail, and tampers with a copy of the log and requires
verification to refuse it; exit 1 if an objective cannot fail, is vacuous (a
register leaves no room for a violation) or tampering goes unnoticed.
`

// isGovernance reports whether args start a governance command.
func isGovernance(args []string) bool {
	return len(args) > 0 && (args[0] == "check" || args[0] == "policy" || args[0] == "aibom")
}

func governance(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := args[0]
	args = args[1:]
	if cmd == "policy" {
		if len(args) == 0 || (args[0] != "init" && args[0] != "validate") {
			fmt.Fprint(stderr, governanceUsage)
			return 2
		}
		cmd, args = "policy "+args[0], args[1:]
	}
	fs := flag.NewFlagSet("tellproof-agent "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "agent config file (for the data directory)")
	dataDir := fs.String("data-dir", "", "the data directory (default from the config)")
	policyPath := fs.String("policy", "", "the policy file")
	since := fs.String("since", "", "first local day, YYYY-MM-DD")
	until := fs.String("until", "", "last local day, YYYY-MM-DD")
	asJSON := fs.Bool("json", false, "check: print JSON")
	junit := fs.String("junit", "", "check: also write JUnit XML to this file")
	strict := fs.Bool("strict", false, "check: attention fails too")
	selfTest := fs.Bool("self-test", false, "check: test that each objective can fail and that tampering is detected")
	name := fs.String("name", "", "policy init: the AI system's name")
	tz := fs.String("timezone", "", "policy init: IANA time zone (default $TZ, else UTC)")
	force := fs.Bool("force", false, "policy init: replace an existing policy file")
	out := fs.String("out", "", "aibom: write to this file instead of standard output")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(stderr, governanceUsage)
		return 2
	}
	if *dataDir == "" {
		cfg, err := agent.LoadConfig(*configPath, getenv)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		*dataDir = cfg.DataDir
	}
	if *policyPath == "" {
		*policyPath = policy.Find(getenv, *dataDir)
	}
	switch cmd {
	case "policy validate":
		p, err := policy.Load(*policyPath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s is valid: %s, %d models, %d regions, %d changes, %d incidents, %d evaluation runs declared.\n",
			*policyPath, p.AISystem, len(p.DeclaredModels), len(p.DeclaredRegions), len(p.Changes), len(p.Incidents), len(p.Evals))
		return 0
	case "policy init":
		return policyInit(*policyPath, *dataDir, *name, *tz, *force, getenv, stdout, stderr)
	case "check":
		pol, err := policy.Load(*policyPath)
		if err != nil {
			fmt.Fprintf(stderr, "%v\n(create one with: tellproof-agent policy init)\n", err)
			return 2
		}
		if *selfTest {
			if *junit != "" || *strict {
				fmt.Fprint(stderr, governanceUsage)
				return 2
			}
			return checkSelfTest(localcheck.Options{DataDir: *dataDir, Policy: pol, Since: *since, Until: *until}, *asJSON, stdout, stderr)
		}
		r, err := localcheck.Run(localcheck.Options{DataDir: *dataDir, Policy: pol, Since: *since, Until: *until})
		switch {
		case errors.Is(err, localcheck.ErrUnverified):
			fmt.Fprintf(stderr, "tellproof-agent: %v. Nothing was evaluated.\n", err)
			return 1
		case err != nil:
			fmt.Fprintln(stderr, err)
			return 2
		}
		if *asJSON {
			err = localcheck.WriteJSON(stdout, r)
		} else {
			err = localcheck.WriteText(stdout, r)
		}
		if err == nil && *junit != "" {
			var f *os.File
			if f, err = os.Create(*junit); err == nil {
				err = localcheck.WriteJUnit(f, r, *strict)
				if cerr := f.Close(); err == nil {
					err = cerr
				}
			}
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if r.Failed(*strict) {
			return 1
		}
		return 0
	case "aibom":
		var pol *policy.Policy
		if p, err := policy.Load(*policyPath); err == nil {
			pol = p
		} else if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stderr, err)
			return 2
		}
		sum, bs, err := localreport.ReadBatches(*dataDir, localreport.Range{})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if !sum.Verified {
			fmt.Fprintf(stderr, "tellproof-agent: the local evidence log failed verification: %s. No AI-BOM written.\n", sum.Problem)
			return 1
		}
		o := aibom.Options{Policy: pol, AgentID: sum.AgentID, LastDigest: sum.LastHash, Version: version}
		loc := time.UTC
		if pol != nil {
			loc, _ = time.LoadLocation(pol.Timezone)
		}
		for _, d := range []struct {
			in  string
			out *time.Time
			add int
		}{{*since, &o.Since, 0}, {*until, &o.Until, 1}} {
			if d.in == "" {
				continue
			}
			t, err := time.ParseInLocation("2006-01-02", d.in, loc)
			if err != nil {
				fmt.Fprintf(stderr, "tellproof-agent: %q is not a date (YYYY-MM-DD)\n", d.in)
				return 2
			}
			*d.out = t.AddDate(0, 0, d.add)
		}
		w := stdout
		if *out != "" {
			f, err := os.Create(*out)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			defer f.Close()
			w = f
		}
		if err := aibom.Write(w, aibom.Build(bs, o)); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if *out != "" {
			fmt.Fprintf(stdout, "AI-BOM (CycloneDX 1.6) written to %s\n", *out)
		}
		return 0
	}
	return 2
}

// policyInit writes a policy declaring what the log has seen, for a person to
// review. It never fills the registers: a change or incident is something a
// person records, not something the agent can attest for them.
func checkSelfTest(o localcheck.Options, asJSON bool, stdout, stderr io.Writer) int {
	r, err := localcheck.SelfTest(o)
	switch {
	case errors.Is(err, localcheck.ErrUnverified):
		fmt.Fprintf(stderr, "tellproof-agent: %v. Nothing was tested.\n", err)
		return 1
	case err != nil:
		fmt.Fprintln(stderr, err)
		return 2
	}
	if asJSON {
		err = localcheck.WriteSelfTestJSON(stdout, r)
	} else {
		err = localcheck.WriteSelfTestText(stdout, r)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if r.Failed() {
		return 1
	}
	return 0
}

func policyInit(path, dataDir, name, tz string, force bool, getenv func(string) string, stdout, stderr io.Writer) int {
	if _, err := os.Stat(path); err == nil && !force {
		fmt.Fprintf(stderr, "tellproof-agent: %s exists; edit it, or pass --force to replace it\n", path)
		return 2
	}
	if tz == "" {
		tz = getenv("TZ")
	}
	if _, err := time.LoadLocation(tz); tz == "" || err != nil {
		tz = "UTC"
	}
	if name == "" {
		name = "My AI system"
	}
	p := &policy.Policy{Schema: policy.SchemaURL, AISystem: name, Environment: "production", Timezone: tz,
		DeclaredModels: []evaluate.DeclaredModel{}, DeclaredRegions: []string{}, Changes: []evaluate.Change{},
		Incidents: []evaluate.Incident{}, Evals: nil}
	sum, bs, err := localreport.ReadBatches(dataDir, localreport.Range{})
	switch {
	case err != nil:
		fmt.Fprintf(stdout, "No local evidence log in %s yet: writing an empty policy. Run `tellproof-agent policy init --force` once the agent has seen traffic, or declare models and regions by hand.\n", dataDir)
	case !sum.Verified:
		fmt.Fprintf(stderr, "tellproof-agent: the local evidence log failed verification: %s\n", sum.Problem)
		return 1
	}
	models := map[evaluate.DeclaredModel]bool{}
	regions := map[string]bool{}
	var transitions int
	for _, b := range bs {
		for _, t := range b.Metrics.Tokens {
			models[evaluate.DeclaredModel{Provider: t.Provider, Model: t.Model}] = true
			if t.Region != "" && t.Region != "unknown" {
				regions[t.Region] = true
			}
		}
		transitions += len(b.Metrics.Transitions)
	}
	for m := range models {
		p.DeclaredModels = append(p.DeclaredModels, m)
	}
	sort.Slice(p.DeclaredModels, func(i, j int) bool {
		a, b := p.DeclaredModels[i], p.DeclaredModels[j]
		return a.Provider+"/"+a.Model < b.Provider+"/"+b.Model
	})
	for r := range regions {
		p.DeclaredRegions = append(p.DeclaredRegions, r)
	}
	sort.Strings(p.DeclaredRegions)
	if err := p.Write(path); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "Wrote %s: %d models and %d regions as seen in the log.\n", path, len(p.DeclaredModels), len(p.DeclaredRegions))
	fmt.Fprintln(stdout, "Review it before you rely on it: a declaration says what the system may use, so remove anything that should not be there.")
	if transitions > 0 {
		fmt.Fprintf(stdout, "The log shows %d model or prompt change(s); record each one you approve under \"changes\" (tellproof-agent check lists any that are missing).\n", transitions)
	}
	fmt.Fprintln(stdout, "Then: tellproof-agent check")
	return 0
}
