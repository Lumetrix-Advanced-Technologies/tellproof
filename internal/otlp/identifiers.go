package otlp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"

	specdata "github.com/lumetrix-advanced-technologies/tellproof/spec"
)

// identifiers is spec/identifiers.json (D-065): the provider, model, region
// and prompt version copied into a batch must look like identifiers and not
// like credentials; anything else becomes Replacement.
type identifiers struct {
	Version     string   `json:"version"`
	Note        string   `json:"note"`
	Fields      []string `json:"fields"`
	Pattern     string   `json:"pattern"`
	Credentials []string `json:"credential_patterns"`
	Replacement string   `json:"replacement"`

	pattern     *regexp.Regexp
	credentials []*regexp.Regexp
}

func loadIdentifiers(b []byte) (*identifiers, error) {
	var id identifiers
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&id); err != nil {
		return nil, fmt.Errorf("otlp: identifiers.json: %w", err)
	}
	var err error
	if id.pattern, err = regexp.Compile(id.Pattern); err != nil {
		return nil, fmt.Errorf("otlp: identifiers.json pattern: %w", err)
	}
	for _, p := range id.Credentials {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("otlp: identifiers.json credential pattern %q: %w", p, err)
		}
		id.credentials = append(id.credentials, re)
	}
	for _, f := range []string{"provider", "model", "region", "prompt_version"} {
		if !slices.Contains(id.Fields, f) {
			return nil, fmt.Errorf("otlp: identifiers.json must cover %q", f)
		}
	}
	if id.Replacement == "" || !id.valid(id.Replacement) {
		return nil, fmt.Errorf("otlp: identifiers.json replacement %q is not itself an identifier", id.Replacement)
	}
	return &id, nil
}

func (id *identifiers) valid(s string) bool {
	if !id.pattern.MatchString(s) {
		return false
	}
	for _, re := range id.credentials {
		if re.MatchString(s) {
			return false
		}
	}
	return true
}

// clean returns s, or the replacement when s is not an identifier. Empty
// stays empty: an absent value is unknown, not invalid.
func (id *identifiers) clean(s string) string {
	if s == "" || id.valid(s) {
		return s
	}
	return id.Replacement
}

var defaultIdentifiers = func() *identifiers {
	id, err := loadIdentifiers([]byte(specdata.Identifiers))
	if err != nil {
		panic(err) // the embedded spec file is checked by the tests
	}
	return id
}()
