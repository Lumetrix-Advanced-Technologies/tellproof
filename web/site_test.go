package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The static site's content pages (P6-3): no scripts, a strict CSP, every
// local link and anchor resolves, and the copy keeps the brand's rules.
var sitePages = []string{"index.html", "security/index.html", "docs/agent/index.html"}

var (
	hrefRE   = regexp.MustCompile(`(?:href|src)="([^"]*)"`)
	idRE     = regexp.MustCompile(`\bid="([^"]+)"`)
	scriptRE = regexp.MustCompile(`(?i)<script|\son[a-z]+=|javascript:`)
	bannedRE = regexp.MustCompile(`(?i)\b(certified|compliant|guaranteed|bank-grade|ai-powered)\b`)
)

func TestSitePages(t *testing.T) {
	for _, page := range sitePages {
		b, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		html := string(b)
		if !strings.Contains(html, `http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'self';`) {
			t.Errorf("%s: no strict CSP", page)
		}
		if scriptRE.MatchString(html) {
			t.Errorf("%s: has script", page)
		}
		if !strings.Contains(html, `<html lang="en-AU">`) || !regexp.MustCompile(`<title>[^<]+</title>`).MatchString(html) {
			t.Errorf("%s: no language or title", page)
		}
		if m := bannedRE.FindString(html); m != "" {
			t.Errorf("%s: says %q (docs/code-standards.md, UI copy)", page, m)
		}
		ids := map[string]bool{}
		for _, m := range idRE.FindAllStringSubmatch(html, -1) {
			ids[m[1]] = true
		}
		for _, m := range hrefRE.FindAllStringSubmatch(html, -1) {
			link := m[1]
			switch {
			case strings.HasPrefix(link, "https://"):
				continue
			case strings.HasPrefix(link, "#"):
				if !ids[link[1:]] {
					t.Errorf("%s: anchor %s has no target", page, link)
				}
				continue
			case strings.Contains(link, "://"), strings.HasPrefix(link, "/"):
				t.Errorf("%s: link %s is not relative or https", page, link)
				continue
			}
			target := filepath.Join(filepath.Dir(page), link)
			if strings.HasSuffix(link, "/") || link == "." || link == "./" {
				target = filepath.Join(target, "index.html")
			}
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s: link %s does not resolve (%s)", page, link, target)
			}
		}
	}
}
