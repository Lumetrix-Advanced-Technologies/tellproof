// Package otlp receives OTLP/HTTP JSON trace exports and normalises each span
// to the few facts the evidence objectives need (docs/architecture.md §3).
// Attribute values are read only for the names spec/semconv-map.json maps;
// content attributes are counted for presence and their values never read, so
// no content can reach a batch.
package otlp

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/spec"
	specdata "github.com/lumetrix-advanced-technologies/tellproof/spec"
)

// piiValues are the tellproof.data_class values that mark personal data (D-020).
func piiValues() []string { return []string{"personal"} }

// Mapping is a validated semconv map, ready for lookups.
type Mapping struct {
	Version  string
	fields   map[string][]string
	readable map[string]bool // attribute names whose values may be read
	presence map[string]bool // content attributes: presence only
	prefixes []string        // content attribute prefixes: presence only
	llmOps   map[string]bool
}

// DefaultMapping is the semconv map embedded at build time.
func DefaultMapping() (*Mapping, error) {
	return NewMapping([]byte(specdata.SemconvMap))
}

// NewMapping validates a semconv map (internal/spec rules: every attribute in
// one field only, no content attribute mapped) and indexes it.
func NewMapping(semconvJSON []byte) (*Mapping, error) {
	var s spec.SemconvMap
	if err := spec.Decode(bytes.NewReader(semconvJSON), &s); err != nil {
		return nil, fmt.Errorf("otlp: semconv map: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("otlp: %w", err)
	}
	for _, f := range []string{"provider", "request_model", "response_model", "input_tokens", "output_tokens", "operation", "region"} {
		if len(s.Fields[f]) == 0 {
			return nil, fmt.Errorf("otlp: semconv map has no %q field", f)
		}
	}
	m := &Mapping{Version: s.Version, fields: s.Fields, readable: map[string]bool{"service.name": true},
		presence: map[string]bool{}, prefixes: s.PresenceOnlyPrefixes, llmOps: map[string]bool{}}
	for _, attrs := range s.Fields {
		for _, a := range attrs {
			m.readable[a] = true
		}
	}
	for _, a := range s.PresenceOnly {
		m.presence[a] = true
		delete(m.readable, a)
	}
	for _, op := range s.LLMOperations {
		m.llmOps[op] = true
	}
	return m, nil
}

// isContent reports whether an attribute holds message content.
func (m *Mapping) isContent(key string) bool {
	if m.presence[key] {
		return true
	}
	for _, p := range m.prefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// canRead reports whether an attribute's value may be decoded at all.
func (m *Mapping) canRead(key string) bool {
	return m.readable[key] && !m.isContent(key)
}
