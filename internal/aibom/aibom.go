// Package aibom writes an AI bill of materials (D-055): a CycloneDX 1.6
// document listing every model an AI system declared or was seen using,
// built from the verified local evidence log rather than from a hand-kept
// list. Each model is a machine-learning-model component with its provider,
// regions, call and token counts, first and last hour seen, prompt versions,
// and whether it was declared and observed; the AI system is the root
// component and depends on them. Nothing in it comes from message content.
package aibom

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/policy"
)

// BOM is the CycloneDX document (the subset of 1.6 that Tellproof fills in).
type BOM struct {
	BOMFormat    string       `json:"bomFormat"`
	SpecVersion  string       `json:"specVersion"`
	SerialNumber string       `json:"serialNumber"`
	Version      int          `json:"version"`
	Metadata     Metadata     `json:"metadata"`
	Components   []Component  `json:"components"`
	Dependencies []Dependency `json:"dependencies"`
}

type Metadata struct {
	Timestamp  string     `json:"timestamp"`
	Tools      Tools      `json:"tools"`
	Component  Component  `json:"component"`
	Properties []Property `json:"properties,omitempty"`
}

type Tools struct {
	Components []Component `json:"components"`
}

type Component struct {
	Type       string      `json:"type"`
	BOMRef     string      `json:"bom-ref,omitempty"`
	Name       string      `json:"name"`
	Version    string      `json:"version,omitempty"`
	Publisher  string      `json:"publisher,omitempty"`
	Supplier   *Entity     `json:"supplier,omitempty"`
	Group      string      `json:"group,omitempty"`
	Properties []Property  `json:"properties,omitempty"`
	ModelCard  *ModelCard  `json:"modelCard,omitempty"`
	ExtRefs    []ExtRef    `json:"externalReferences,omitempty"`
	Licenses   []any       `json:"licenses,omitempty"`
	Components []Component `json:"components,omitempty"`
}

type Entity struct {
	Name string `json:"name"`
}

type Property struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type ExtRef struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// ModelCard carries only what the telemetry shows about how a model is used.
type ModelCard struct {
	Considerations *Considerations `json:"considerations,omitempty"`
}

type Considerations struct {
	UseCases []string `json:"useCases,omitempty"`
}

type Dependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn"`
}

// Options for Build.
type Options struct {
	Policy       *policy.Policy // optional: the AI system's name and declared models
	Since, Until time.Time      // limit the windows used; zero: no limit
	AgentID      string
	LastDigest   string // the latest verified batch, so the BOM names the evidence it came from
	Version      string // tellproof-agent's version
	Now          func() time.Time
}

type modelKey struct{ provider, model string }

type usage struct {
	regions        map[string]bool
	calls, in, out int64
	first, last    string
	prompts        map[string]bool
}

