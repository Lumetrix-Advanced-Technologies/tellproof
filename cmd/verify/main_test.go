package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/cievidence"
)

func vec(name string) string { return filepath.Join("..", "..", "spec", "vectors", name) }

var testJWKSPath = vec("test-jwks.json")

// runCLI runs the command with a plain HTTP client and returns exit code and output.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return runWith(t, &http.Client{}, args...)
}

func runWith(t *testing.T, client *http.Client, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), config{version: "v0.1.0-test", client: client}, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func passChain() []string {
	return []string{vec("pass/KST-CTA-0001.dsse.json"), vec("pass/KST-CTA-0002.dsse.json"), vec("pass/KST-CTA-0003.dsse.json")}
}

// Acceptance: the pass chain prints VERIFIED and exits 0.
func TestPassChainVerified(t *testing.T) {
	code, out, errOut := runCLI(t, append([]string{"--jwks", testJWKSPath}, passChain()...)...)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.HasSuffix(out, "RESULT: VERIFIED\n") {
		t.Errorf("output does not end with RESULT: VERIFIED:\n%s", out)
	}
	if n := strings.Count(out, "result ............... VERIFIED"); n != 3 {
		t.Errorf("%d packs reported VERIFIED, want 3:\n%s", n, out)
	}
	if strings.Contains(out, "FAILED") {
		t.Errorf("pass chain output mentions FAILED:\n%s", out)
	}
	// The chain is checked from the second pack on; digests are printed in full.
	if n := strings.Count(out, "chain link ........... links to the previous pack"); n != 2 {
		t.Errorf("chain checked %d times, want 2:\n%s", n, out)
	}
	if !strings.Contains(out, "chain link ........... not checked (no previous pack given)") {
		t.Errorf("first pack's chain should be reported as not checked:\n%s", out)
	}
	if !regexp.MustCompile(`digest ............... sha256:[0-9a-f]{64}\n`).MatchString(out) {
		t.Errorf("digest is not printed in full:\n%s", out)
	}
}

type manifestCase struct {
	Name   string  `json:"name"`
	File   string  `json:"file"`
	Prev   *string `json:"prev"`
	Expect struct {
		Signature     bool  `json:"signature"`
		SubjectDigest bool  `json:"subject_digest"`
		Chain         *bool `json:"chain"`
		Merkle        bool  `json:"merkle"`
		Verified      bool  `json:"verified"`
	} `json:"expect"`
}

func manifestCases(t *testing.T) []manifestCase {
	t.Helper()
	b, err := os.ReadFile(vec("manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Cases []manifestCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m.Cases
}

func caseArgs(c manifestCase, extra ...string) []string {
	args := append([]string{"--jwks", testJWKSPath}, extra...)
	if c.Prev != nil {
		args = append(args, vec(*c.Prev))
	}
	return append(args, vec(c.File))
}

// lastBlock returns the human output for the last pack: from its file header
// to the RESULT line.
func lastBlock(t *testing.T, out, file string) string {
	t.Helper()
	i := strings.LastIndex(out, file+"\n")
	if i < 0 {
		t.Fatalf("no block for %s in:\n%s", file, out)
	}
	return out[i:]
}

// Acceptance: each fail vector exits 1 with the right failed check named, and
// no check that passed is reported as failed.
func TestFailVectorsNameTheFailedCheck(t *testing.T) {
	for _, c := range manifestCases(t) {
		if c.Expect.Verified {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			code, out, errOut := runCLI(t, caseArgs(c)...)
			if code != 1 {
				t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
			}
			if !strings.HasSuffix(out, "RESULT: FAILED\n") {
				t.Errorf("output does not end with RESULT: FAILED:\n%s", out)
			}
			block := lastBlock(t, out, vec(c.File))
			chainFailed := c.Expect.Chain != nil && !*c.Expect.Chain
			for label, failed := range map[string]bool{
				"signature ............ ": !c.Expect.Signature,
				"subject digest ....... ": !c.Expect.SubjectDigest,
				"chain link ........... ": chainFailed,
				"disclosed trace ...... ": !c.Expect.Merkle,
			} {
				line := regexp.MustCompile(regexp.QuoteMeta(label) + `.*`).FindString(block)
				if line == "" {
					t.Errorf("no %q line in:\n%s", strings.TrimRight(label, ". "), block)
					continue
				}
				if got := strings.Contains(line, "FAILED"); got != failed {
					t.Errorf("line %q: FAILED = %v, want %v", line, got, failed)
				}
			}
			if !strings.Contains(block, "result ............... FAILED (") {
				t.Errorf("pack result line does not list the failed checks:\n%s", block)
			}
		})
	}
}

// --json reports, for every manifest case, exactly the manifest's expected
// result for the last pack, and the overall verdict.
func TestJSONMatchesManifest(t *testing.T) {
	for _, c := range manifestCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			code, out, errOut := runCLI(t, caseArgs(c, "--json")...)
			if want := map[bool]int{true: 0, false: 1}[c.Expect.Verified]; code != want {
				t.Fatalf("exit %d, want %d\nstderr: %s", code, want, errOut)
			}
			var report struct {
				Verified bool `json:"verified"`
				Packs    []struct {
					File          string `json:"file"`
					Signature     bool   `json:"signature"`
					SubjectDigest bool   `json:"subject_digest"`
					Chain         *bool  `json:"chain"`
					Merkle        *bool  `json:"merkle"`
					Verified      bool   `json:"verified"`
					Digest        string `json:"digest"`
				} `json:"packs"`
			}
			dec := json.NewDecoder(strings.NewReader(out))
			if err := dec.Decode(&report); err != nil {
				t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
			}
			if dec.More() {
				t.Fatalf("extra output after the JSON document:\n%s", out)
			}
			if report.Verified != c.Expect.Verified || len(report.Packs) == 0 {
				t.Fatalf("verified = %v with %d packs, want %v", report.Verified, len(report.Packs), c.Expect.Verified)
			}
			last := report.Packs[len(report.Packs)-1]
			got := normJSON(t, map[string]any{"signature": last.Signature, "subject_digest": last.SubjectDigest, "chain": last.Chain, "merkle": last.Merkle, "verified": last.Verified})
			want := normJSON(t, c.Expect)
			if got != want {
				t.Errorf("last pack = %s, manifest %s", got, want)
			}
			if last.File != vec(c.File) || !strings.HasPrefix(last.Digest, "sha256:") {
				t.Errorf("file %q digest %q", last.File, last.Digest)
			}
		})
	}
}

