// Package policy reads tellproof.policy.json (D-055): what an AI system is
// meant to be, written down where its code lives. It declares the models and
// regions the system may use, and keeps the registers the evidence
// objectives compare the telemetry with: material changes, incidents and
// evaluation runs recorded outside the telemetry. `tellproof-agent check`
// evaluates the local evidence log against it with the same evaluator that
// issues hosted packs.
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/cievidence"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/evaluate"
)

// DefaultFile is where `check` and `policy init` look by default.
const DefaultFile = "tellproof.policy.json"

// LegacyFile is the name before the rename to Tellproof (D-056); it is
// still read when DefaultFile is absent.
const LegacyFile = "plumbline.policy.json"

// SchemaURL is the policy's published JSON Schema, for editors.
const SchemaURL = "https://raw.githubusercontent.com/Lumetrix-Advanced-Technologies/tellproof/main/spec/policy.schema.json"

// Policy is tellproof.policy.json.
type Policy struct {
	Schema          string                   `json:"$schema,omitempty"`
	Organisation    string                   `json:"organisation"`
	AISystem        string                   `json:"ai_system"`
	Environment     string                   `json:"environment"`
	Description     string                   `json:"description,omitempty"`
	Timezone        string                   `json:"timezone"`
	DeclaredModels  []evaluate.DeclaredModel `json:"declared_models"`
	DeclaredRegions []string                 `json:"declared_regions"`
	Changes         []evaluate.Change        `json:"changes"`
	Incidents       []evaluate.Incident      `json:"incidents"`
	Evals           []batch.Eval             `json:"evals"`
	CI              *CI                      `json:"ci,omitempty"`
}

// CI is what `tellproof-agent ci verify` requires of a commit (D-058):
// gates that must have run and passed, and gates whose must-fail canary
// must have been caught.
type CI struct {
	RequiredGates []string `json:"required_gates"`
	CanaryGates   []string `json:"canary_gates,omitempty"`
}

var (
	nameRE = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]{1,128}$`)
	refRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,40}$`)
)

// Find is where the policy is: $TELLPROOF_POLICY, else tellproof.policy.json
// in the working directory, else in the data directory (where a container
// keeps it), else tellproof.policy.json in the working directory (to create).
func Find(getenv func(string) string, dataDir string) string {
	if p := getenv("TELLPROOF_POLICY"); p != "" {
		return p
	}
	for _, name := range []string{DefaultFile, LegacyFile} {
		if fileExists(name) {
			return name
		}
		if dataDir != "" {
			if p := filepath.Join(dataDir, name); fileExists(p) {
				return p
			}
		}
	}
	return DefaultFile
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// Load reads and checks a policy file. Unknown keys are refused, so a typo
// never silently weakens a declaration.
func Load(path string) (*Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	return Parse(b)
}

// Parse is Load for bytes.
func Parse(b []byte) (*Policy, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("policy: unexpected data after the JSON document")
	}
	return &p, p.Validate()
}

// Validate reports the first problem, naming the field.
func (p *Policy) Validate() error {
	bad := func(format string, a ...any) error { return fmt.Errorf("policy: "+format, a...) }
	switch {
	case strings.TrimSpace(p.AISystem) == "" || len(p.AISystem) > 200:
		return bad("ai_system is required (at most 200 characters)")
	case len(p.Organisation) > 200 || len(p.Environment) > 40:
		return bad("organisation is at most 200 characters and environment at most 40")
	}
	if p.Timezone == "" {
		p.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return bad("timezone %q is not an IANA time zone (for example Australia/Sydney)", p.Timezone)
	}
	for i, m := range p.DeclaredModels {
		if !nameRE.MatchString(m.Provider) || !nameRE.MatchString(m.Model) || len(m.Role) > 100 {
			return bad("declared_models[%d]: provider and model are short names (as the agent reports them), role at most 100 characters", i)
		}
	}
	for i, r := range p.DeclaredRegions {
		if !nameRE.MatchString(r) {
			return bad("declared_regions[%d]: %q is not a region name such as us-east-1", i, r)
		}
	}
	for i, c := range p.Changes {
		if _, err := time.Parse("2006-01-02", c.Date); err != nil {
			return bad("changes[%d]: date must be YYYY-MM-DD", i)
		}
		if c.Kind != "model" && c.Kind != "prompt" {
			return bad("changes[%d]: kind must be model or prompt", i)
		}
		if strings.TrimSpace(c.What) == "" || len(c.What) > 500 {
			return bad("changes[%d]: what changed is 1 to 500 characters", i)
		}
	}
	for i, in := range p.Incidents {
		if !refRE.MatchString(in.ID) {
			return bad("incidents[%d]: id is 1 to 40 letters, digits, dots, dashes or underscores", i)
		}
		opened, err := time.Parse(time.RFC3339, in.Opened)
		if err != nil {
			return bad("incidents[%d]: opened must be an RFC 3339 time such as 2026-10-14T03:20:00Z", i)
		}
		for name, v := range map[string]string{"acknowledged": in.Acknowledged, "resolved": in.Resolved} {
			if v == "" {
				continue
			}
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return bad("incidents[%d]: %s must be an RFC 3339 time", i, name)
			}
			if t.Before(opened) {
				return bad("incidents[%d]: %s is before opened", i, name)
			}
		}
		if len(in.Summary) > 500 {
			return bad("incidents[%d]: summary is at most 500 characters", i)
		}
	}
	for i, e := range p.Evals {
		if strings.TrimSpace(e.Suite) == "" || !nameRE.MatchString(e.Model) || len(e.PromptVersion) > 128 {
			return bad("evals[%d]: suite and model are required; model as the agent reports it", i)
		}
		if _, err := time.Parse(time.RFC3339, e.RanAt); err != nil {
			return bad("evals[%d]: ran_at must be an RFC 3339 time", i)
		}
		if e.PassRate < 0 || e.PassRate > 1 {
			return bad("evals[%d]: pass_rate is a number from 0 to 1", i)
		}
	}
	if p.CI != nil {
		for field, gates := range map[string][]string{"required_gates": p.CI.RequiredGates, "canary_gates": p.CI.CanaryGates} {
			for i, g := range gates {
				if !cievidence.GateRE.MatchString(g) {
					return bad("ci.%s[%d]: %q is not a gate name (lower-case letters, digits, '.', '_' or '-', at most 64)", field, i, g)
				}
			}
		}
	}
	return nil
}

// System is the evaluator's view of the policy for one period (local days,
// inclusive).
func (p *Policy) System(start, end string) evaluate.System {
	models := p.DeclaredModels
	if models == nil {
		models = []evaluate.DeclaredModel{}
	}
	regions := p.DeclaredRegions
	if regions == nil {
		regions = []string{}
	}
	return evaluate.System{Organisation: p.Organisation, AISystem: p.AISystem, Environment: p.Environment, Description: p.Description,
		Timezone: p.Timezone, Period: evaluate.Period{Start: start, End: end}, DeclaredModels: models, DeclaredRegions: regions}
}

// Write saves a policy as indented JSON.
func (p *Policy) Write(path string) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
