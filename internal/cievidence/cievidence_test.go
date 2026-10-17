package cievidence

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

func TestRepoNameDropsCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"https://x-access-token:ghs_SECRET@github.com/acme/app.git": "git+https://github.com/acme/app",
		"https://user:pass@gitlab.example:8443/group/app":           "git+https://gitlab.example:8443/group/app",
		"git@github.com:acme/app.git":                               "git+https://github.com/acme/app",
		"ssh://git@github.com:22/acme/app.git":                      "git+https://github.com/acme/app",
		"":                                                          "",
	} {
		if got := RepoName(in); got != want || strings.Contains(got, "SECRET") || strings.Contains(got, "pass") {
			t.Errorf("RepoName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgents(t *testing.T) {
	cases := []struct {
		author, msg string
		want        string
	}{
		{"Ada <ada@example.com>", "Fix parser\n\nCo-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>", "claude"},
		{"Copilot <198982749+Copilot@users.noreply.github.com>", "Add retries", "copilot"},
		{"Ada <ada@example.com>", "aider: refactor the loader", "aider"},
		{"Ada <ada@example.com>", "Update\n\nCo-authored-by: Cursor Agent <cursoragent@cursor.com>", "cursor"},
		{"devin-ai-integration[bot] <x@users.noreply.github.com>", "Bump", "devin"},
		{"Ada <ada@example.com>", "Plain human commit mentioning claude in passing", ""},
		{"Ada <ada@example.com>", "Both\n\nCo-authored-by: Copilot <c@x>\n🤖 Generated with [Claude Code](https://claude.com/claude-code)", "claude,copilot"},
	}
	for _, c := range cases {
		if got := strings.Join(Agents(c.author, c.msg), ","); got != c.want {
			t.Errorf("%q: %q, want %q", c.msg, got, c.want)
		}
	}
}

func TestReadJUnit(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, []byte(body), 0o600)
		return p
	}
	// Summary attributes are ignored; the cases are counted.
	j, err := ReadJUnit(write("a.xml", `<testsuites tests="99"><testsuite name="a"><testcase name="1"/><testcase name="2"><failure message="x"/></testcase>
	  <testsuite name="nested"><testcase name="3"><skipped/></testcase><testcase name="4"><error/></testcase></testsuite></testsuite></testsuites>`))
	if err != nil || *j != (JUnit{Tests: 4, Failures: 1, Errors: 1, Skipped: 1}) {
		t.Errorf("%v %+v", err, j)
	}
	if j, err := ReadJUnit(write("b.xml", `<testsuite name="b"><testcase name="1"/></testsuite>`)); err != nil || j.Tests != 1 {
		t.Errorf("single suite: %v %+v", err, j)
	}
	if _, err := ReadJUnit(write("c.xml", `not xml`)); err == nil {
		t.Error("garbage accepted")
	}
}

func TestSaveLoadKeepsTheLatestAndCountsRuns(t *testing.T) {
	dir := t.TempDir()
	at := func(m int) string { return time.Date(2026, 10, 17, 1, m, 0, 0, time.UTC).Format(time.RFC3339) }
	for _, g := range []Gate{
		{Name: "tests", Passed: false, ExitCode: 1, Started: at(1)},
		{Name: "tests", Passed: true, Started: at(2), JUnit: &JUnit{Tests: 3}},
		{Name: "lint", Passed: true, Started: at(1)},
		{Name: "tests", Canary: true, Passed: true, ExitCode: 1, Started: at(3)},
	} {
		if _, err := Save(dir, commit, g); err != nil {
			t.Fatal(err)
		}
	}
	gates, canaries, err := Load(dir, commit)
	if err != nil || len(gates) != 2 || len(canaries) != 1 {
		t.Fatalf("%v %+v %+v", err, gates, canaries)
	}
	if gates[1].Name != "tests" || !gates[1].Passed || gates[1].Attempts != 2 || gates[0].Attempts != 1 || canaries[0].Attempts != 1 {
		t.Errorf("gates %+v", gates)
	}
	if _, err := Save(dir, "abc", Gate{Name: "tests"}); err == nil {
		t.Error("a short commit was accepted")
	}
	if _, err := Save(dir, commit, Gate{Name: "Bad Name"}); err == nil {
		t.Error("a bad gate name was accepted")
	}
}

func statement() *Statement {
	return NewStatement("git+https://github.com/acme/app", commit, "refs/heads/main", Run{Provider: "github", ID: "42"},
		[]Gate{{Name: "lint", Passed: true}, {Name: "tests", Passed: true, JUnit: &JUnit{Tests: 12, Skipped: 1}, Attempts: 2}},
		[]Gate{{Name: "tests", Canary: true, Passed: true, ExitCode: 1}},
		&AgentCommits{Range: commit, Commits: 1, WithAgent: 1, ByAgent: map[string]int{"claude": 1}}, "tellproof-agent test", time.Unix(0, 0))
}

