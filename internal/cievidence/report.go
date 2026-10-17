package cievidence

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// VerifyAndReport opens a statement (checking the signature with the PEM
// public key, or accepting a bare statement when unsigned), checks it against
// req and writes the outcome. Exit codes: 0 every requirement met, 1 one is
// not or the statement does not verify, 2 a bad key.
func VerifyAndReport(w io.Writer, b, pubPEM []byte, unsigned bool, req Requirements, asJSON bool) (int, error) {
	var pub []byte
	if pubPEM != nil {
		k, err := ParsePublic(pubPEM)
		if err != nil {
			return 2, err
		}
		pub = k
	}
	st, signed, err := Open(b, pub, unsigned)
	if err != nil {
		return 1, err
	}
	checks := Verify(st, req)
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		err = enc.Encode(map[string]any{"commit": st.Predicate.Commit, "repository": st.Predicate.Repository, "signed": signed,
			"ok": !Failed(checks), "checks": checks, "agent_commits": st.Predicate.AgentCommits})
	} else {
		err = WriteText(w, st, signed, checks)
	}
	if err != nil {
		return 1, err
	}
	if Failed(checks) {
		return 1, nil
	}
	return 0, nil
}

// WriteText writes a verification for a person.
func WriteText(w io.Writer, st *Statement, signed bool, checks []Check) error {
	var b strings.Builder
	p := st.Predicate
	sig := "signature verified"
	if !signed {
		sig = "UNSIGNED: integrity not checked here (verify its GitHub attestation with gh attestation verify)"
	}
	fmt.Fprintf(&b, "Tellproof CI evidence: %s at %s (%s)\n", p.Repository, p.Commit, sig)
	if p.Run.URL != "" {
		fmt.Fprintf(&b, "  Run: %s\n", p.Run.URL)
	}
	for _, c := range checks {
		mark := "✓ OK  "
		switch {
		case !c.OK:
			mark = "✕ FAIL"
		case c.Note:
			mark = "! NOTE"
		}
		fmt.Fprintf(&b, "%s %-24s %s\n", mark, c.Name, c.Detail)
	}
	if a := p.AgentCommits; a != nil {
		var by []string
		for _, name := range slices.Sorted(maps.Keys(a.ByAgent)) {
			by = append(by, fmt.Sprintf("%s %d", name, a.ByAgent[name]))
		}
		line := fmt.Sprintf("Coding agents: %d of %d commits in %s name one", a.WithAgent, a.Commits, a.Range)
		if len(by) > 0 {
			line += " (" + strings.Join(by, ", ") + ")"
		}
		b.WriteString(line + ".\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
