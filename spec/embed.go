// Package specdata embeds the shared spec files (as immutable strings) so binaries (the agent above
// all) carry the exact mappings, rules and schemas they were built and tested
// with. The files themselves are the source of truth; edit them, not this.
package specdata

import _ "embed"

// SemconvMap is spec/semconv-map.json: OpenTelemetry attribute names per
// internal field, and the content attributes that are counted, never read.
//
//go:embed semconv-map.json
var SemconvMap string

// Identifiers is spec/identifiers.json: what a provider, model, region or
// prompt version copied into a batch may look like (D-065).
//
//go:embed identifiers.json
var Identifiers string

// Objectives is spec/objectives.json: the eleven evidence objectives and rules.
//
//go:embed objectives.json
var Objectives string

// FrameworkRefs is spec/framework-refs.json: framework IDs and our short titles.
//
//go:embed framework-refs.json
var FrameworkRefs string
