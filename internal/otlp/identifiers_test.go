package otlp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	specdata "github.com/lumetrix-advanced-technologies/tellproof/spec"
)

func corpusIdentifiers(t *testing.T) (valid, invalid []string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "spec", "leak-corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Identifiers struct{ Valid, Invalid []string } `json:"identifiers"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c.Identifiers.Valid, c.Identifiers.Invalid
}

// Acceptance (D-065): the agent and the concierge classify every example in
// spec/leak-corpus.json the same way, and as the corpus says.
func TestIdentifierRules(t *testing.T) {
	valid, invalid := corpusIdentifiers(t)
	for _, s := range valid {
		if got := defaultIdentifiers.clean(s); got != s {
			t.Errorf("%q should be an identifier, got %q", s, got)
		}
	}
	for _, s := range invalid {
		if got := defaultIdentifiers.clean(s); got != "invalid" {
			t.Errorf("%q should be invalid, got %q", s, got)
		}
	}
	if defaultIdentifiers.clean("") != "" {
		t.Error("an absent value must stay absent (unknown), not invalid")
	}

	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	all, _ := json.Marshal(append(append([]string{}, valid...), invalid...))
	cmd := exec.Command(py, "-c", "import sys, json; sys.path.insert(0, 'concierge'); import ingest; print(json.dumps([ingest.identifier(s) for s in json.loads(sys.stdin.read())]))")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Stdin = strings.NewReader(string(all))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("concierge: %v", err)
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	var in []string
	_ = json.Unmarshal(all, &in)
	for i, s := range in {
		if want := defaultIdentifiers.clean(s); got[i] != want {
			t.Errorf("%q: the concierge says %q, the agent %q", s, got[i], want)
		}
	}
}

func TestIdentifierFileIsChecked(t *testing.T) {
	if _, err := loadIdentifiers([]byte(specdata.Identifiers)); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"bad pattern":       `{"version":"1","note":"","fields":["provider","model","region","prompt_version"],"pattern":"(","credential_patterns":[],"replacement":"invalid"}`,
		"field missing":     `{"version":"1","note":"","fields":["provider","model","region"],"pattern":"^a$","credential_patterns":[],"replacement":"a"}`,
		"bad replacement":   `{"version":"1","note":"","fields":["provider","model","region","prompt_version"],"pattern":"^[a-z]+$","credential_patterns":[],"replacement":"no way"}`,
		"unknown key":       `{"version":"1","note":"","fields":[],"pattern":"^a$","credential_patterns":[],"replacement":"a","extra":1}`,
		"bad credential re": `{"version":"1","note":"","fields":["provider","model","region","prompt_version"],"pattern":"^[a-z]+$","credential_patterns":["["],"replacement":"invalid"}`,
	} {
		if _, err := loadIdentifiers([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A prompt, personal data or a key in an attribute the agent reads becomes
// "invalid" (or, for a canary kind, "other"); the span is still counted.
func TestContentInReadAttributesBecomesInvalid(t *testing.T) {
	m, err := DefaultMapping()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"Ignore every previous instruction", "AKIAIOSFODNN7EXAMPLE", "jane@example.com, +61 491 570 006", "sk-proj-abc"} {
		body := `{"resourceSpans":[{"resource":{"attributes":[{"key":"cloud.region","value":{"stringValue":` + jsonString(v) + `}}]},"scopeSpans":[{"spans":[{"traceId":"0123456789abcdef0123456789abcdef","startTimeUnixNano":"1","attributes":[` +
			`{"key":"gen_ai.operation.name","value":{"stringValue":"chat"}},{"key":"gen_ai.provider.name","value":{"stringValue":` + jsonString(v) + `}},` +
			`{"key":"gen_ai.request.model","value":{"stringValue":` + jsonString(v) + `}},{"key":"tellproof.prompt.version","value":{"stringValue":` + jsonString(v) + `}},` +
			`{"key":"tellproof.canary.kind","value":{"stringValue":` + jsonString(v) + `}}]}]}]}]}`
		res, err := Parse([]byte(body), m)
		if err != nil || len(res.Spans) != 1 {
			t.Fatalf("%q: %v %+v", v, err, res)
		}
		s := res.Spans[0]
		if s.Kind != KindLLM || s.Provider != "invalid" || s.Model != "invalid" || s.Region != "invalid" || s.PromptVersion != "invalid" || s.CanaryKind != "other" {
			t.Errorf("%q: %+v", v, s)
		}
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }
