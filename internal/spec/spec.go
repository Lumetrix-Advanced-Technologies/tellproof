// Package spec loads and cross-checks the shared spec files: spec/objectives.json,
// spec/framework-refs.json and spec/semconv-map.json.
package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

// maxRefTitle caps framework ref titles. Titles are our own short wording; a long
// title is a sign that licensed control text was pasted in.
const maxRefTitle = 60

// Objectives is spec/objectives.json.
type Objectives struct {
	Version    int         `json:"version"`
	RuleKinds  []string    `json:"rule_kinds"`
	Objectives []Objective `json:"objectives"`
}

// Objective is one evidence objective.
type Objective struct {
	ID      string   `json:"id"`
	Version int      `json:"version"`
	Title   string   `json:"title"`
	Rule    Rule     `json:"rule"`
	Scope   string   `json:"scope"`
	Basis   string   `json:"basis"`
	MapsTo  []string `json:"maps_to"`
}

// Rule holds the fields of every rule kind; ruleShapes says which a kind uses.
// Numbers are pointers so that an explicit 0 counts as present.
type Rule struct {
	Kind          string   `json:"kind"`
	Numerator     string   `json:"numerator,omitempty"`
	Denominator   string   `json:"denominator,omitempty"`
	Metric        string   `json:"metric,omitempty"`
	Observed      string   `json:"observed,omitempty"`
	Declared      string   `json:"declared,omitempty"`
	Event         string   `json:"event,omitempty"`
	VersionsFrom  string   `json:"versions_from,omitempty"`
	MaxAge        string   `json:"max_age,omitempty"`
	Threshold     *float64 `json:"threshold,omitempty"`
	AttentionBand *float64 `json:"attention_band,omitempty"`
	MinPassRate   *float64 `json:"min_pass_rate,omitempty"`
}

// FrameworkRefs is spec/framework-refs.json.
type FrameworkRefs struct {
	Version int            `json:"version"`
	Note    string         `json:"note"`
	Refs    []FrameworkRef `json:"refs"`
}

// FrameworkRef is a framework control ID with our own short title, never its text.
type FrameworkRef struct {
	ID        string `json:"id"`
	Framework string `json:"framework"`
	Ref       string `json:"ref"`
	Title     string `json:"title"`
	Verified  bool   `json:"verified"`
}

// SemconvMap is spec/semconv-map.json.
type SemconvMap struct {
	Version      string              `json:"version"`
	Note         string              `json:"note"`
	Fields       map[string][]string `json:"fields"`
	PresenceOnly []string            `json:"presence_only"`
	// PresenceOnlyPrefixes: any attribute whose name starts with one of these is
	// content too (indexed names such as gen_ai.prompt.0.content).
	PresenceOnlyPrefixes []string `json:"presence_only_prefixes"`
	// LLMOperations are the operation values that make a span an LLM call.
	LLMOperations []string `json:"llm_operations"`
}

type ruleShape struct {
	required []string
	optional []string
}

// ruleShapes lists the five rule kinds in order and the fields each takes.
func ruleShapes() ([]string, map[string]ruleShape) {
	kinds := []string{"ratio_at_least", "max_value", "zero_count", "freshness_within", "all_declared"}
	shapes := map[string]ruleShape{
		"ratio_at_least":   {required: []string{"numerator", "denominator", "threshold"}, optional: []string{"attention_band"}},
		"max_value":        {required: []string{"metric", "threshold"}, optional: []string{"attention_band"}},
		"zero_count":       {required: []string{"metric"}},
		"freshness_within": {required: []string{"event", "versions_from", "max_age"}, optional: []string{"min_pass_rate"}},
		"all_declared":     {required: []string{"observed", "declared"}},
	}
	return kinds, shapes
}

// fields returns the JSON names of the rule fields that are set, kind excluded.
func (r Rule) fields() []string {
	var out []string
	add := func(name string, set bool) {
		if set {
			out = append(out, name)
		}
	}
	add("numerator", r.Numerator != "")
	add("denominator", r.Denominator != "")
	add("metric", r.Metric != "")
	add("observed", r.Observed != "")
	add("declared", r.Declared != "")
	add("event", r.Event != "")
	add("versions_from", r.VersionsFrom != "")
	add("max_age", r.MaxAge != "")
	add("threshold", r.Threshold != nil)
	add("attention_band", r.AttentionBand != nil)
	add("min_pass_rate", r.MinPassRate != nil)
	return out
}

// Decode strictly decodes one JSON document into v: unknown fields and trailing
// data are errors.
func Decode(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON document")
	}
	return nil
}

// Check decodes the three spec files and validates each one and the references
// between them. It reports every problem found, one per line.
func Check(objectives, frameworkRefs, semconvMap []byte) error {
	var o Objectives
	var f FrameworkRefs
	var s SemconvMap
	var errs []error
	if err := Decode(bytes.NewReader(objectives), &o); err != nil {
		errs = append(errs, fmt.Errorf("objectives.json: %w", err))
	}
	if err := Decode(bytes.NewReader(frameworkRefs), &f); err != nil {
		errs = append(errs, fmt.Errorf("framework-refs.json: %w", err))
	}
	if err := Decode(bytes.NewReader(semconvMap), &s); err != nil {
		errs = append(errs, fmt.Errorf("semconv-map.json: %w", err))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return errors.Join(f.Validate(), o.Validate(&f), s.Validate())
}

// Validate checks that ref IDs are unique and present and that titles stay short.
func (f *FrameworkRefs) Validate() error {
	var errs []error
	seen := make(map[string]bool, len(f.Refs))
	for i, r := range f.Refs {
		if r.ID == "" {
			errs = append(errs, fmt.Errorf("framework-refs.json: refs[%d] has no id", i))
			continue
		}
		if seen[r.ID] {
			errs = append(errs, fmt.Errorf("framework-refs.json: duplicate ref id %q", r.ID))
		}
		seen[r.ID] = true
		if r.Framework == "" || r.Ref == "" || r.Title == "" {
			errs = append(errs, fmt.Errorf("framework-refs.json: %s: framework, ref and title are all required", r.ID))
		}
		if n := utf8.RuneCountInString(r.Title); n > maxRefTitle {
			errs = append(errs, fmt.Errorf("framework-refs.json: %s: title is %d characters; keep our own short title (at most %d)", r.ID, n, maxRefTitle))
		}
	}
	return errors.Join(errs...)
}

// Validate checks rule kinds, rule fields, objective IDs and that every maps_to
// entry exists in refs.
func (o *Objectives) Validate(refs *FrameworkRefs) error {
	kinds, shapes := ruleShapes()
	known := strings.Join(kinds, ", ")
	refIDs := make(map[string]bool, len(refs.Refs))
	for _, r := range refs.Refs {
		refIDs[r.ID] = true
	}

	var errs []error
	if !slices.Equal(o.RuleKinds, kinds) {
		errs = append(errs, fmt.Errorf("objectives.json: rule_kinds must be exactly %s; got %s", known, strings.Join(o.RuleKinds, ", ")))
	}
	seen := make(map[string]bool, len(o.Objectives))
	for i, obj := range o.Objectives {
		id := obj.ID
		if id == "" {
			id = fmt.Sprintf("objectives[%d]", i)
			errs = append(errs, fmt.Errorf("objectives.json: %s has no id", id))
		} else if seen[id] {
			errs = append(errs, fmt.Errorf("objectives.json: duplicate objective id %q", id))
		}
		seen[id] = true

		shape, ok := shapes[obj.Rule.Kind]
		if !ok {
			errs = append(errs, fmt.Errorf("objectives.json: %s: rule kind %q is not one of %s", id, obj.Rule.Kind, known))
		} else {
			set := obj.Rule.fields()
			for _, name := range shape.required {
				if !slices.Contains(set, name) {
					errs = append(errs, fmt.Errorf("objectives.json: %s: rule kind %q requires %q", id, obj.Rule.Kind, name))
				}
			}
			for _, name := range set {
				if !slices.Contains(shape.required, name) && !slices.Contains(shape.optional, name) {
					errs = append(errs, fmt.Errorf("objectives.json: %s: rule kind %q does not take %q", id, obj.Rule.Kind, name))
				}
			}
		}

		for _, ref := range obj.MapsTo {
			if !refIDs[ref] {
				errs = append(errs, fmt.Errorf("objectives.json: %s: maps_to %q is not in framework-refs.json", id, ref))
			}
		}
	}
	return errors.Join(errs...)
}

// Validate checks that every attribute name appears once: mapped to a single
// field, or listed as presence-only (content whose value is never read).
func (s *SemconvMap) Validate() error {
	var errs []error
	if s.Version == "" {
		errs = append(errs, errors.New("semconv-map.json: version is required"))
	}
	if len(s.PresenceOnly) == 0 {
		errs = append(errs, errors.New("semconv-map.json: presence_only must list the content attributes"))
	}

	fieldNames := make([]string, 0, len(s.Fields))
	for name := range s.Fields {
		fieldNames = append(fieldNames, name)
	}
	slices.Sort(fieldNames) // map order is random; keep messages stable

	mappedTo := make(map[string]string)
	for _, field := range fieldNames {
		attrs := s.Fields[field]
		if len(attrs) == 0 {
			errs = append(errs, fmt.Errorf("semconv-map.json: field %q lists no attributes", field))
		}
		for _, attr := range attrs {
			if prev, ok := mappedTo[attr]; ok {
				errs = append(errs, fmt.Errorf("semconv-map.json: attribute %q is mapped to both %q and %q", attr, prev, field))
				continue
			}
			mappedTo[attr] = field
		}
	}

	for _, prefix := range s.PresenceOnlyPrefixes {
		if !strings.HasSuffix(prefix, ".") || len(prefix) < 2 {
			errs = append(errs, fmt.Errorf("semconv-map.json: presence-only prefix %q must be a name followed by a dot", prefix))
		}
		for _, field := range fieldNames {
			for _, attr := range s.Fields[field] {
				if strings.HasPrefix(attr, prefix) {
					errs = append(errs, fmt.Errorf("semconv-map.json: attribute %q of field %q falls under presence-only prefix %q; content values must never be read", attr, field, prefix))
				}
			}
		}
	}
	seen := make(map[string]bool, len(s.PresenceOnly))
	for _, attr := range s.PresenceOnly {
		if field, ok := mappedTo[attr]; ok {
			errs = append(errs, fmt.Errorf("semconv-map.json: presence-only attribute %q is also mapped to field %q; content values must never be read", attr, field))
		}
		if seen[attr] {
			errs = append(errs, fmt.Errorf("semconv-map.json: presence-only attribute %q is listed twice", attr))
		}
		seen[attr] = true
	}
	return errors.Join(errs...)
}
