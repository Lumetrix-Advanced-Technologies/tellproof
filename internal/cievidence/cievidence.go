// Package cievidence is evidence of what a codebase's CI did on a commit
// (D-058), for code written by people and coding agents alike: which gates
// ran (tests, lint, scans), how each ended, how many tests ran, whether a
// must-fail canary showed a gate can fail, and which commits name an AI agent
// as an author. It keeps no logs, no code and no command lines (only their
// digests), and writes one in-toto statement per commit, signed with an
// Ed25519 key in a DSSE envelope or left for GitHub artifact attestations to
// sign. Verify checks a statement against a policy's required gates.
package cievidence

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Identifiers. The predicate type uses the placeholder domain until D-008.
const (
	Schema        = "tellproof-ci/v0.1"
	PredicateType = "https://lumetrix.example/tellproof/ci/v0.1"
	StatementType = "https://in-toto.io/Statement/v1"
)

// GateRE is a gate's name (as in the policy file).
var GateRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

var commitRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// JUnit is a test report's counts.
type JUnit struct {
	Tests    int `json:"tests"`
	Failures int `json:"failures"`
	Errors   int `json:"errors"`
	Skipped  int `json:"skipped"`
}

// Gate is one run of a gate on a commit. A canary is a must-fail run: the
// gate given known-bad input, which passes (is caught) when the gate fails.
type Gate struct {
	Name          string `json:"name"`
	Canary        bool   `json:"canary"`
	CommandDigest string `json:"command_digest"`         // sha256 of the command line; the command is never kept
	PatchDigest   string `json:"patch_digest,omitempty"` // canary: sha256 of the patch that broke the code
	ExitCode      int    `json:"exit_code"`
	Passed        bool   `json:"passed"` // a gate: exit 0; a canary: exit not 0 (caught)
	Started       string `json:"started"`
	DurationMS    int64  `json:"duration_ms"`
	JUnit         *JUnit `json:"junit,omitempty"`
	Attempts      int    `json:"attempts,omitempty"` // in a statement: how many runs were recorded; the latest is shown
}

// Run is the CI run that made the statement.
type Run struct {
	Provider string `json:"provider"` // github | gitlab | local
	ID       string `json:"id,omitempty"`
	Attempt  string `json:"attempt,omitempty"`
	URL      string `json:"url,omitempty"`
}

// AgentCommits counts the commits in a range that name an AI coding agent
// (an author or co-author trailer, or a generated-with line). Only agent
// names and counts are kept, never a message.
type AgentCommits struct {
	Range     string         `json:"range"`
	Commits   int            `json:"commits"`
	WithAgent int            `json:"with_agent"`
	ByAgent   map[string]int `json:"by_agent"`
}

// Predicate is the statement's predicate.
type Predicate struct {
	CISchema     string        `json:"ci_schema"`
	Repository   string        `json:"repository"`
	Commit       string        `json:"commit"`
	Ref          string        `json:"ref,omitempty"`
	Run          Run           `json:"run"`
	Gates        []Gate        `json:"gates"`
	Canaries     []Gate        `json:"canaries"`
	AgentCommits *AgentCommits `json:"agent_commits,omitempty"`
	GeneratedAt  string        `json:"generated_at"`
	Generator    string        `json:"generator"`
}

// Subject is an in-toto subject.
type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

// Statement is the in-toto statement.
type Statement struct {
	Type          string    `json:"_type"`
	Subject       []Subject `json:"subject"`
	PredicateType string    `json:"predicateType"`
	Predicate     Predicate `json:"predicate"`
}