func TestSignOpenAndTamper(t *testing.T) {
	privPEM, pubPEM, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ParsePrivate(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublic(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	env, err := Sign(context.Background(), statement(), priv)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	s, signed, err := Open(b, pub, false)
	if err != nil || !signed || s.Predicate.Commit != commit {
		t.Fatalf("open: %v %v", err, signed)
	}
	// Edited payload, other key, no key, bare statement without --unsigned.
	env.Payload = []byte(strings.Replace(string(env.Payload), `"passed":true`, `"passed":false`, 1))
	tampered, _ := json.Marshal(env)
	if _, _, err := Open(tampered, pub, false); err == nil {
		t.Error("an edited statement verified")
	}
	_, otherPub, _ := NewKey()
	op, _ := ParsePublic(otherPub)
	if _, _, err := Open(b, op, false); err == nil {
		t.Error("another key verified")
	}
	if _, _, err := Open(b, nil, false); err == nil {
		t.Error("a signed statement opened without a key")
	}
	bare, _ := Payload(statement())
	if _, _, err := Open(bare, nil, false); err == nil {
		t.Error("an unsigned statement opened without --unsigned")
	}
	if s, signed, err := Open(bare, nil, true); err != nil || signed || s == nil {
		t.Errorf("unsigned: %v", err)
	}
	if _, err := Decode([]byte(strings.Replace(string(bare), Schema, "other/v9", 1))); err == nil {
		t.Error("an unknown schema was accepted")
	}
}

func TestVerifyRequirements(t *testing.T) {
	s := statement()
	cs := Verify(s, Requirements{Commit: commit[:12], Required: []string{"lint", "tests"}, Canaries: []string{"tests"}})
	if Failed(cs) || len(cs) != 4 || !strings.Contains(cs[2].Detail, "12 tests, 1 skipped (after 2 runs)") {
		t.Fatalf("passing: %+v", cs)
	}
	bad := func(name string, edit func(p *Predicate), req Requirements, want string) {
		t.Helper()
		s := statement()
		edit(&s.Predicate)
		for _, c := range Verify(s, req) {
			if !c.OK && strings.Contains(c.Detail, want) {
				return
			}
		}
		t.Errorf("%s: no failing check saying %q in %+v", name, want, Verify(s, req))
	}
	none := func(*Predicate) {}
	bad("other commit", none, Requirements{Commit: "fedcba9"}, "expected fedcba9")
	bad("missing gate", none, Requirements{Required: []string{"scan"}}, "did not run")
	bad("failed gate", func(p *Predicate) { p.Gates[0].Passed, p.Gates[0].ExitCode = false, 2 }, Requirements{Required: []string{"lint"}}, "failed (exit 2)")
	bad("no tests", func(p *Predicate) { p.Gates[1].JUnit.Tests = 0 }, Requirements{Required: []string{"tests"}}, "ran no tests")
	bad("lying exit", func(p *Predicate) { p.Gates[1].JUnit.Failures = 3 }, Requirements{Required: []string{"tests"}}, "exited 0 with 3 failing tests")
	bad("no canary", none, Requirements{Canaries: []string{"lint"}}, "no must-fail canary")
	bad("blind gate", func(p *Predicate) { p.Canaries[0].Passed = false }, Requirements{Canaries: []string{"tests"}}, "passed on known-bad input")
	s = statement()
	s.Predicate.Gates[0].Passed = false
	s.Predicate.Canaries[0].Passed = false
	if cs := Verify(s, Requirements{Required: []string{"tests"}}); Failed(cs) || len(cs) != 3 || !cs[1].Note || !cs[2].Note {
		t.Errorf("unrequired failures are notes: %+v", cs)
	}
}

func TestGitContextAndAgentCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	g := Git{Dir: dir}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("remote", "add", "origin", "https://x-access-token:ghs_SECRET@github.com/acme/app.git")
	run("commit", "-q", "--allow-empty", "-m", "base")
	base, _ := g.Head()
	run("commit", "-q", "--allow-empty", "-m", "Human change")
	run("commit", "-q", "--allow-empty", "-m", "Agent change\n\nCo-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>")
	head, err := g.Head()
	if err != nil || len(head) != 40 {
		t.Fatal(err)
	}
	if g.Ref() != "refs/heads/main" || g.Remote() != "git+https://github.com/acme/app" {
		t.Errorf("ref %q remote %q", g.Ref(), g.Remote())
	}
	a, err := g.AgentCommitsIn(base, head)
	if err != nil || a.Commits != 2 || a.WithAgent != 1 || a.ByAgent["claude"] != 1 || a.Range != base+".."+head {
		t.Errorf("%v %+v", err, a)
	}
	if one, _ := g.AgentCommitsIn("", head); one.Commits != 1 || one.WithAgent != 1 {
		t.Errorf("head only %+v", one)
	}
}
