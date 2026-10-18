package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The banner fits the terminal it is drawn for, in every colour mode, and
// carries the name and version (D-061).
func TestBannerFitsTheTerminal(t *testing.T) {
	for _, mode := range []int{0, 1, 2} {
		for _, light := range []bool{false, true} {
			p := palette{mode: mode, light: light}
			for _, width := range []int{120, 80, 79, 60, 44, 43, 20} {
				out := renderBanner(p, width, "v9.9.9", "Evidence agent")
				limit := width
				if width >= bannerWide {
					limit = bannerWide
				}
				for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
					if v := visible(line); v > limit && width > 43 {
						t.Errorf("mode %d width %d line %d is %d wide: %q", mode, width, i, v, line)
					}
				}
				if !strings.Contains(out, "v9.9.9") {
					t.Errorf("mode %d width %d: no version", mode, width)
				}
				switch {
				case width >= bannerWide:
					if !strings.Contains(out, "Welcome to") || strings.Count(out, "\n") < 12 {
						t.Errorf("width %d: not the full banner:\n%s", width, out)
					}
				case width >= 44:
					if !strings.Contains(out, "Tellproof") || strings.Contains(out, "Welcome to") {
						t.Errorf("width %d: not the compact banner:\n%s", width, out)
					}
				default:
					if strings.Count(out, "\n") != 1 {
						t.Errorf("width %d: not one line:\n%s", width, out)
					}
				}
				if (mode == 0) != !strings.Contains(out, "\x1b[") {
					t.Errorf("mode %d: escape codes present = %v", mode, strings.Contains(out, "\x1b["))
				}
			}
		}
	}
	if w := welcomeText(palette{mode: 2}, 80); !strings.Contains(w, "run --local") {
		t.Errorf("welcome has no first steps:\n%s", w)
	} else {
		for _, line := range strings.Split(w, "\n") {
			if visible(line) > 80 {
				t.Errorf("welcome line wider than 80: %q", line)
			}
		}
	}
}

// Scripts, pipes, files, CI and opted-out users never see it; with no
// arguments and no terminal, the agent behaves as it always did.
func TestBannerOnlyInATerminal(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	if ok, _, _ := terminal(&bytes.Buffer{}, env()); ok {
		t.Error("a buffer is not a terminal")
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if ok, _, _ := terminal(f, env()); ok {
		t.Error("a file is not a terminal")
	}
	for _, kv := range [][]string{{"TELLPROOF_NO_BANNER", "1"}, {"CI", "true"}, {"TERM", "dumb"}} {
		if ok, _, _ := terminal(os.Stdout, env(kv...)); ok {
			t.Errorf("%s=%s still shows the banner", kv[0], kv[1])
		}
	}
	var out, errb bytes.Buffer
	code := run(context.Background(), nil, env("TELLPROOF_DATA_DIR", t.TempDir()), &out, &errb)
	if strings.Contains(out.String(), "Welcome") || code == 0 && out.Len() > 0 && strings.Contains(out.String(), "Get started") {
		t.Errorf("no arguments without a terminal showed the welcome: %d %q", code, out.String())
	}
}
