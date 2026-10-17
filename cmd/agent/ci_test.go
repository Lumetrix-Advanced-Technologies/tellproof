package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo makes a repository with one file the "gate" checks, and a patch
// that breaks it.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
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
	_ = os.WriteFile(filepath.Join(dir, "app.txt"), []byte("status: good\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".tellproof-ci/\n*.pem\n*.pem.pub\nreport.xml\nci.json\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "app\n\nCo-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>")
	_ = os.WriteFile(filepath.Join(dir, "break.patch"), []byte("--- a/app.txt\n+++ b/app.txt\n@@ -1 +1 @@\n-status: good\n+status: broken\n"), 0o644)
	return dir
}

func agentCmd(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, func(k string) string { return env[k] }, &out, &errb)
	return code, out.String(), errb.String()
}

// Acceptance (P7-3): gates and a canary recorded on a commit become a signed
// statement that ci verify accepts against the policy; a skipped gate, a
// blind canary or an edited statement fail it.
func TestCIEvidenceEndToEnd(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	env := map[string]string{}
	gateCmd := []string{"grep", "-q", "good", "app.txt"}

	if code, _, errs := agentCmd(t, env, append([]string{"ci", "run", "--gate", "lint", "--"}, gateCmd...)...); code != 0 {
		t.Fatalf("ci run lint: %d %s", code, errs)
	}
	junit := `<testsuite><testcase name="a"/><testcase name="b"/><testcase name="c"><skipped/></testcase></testsuite>`
	if code, _, errs := agentCmd(t, env, "ci", "run", "--gate", "tests", "--junit", "report.xml", "--", "sh", "-c", "printf '"+junit+"' > report.xml"); code != 0 {
		t.Fatalf("ci run tests: %d %s", code, errs)
	}
	// The gate's own exit status passes through.
	if code, _, _ := agentCmd(t, env, "ci", "run", "--gate", "scan", "--", "sh", "-c", "exit 3"); code != 3 {
		t.Errorf("exit status not passed through: %d", code)
	}
	// The canary breaks app.txt in a temporary worktree: the gate fails, so it is caught.
	code, _, errs := agentCmd(t, env, append([]string{"ci", "canary", "--gate", "lint", "--patch", "break.patch", "--"}, gateCmd...)...)
	if code != 0 || !strings.Contains(errs, "caught") {
		t.Fatalf("canary: %d %s", code, errs)
	}
	if b, _ := os.ReadFile("app.txt"); string(b) != "status: good\n" {
		t.Fatal("the canary changed the checkout")
	}
	// A canary on a gate that cannot see the breakage is missed.
	if code, _, errs := agentCmd(t, env, "ci", "canary", "--gate", "tests", "--patch", "break.patch", "--", "true"); code != 1 || !strings.Contains(errs, "MISSED") {
		t.Errorf("blind canary: %d %s", code, errs)
	}

	if code, out, _ := agentCmd(t, env, "ci", "keygen", "--out", "ci.pem"); code != 0 || !strings.Contains(out, "PUBLIC KEY") {
		t.Fatalf("keygen: %d %s", code, out)
	}
	if code, _, errs := agentCmd(t, env, "ci", "attest", "--key", "ci.pem", "--out", "ci.json"); code != 0 || !strings.Contains(errs, "1 of 1 commits name a coding agent") {
		t.Fatalf("attest: %d %s", code, errs)
	}
	raw, _ := os.ReadFile("ci.json")
	if bytes.Contains(raw, []byte("SECRET")) || bytes.Contains(raw, []byte("grep")) || bytes.Contains(raw, []byte("status: good")) {
		t.Fatal("the statement carries a token, a command line or file content")
	}

	pol := `{"ai_system": "app", "ci": {"required_gates": ["lint", "tests"], "canary_gates": ["lint"]}}`
	_ = os.WriteFile("tellproof.policy.json", []byte(pol), 0o644)
	code, out, errs := agentCmd(t, env, "ci", "verify", "--pub", "ci.pem.pub", "ci.json")
	if code != 0 || !strings.Contains(out, "signature verified") || !strings.Contains(out, "3 tests, 1 skipped") || !strings.Contains(out, "claude 1") {
		t.Fatalf("verify: %d %s %s", code, out, errs)
	}
	// Requirements the evidence does not meet.
	for name, args := range map[string][]string{
		"scan failed":    {"--require", "scan"},
		"tests canary":   {"--canary", "tests"},
		"no such gate":   {"--require", "e2e"},
		"another commit": {"--require", "lint", "--commit", strings.Repeat("a", 40)},
	} {
		if code, out, _ := agentCmd(t, env, append(append([]string{"ci", "verify", "--pub", "ci.pem.pub"}, args...), "ci.json")...); code != 1 {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	// An edited statement.
	var envl map[string]any
	_ = json.Unmarshal(raw, &envl)
	envl["payload"] = strings.Replace(envl["payload"].(string), "A", "B", 1)
	edited, _ := json.Marshal(envl)
	_ = os.WriteFile("edited.json", edited, 0o644)
	if code, _, _ := agentCmd(t, env, "ci", "verify", "--pub", "ci.pem.pub", "edited.json"); code != 1 {
		t.Error("an edited statement verified")
	}
	// Unsigned, for GitHub attestations; the run context comes from GitHub's variables.
	gh := map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "acme/app", "GITHUB_RUN_ID": "77", "GITHUB_REF": "refs/pull/9/merge"}
	if code, _, errs := agentCmd(t, gh, "ci", "attest", "--unsigned", "--out", "bare.json", "--predicate", "predicate.json"); code != 0 {
		t.Fatalf("unsigned: %d %s", code, errs)
	}
	if code, out, _ := agentCmd(t, env, "ci", "verify", "bare.json"); code != 1 || out != "" {
		t.Errorf("an unsigned statement verified without --unsigned: %d", code)
	}
	code, out, _ = agentCmd(t, env, "ci", "verify", "--unsigned", "--json", "bare.json")
	var res struct {
		Signed bool `json:"signed"`
		OK     bool `json:"ok"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || res.Signed || !res.OK {
		t.Errorf("unsigned verify: %d %s", code, out)
	}
	if pb, err := os.ReadFile("predicate.json"); err != nil || !bytes.Contains(pb, []byte(`"ci_schema": "tellproof-ci/v0.1"`)) {
		t.Errorf("predicate: %v %s", err, pb)
	}
	bare, _ := os.ReadFile("bare.json")
	if !bytes.Contains(bare, []byte("https://github.com/acme/app/actions/runs/77")) || !bytes.Contains(bare, []byte("refs/pull/9/merge")) {
		t.Errorf("run context missing: %s", bare)
	}
	// Usage errors.
	if code, _, _ := agentCmd(t, env, "ci", "run", "--", "true"); code != 2 {
		t.Error("ci run without --gate")
	}
	if code, _, _ := agentCmd(t, env, "ci", "attest"); code != 2 {
		t.Error("attest without a key")
	}
}