// Build makes the BOM from verified batches.
func Build(bs []batch.Batch, o Options) BOM {
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	seen := map[modelKey]*usage{}
	get := func(k modelKey) *usage {
		if seen[k] == nil {
			seen[k] = &usage{regions: map[string]bool{}, prompts: map[string]bool{}}
		}
		return seen[k]
	}
	var from, to string
	for _, b := range bs {
		t, err := time.Parse(time.RFC3339, b.Window.Start)
		if err != nil || (!o.Since.IsZero() && t.Before(o.Since)) || (!o.Until.IsZero() && !t.Before(o.Until)) {
			continue
		}
		if from == "" || b.Window.Start < from {
			from = b.Window.Start
		}
		if b.Window.End > to {
			to = b.Window.End
		}
		for _, tc := range b.Metrics.Tokens {
			u := get(modelKey{tc.Provider, tc.Model})
			if tc.Region != "" && tc.Region != "unknown" {
				u.regions[tc.Region] = true
			}
			u.calls, u.in, u.out = u.calls+tc.Calls, u.in+tc.InputTokens, u.out+tc.OutputTokens
			if u.first == "" || b.Window.Start < u.first {
				u.first = b.Window.Start
			}
			if b.Window.End > u.last {
				u.last = b.Window.End
			}
		}
		for _, v := range b.Observed.Versions {
			for k, u := range seen {
				if k.model == v.Model && v.PromptVersion != "" {
					u.prompts[v.PromptVersion] = true
				}
			}
		}
	}
	declared := map[modelKey]string{} // -> role
	if o.Policy != nil {
		for _, m := range o.Policy.DeclaredModels {
			declared[modelKey{m.Provider, m.Model}] = m.Role
		}
	}
	keys := map[modelKey]bool{}
	for k := range seen {
		keys[k] = true
	}
	for k := range declared {
		keys[k] = true
	}
	var order []modelKey
	for k := range keys {
		order = append(order, k)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].provider != order[j].provider {
			return order[i].provider < order[j].provider
		}
		return order[i].model < order[j].model
	})

	sys := Component{Type: "application", BOMRef: "ai-system", Name: "AI system"}
	if o.Policy != nil {
		sys.Name = o.Policy.AISystem
		if o.Policy.Organisation != "" {
			sys.Supplier = &Entity{Name: o.Policy.Organisation}
		}
		if o.Policy.Environment != "" {
			sys.Properties = append(sys.Properties, Property{"tellproof:environment", o.Policy.Environment})
		}
	}
	bom := BOM{BOMFormat: "CycloneDX", SpecVersion: "1.6", SerialNumber: "urn:uuid:" + uuid(), Version: 1,
		Metadata: Metadata{Timestamp: now().UTC().Format(time.RFC3339),
			Tools: Tools{Components: []Component{{Type: "application", Name: "tellproof-agent", Version: o.Version, Group: "lumetrix",
				ExtRefs: []ExtRef{{Type: "vcs", URL: "https://github.com/Lumetrix-Advanced-Technologies/tellproof"}}}}},
			Component: sys},
		Components: []Component{}, Dependencies: []Dependency{}}
	props := []Property{{"tellproof:evidence:agent_id", o.AgentID}, {"tellproof:evidence:batches", strconv.Itoa(len(bs))}}
	if o.LastDigest != "" {
		props = append(props, Property{"tellproof:evidence:last_batch_digest", o.LastDigest})
	}
	if from != "" {
		props = append(props, Property{"tellproof:evidence:from", from}, Property{"tellproof:evidence:to", to})
	}
	bom.Metadata.Properties = props
	dep := Dependency{Ref: "ai-system", DependsOn: []string{}}
	for _, k := range order {
		ref := "model:" + k.provider + "/" + k.model
		c := Component{Type: "machine-learning-model", BOMRef: ref, Name: k.model, Publisher: k.provider, Supplier: &Entity{Name: k.provider}}
		u, observed := seen[k]
		role, isDeclared := declared[k]
		c.Properties = append(c.Properties, Property{"tellproof:provider", k.provider}, Property{"tellproof:observed", strconv.FormatBool(observed)})
		if o.Policy != nil {
			c.Properties = append(c.Properties, Property{"tellproof:declared", strconv.FormatBool(isDeclared)})
		}
		if role != "" {
			c.Properties = append(c.Properties, Property{"tellproof:role", role})
			c.ModelCard = &ModelCard{Considerations: &Considerations{UseCases: []string{role}}}
		}
		if observed {
			c.Properties = append(c.Properties,
				Property{"tellproof:regions", join(u.regions)},
				Property{"tellproof:calls", strconv.FormatInt(u.calls, 10)},
				Property{"tellproof:input_tokens", strconv.FormatInt(u.in, 10)},
				Property{"tellproof:output_tokens", strconv.FormatInt(u.out, 10)},
				Property{"tellproof:first_seen", u.first},
				Property{"tellproof:last_seen", u.last})
			if len(u.prompts) > 0 {
				c.Properties = append(c.Properties, Property{"tellproof:prompt_versions", join(u.prompts)})
			}
		}
		bom.Components = append(bom.Components, c)
		dep.DependsOn = append(dep.DependsOn, ref)
	}
	bom.Dependencies = append(bom.Dependencies, dep)
	for _, ref := range dep.DependsOn {
		bom.Dependencies = append(bom.Dependencies, Dependency{Ref: ref, DependsOn: []string{}})
	}
	return bom
}

func join(m map[string]bool) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Write writes the BOM as indented JSON.
func Write(w io.Writer, b BOM) error {
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", out)
	return err
}