// CommandDigest is "sha256:" + SHA-256 of the arguments joined by NUL: it
// shows two runs used the same command without keeping a command line, which
// may hold a token.
func CommandDigest(args []string) string {
	sum := sha256.Sum256([]byte(strings.Join(args, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// FileDigest is "sha256:" + SHA-256 of a file.
func FileDigest(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Save stores a gate run for a commit under dir/<commit>/.
func Save(dir, commit string, g Gate) (string, error) {
	if !commitRE.MatchString(commit) {
		return "", fmt.Errorf("cievidence: %q is not a full commit hash", commit)
	}
	if !GateRE.MatchString(g.Name) {
		return "", fmt.Errorf("cievidence: %q is not a gate name (lower-case letters, digits, '.', '_' or '-', at most 64)", g.Name)
	}
	d := filepath.Join(dir, commit)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	kind := "gate"
	if g.Canary {
		kind = "canary"
	}
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(d, fmt.Sprintf("%s.%s.%d.json", g.Name, kind, time.Now().UnixNano()))
	return path, os.WriteFile(path, append(b, '\n'), 0o600)
}

// Load reads every run recorded for commit and keeps the latest per gate
// (gates and canaries apart), with the number of runs as Attempts.
func Load(dir, commit string) (gates, canaries []Gate, err error) {
	files, err := filepath.Glob(filepath.Join(dir, commit, "*.json"))
	if err != nil {
		return nil, nil, err
	}
	type key struct {
		name   string
		canary bool
	}
	latest := map[key]Gate{}
	count := map[key]int{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, err
		}
		var g Gate
		if err := json.Unmarshal(b, &g); err != nil {
			return nil, nil, fmt.Errorf("cievidence: %s: %w", f, err)
		}
		k := key{g.Name, g.Canary}
		count[k]++
		if old, ok := latest[k]; !ok || g.Started >= old.Started {
			latest[k] = g
		}
	}
	for k, g := range latest {
		g.Attempts = count[k]
		if k.canary {
			canaries = append(canaries, g)
		} else {
			gates = append(gates, g)
		}
	}
	byName := func(a, b Gate) int { return cmp.Compare(a.Name, b.Name) }
	slices.SortFunc(gates, byName)
	slices.SortFunc(canaries, byName)
	return gates, canaries, nil
}

// NewStatement makes the statement for a commit.
func NewStatement(repo, commit, ref string, run Run, gates, canaries []Gate, agents *AgentCommits, generator string, now time.Time) *Statement {
	if gates == nil {
		gates = []Gate{}
	}
	if canaries == nil {
		canaries = []Gate{}
	}
	return &Statement{Type: StatementType, PredicateType: PredicateType,
		Subject: []Subject{{Name: repo, Digest: map[string]string{"gitCommit": commit}}},
		Predicate: Predicate{CISchema: Schema, Repository: repo, Commit: commit, Ref: ref, Run: run, Gates: gates, Canaries: canaries,
			AgentCommits: agents, GeneratedAt: now.UTC().Format(time.RFC3339), Generator: generator}}
}

// ErrInvalid is a statement that is not a Tellproof CI statement.
var ErrInvalid = errors.New("cievidence: not a Tellproof CI statement")

// Decode reads a statement strictly.
func Decode(b []byte) (*Statement, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var s Statement
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	p := s.Predicate
	switch {
	case s.Type != StatementType || s.PredicateType != PredicateType || p.CISchema != Schema:
		return nil, fmt.Errorf("%w: type %q, predicate %q, schema %q", ErrInvalid, s.Type, s.PredicateType, p.CISchema)
	case !commitRE.MatchString(p.Commit) || len(s.Subject) != 1 || s.Subject[0].Digest["gitCommit"] != p.Commit:
		return nil, fmt.Errorf("%w: the subject must be the commit", ErrInvalid)
	}
	return &s, nil
}

// Requirements is what a commit must show.
type Requirements struct {
	Commit   string   // empty: any
	Required []string // gates that must have run and passed
	Canaries []string // gates whose canary must have been caught
}

// Check is one requirement's outcome.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Note   bool   `json:"note,omitempty"` // worth knowing, not required: never fails verification
	Detail string `json:"detail"`
}

// Verify checks a decoded statement against the requirements. Signature
// checks are the caller's (VerifyEnvelope).
func Verify(s *Statement, req Requirements) []Check {
	p := s.Predicate
	var out []Check
	add := func(name string, ok bool, format string, a ...any) {
		out = append(out, Check{Name: name, OK: ok, Detail: fmt.Sprintf(format, a...)})
	}
	if req.Commit != "" {
		add("commit", strings.HasPrefix(p.Commit, req.Commit) && len(req.Commit) >= 7, "statement is for %s, expected %s", p.Commit, req.Commit)
	}
	find := func(list []Gate, name string) *Gate {
		for i := range list {
			if list[i].Name == name {
				return &list[i]
			}
		}
		return nil
	}
	for _, name := range req.Required {
		g := find(p.Gates, name)
		switch {
		case g == nil:
			add("gate "+name, false, "did not run on this commit")
		case !g.Passed:
			add("gate "+name, false, "failed (exit %d)", g.ExitCode)
		case g.JUnit != nil && g.JUnit.Tests == 0:
			add("gate "+name, false, "passed but ran no tests: a gate that tests nothing cannot fail")
		case g.JUnit != nil && g.JUnit.Failures+g.JUnit.Errors > 0:
			add("gate "+name, false, "exited 0 with %d failing tests in its report", g.JUnit.Failures+g.JUnit.Errors)
		default:
			add("gate "+name, true, "passed%s%s", junitText(g.JUnit), attemptsText(g.Attempts))
		}
	}
	for _, name := range req.Canaries {
		c := find(p.Canaries, name)
		switch {
		case c == nil:
			add("canary "+name, false, "no must-fail canary ran for this gate, so nothing shows it can fail")
		case !c.Passed:
			add("canary "+name, false, "the gate passed on known-bad input: it cannot tell broken code from working code")
		default:
			add("canary "+name, true, "caught: the gate failed on known-bad input (exit %d)", c.ExitCode)
		}
	}
	// Anything else that ran and failed is noted, though not required.
	note := func(name, format string, a ...any) {
		out = append(out, Check{Name: name, OK: true, Note: true, Detail: fmt.Sprintf(format, a...)})
	}
	for _, g := range p.Gates {
		if !slices.Contains(req.Required, g.Name) && !g.Passed {
			note("gate "+g.Name, "failed (exit %d); not required", g.ExitCode)
		}
	}
	for _, c := range p.Canaries {
		if !slices.Contains(req.Canaries, c.Name) && !c.Passed {
			note("canary "+c.Name, "missed: the gate passed on known-bad input; not required")
		}
	}
	return out
}

// Failed reports whether any check failed.
func Failed(cs []Check) bool {
	return slices.ContainsFunc(cs, func(c Check) bool { return !c.OK })
}

func junitText(j *JUnit) string {
	if j == nil {
		return ""
	}
	return fmt.Sprintf(": %d tests, %d skipped", j.Tests, j.Skipped)
}

func attemptsText(n int) string {
	if n > 1 {
		return " (after " + strconv.Itoa(n) + " runs)"
	}
	return ""
}