// normJSON encodes v with keys sorted, so values can be compared as text.
func normJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	b, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestVersion(t *testing.T) {
	code, out, _ := runCLI(t, "--version")
	if code != 0 || out != "tellproof-verify v0.1.0-test\n" {
		t.Errorf("--version = exit %d, %q", code, out)
	}
}

// Flags may come after the pack files.
func TestFlagsAfterFiles(t *testing.T) {
	code, out, errOut := runCLI(t, vec("pass/KST-CTA-0001.dsse.json"), "--jwks", testJWKSPath, "--json")
	if code != 0 || !json.Valid([]byte(out)) {
		t.Errorf("exit %d, JSON valid %v\nstdout: %s\nstderr: %s", code, json.Valid([]byte(out)), out, errOut)
	}
}

// Usage and input problems exit 2 and explain themselves on stderr.
func TestUsageErrors(t *testing.T) {
	dir := t.TempDir()
	notJSON := filepath.Join(dir, "bad-jwks.json")
	if err := os.WriteFile(notJSON, []byte(`{"keys":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pack1 := vec("pass/KST-CTA-0001.dsse.json")
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no arguments", nil, "usage"},
		{"no packs", []string{"--jwks", testJWKSPath}, "no pack files"},
		{"no --jwks", []string{pack1}, "--jwks is required"},
		{"unknown flag", []string{"--jwks", testJWKSPath, "--nope", pack1}, "nope"},
		{"missing JWKS file", []string{"--jwks", filepath.Join(dir, "absent.json"), pack1}, "jwks"},
		{"invalid JWKS", []string{"--jwks", notJSON, pack1}, "no keys"},
		{"plain http JWKS URL", []string{"--jwks", "http://example.com/jwks.json", pack1}, "https"},
		{"missing pack file", []string{"--jwks", testJWKSPath, filepath.Join(dir, "absent.dsse.json")}, "absent.dsse.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, errOut := runCLI(t, tt.args...)
			if code != 2 {
				t.Errorf("exit %d, want 2\nstdout: %s\nstderr: %s", code, out, errOut)
			}
			if !strings.Contains(errOut, tt.wantErr) {
				t.Errorf("stderr %q does not mention %q", errOut, tt.wantErr)
			}
		})
	}
}

// A file that is not a pack fails verification (exit 1) rather than crashing.
func TestNotAPackFails(t *testing.T) {
	dir := t.TempDir()
	tests := map[string]string{
		"not an envelope.json": `{"hello":"world"}`,
		"not a statement.json": `{"payloadType":"application/vnd.in-toto+json","payload":"e30=","signatures":[{"keyid":"test-p256-2026-10","sig":"AAAA"}]}`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			code, out, errOut := runCLI(t, "--jwks", testJWKSPath, path)
			if code != 1 {
				t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", code, out, errOut)
			}
			if !strings.Contains(out, "FAILED: not a valid pack") || !strings.HasSuffix(out, "RESULT: FAILED\n") {
				t.Errorf("output:\n%s", out)
			}
		})
	}
}

func TestJWKSFromURL(t *testing.T) {
	jwks, err := os.ReadFile(testJWKSPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/jwks.json":
			w.Write(jwks)
		case "/huge":
			w.Write(bytes.Repeat([]byte(" "), maxJWKSBytes+1))
		default:
			http.NotFound(w, r)
		}
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the untrusted-certificate case logs a handshake error
	srv.StartTLS()
	defer srv.Close()
	pack1 := vec("pass/KST-CTA-0001.dsse.json")

	code, out, errOut := runWith(t, srv.Client(), "--jwks", srv.URL+"/.well-known/jwks.json", pack1)
	if code != 0 || !strings.HasSuffix(out, "RESULT: VERIFIED\n") {
		t.Errorf("JWKS from URL: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	for path, wantErr := range map[string]string{"/missing": "404", "/huge": "larger than"} {
		code, _, errOut := runWith(t, srv.Client(), "--jwks", srv.URL+path, pack1)
		if code != 2 || !strings.Contains(errOut, wantErr) {
			t.Errorf("%s: exit %d, stderr %q; want 2 and %q", path, code, errOut, wantErr)
		}
	}
	// The default client does not trust the test server's certificate.
	if code, _, errOut := runWith(t, &http.Client{}, "--jwks", srv.URL+"/.well-known/jwks.json", pack1); code != 2 || !strings.Contains(errOut, "certificate") {
		t.Errorf("untrusted certificate: exit %d, stderr %q", code, errOut)
	}
}

// A file that is not an envelope breaks the chain: the next pack's link to it
// fails instead of being skipped (D-017).
func TestChainBreaksAfterAFileThatIsNotAPack(t *testing.T) {
	junk := filepath.Join(t.TempDir(), "junk.json")
	if err := os.WriteFile(junk, []byte(`{"hello":"world"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "--jwks", testJWKSPath, "--json", vec("pass/KST-CTA-0001.dsse.json"), junk, vec("pass/KST-CTA-0002.dsse.json"))
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	var report struct {
		Packs []struct {
			Chain    *bool `json:"chain"`
			Verified bool  `json:"verified"`
		} `json:"packs"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil || len(report.Packs) != 3 {
		t.Fatalf("report: %v\n%s", err, out)
	}
	if last := report.Packs[2]; last.Chain == nil || *last.Chain || last.Verified {
		t.Errorf("pack after the junk file: chain %v verified %v; want chain false, not verified", last.Chain, last.Verified)
	}
}

// tellproof-verify ci checks a CI evidence statement (D-058).
func TestVerifyCI(t *testing.T) {
	dir := t.TempDir()
	privPEM, pubPEM, _ := cievidence.NewKey()
	priv, _ := cievidence.ParsePrivate(privPEM)
	commit := strings.Repeat("ab", 20)
	st := cievidence.NewStatement("git+https://github.com/acme/app", commit, "refs/heads/main", cievidence.Run{Provider: "local"},
		[]cievidence.Gate{{Name: "tests", Passed: true, JUnit: &cievidence.JUnit{Tests: 5}}},
		[]cievidence.Gate{{Name: "tests", Canary: true, Passed: true, ExitCode: 1}}, nil, "test", time.Unix(0, 0))
	env, err := cievidence.Sign(context.Background(), st, priv)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	stmt, pub := filepath.Join(dir, "ci.json"), filepath.Join(dir, "ci.pub")
	_ = os.WriteFile(stmt, b, 0o644)
	_ = os.WriteFile(pub, pubPEM, 0o644)
	ci := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := run(context.Background(), config{}, append([]string{"ci"}, args...), &out, &errb)
		return code, out.String() + errb.String()
	}
	if code, out := ci("--pub", pub, "--commit", commit[:10], "--require", "tests", "--canary", "tests", stmt); code != 0 || !strings.Contains(out, "signature verified") {
		t.Fatalf("%d %s", code, out)
	}
	if code, _ := ci("--pub", pub, "--commit", commit, "--require", "lint", stmt); code != 1 {
		t.Error("a missing gate passed")
	}
	if code, _ := ci("--commit", commit, stmt); code != 2 {
		t.Error("no key and not --unsigned")
	}
}
