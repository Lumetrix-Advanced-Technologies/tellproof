// Package envcompat reads Tellproof's environment variables, falling back
// to the PLUMBLINE_* names that v0.1 and v0.2 (as Plumbline) used, so a
// deployment keeps working across the rename (D-056).
package envcompat

import (
	"os"
	"strings"
)

// Prefix is the current variable prefix; Legacy the one before the rename.
const (
	Prefix = "TELLPROOF_"
	Legacy = "PLUMBLINE_"
)

// Wrap returns getenv that, for a TELLPROOF_ name with no value, returns
// the legacy PLUMBLINE_ variable instead.
func Wrap(getenv func(string) string) func(string) string {
	return func(k string) string {
		if v := getenv(k); v != "" {
			return v
		}
		if rest, ok := strings.CutPrefix(k, Prefix); ok {
			return getenv(Legacy + rest)
		}
		return ""
	}
}

// Get is Wrap(os.Getenv).
func Get(k string) string { return Wrap(os.Getenv)(k) }
