package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, method, target, host string) *http.Response {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result()
}

func TestServesThePageAndTheAPI(t *testing.T) {
	h := Handler(Options{DataDir: t.TempDir(), Version: "v-test"})
	for path, ctype := range map[string]string{"/": "text/html", "/dashboard.js": "text/javascript", "/dashboard.css": "text/css", "/brand.css": "text/css"} {
		r := get(t, h, "GET", path, "127.0.0.1:4319")
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), ctype) {
			t.Errorf("%s: %d %s", path, r.StatusCode, r.Header.Get("Content-Type"))
		}
		if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q", path, csp)
		}
	}
	// No log yet: the API says so instead of failing.
	r := get(t, h, "GET", "/api/log", "localhost:4319")
	var api API
	if err := json.NewDecoder(r.Body).Decode(&api); err != nil || api.Log != nil || api.Error == "" || api.Version != "v-test" || api.EarlyAccess == "" {
		t.Fatalf("api without a log: %v %+v", err, api)
	}
	if r := get(t, h, "GET", "/nothing", "localhost"); r.StatusCode != 404 {
		t.Errorf("unknown path: %d", r.StatusCode)
	}
}

// A web page cannot read the dashboard by pointing its own name at 127.0.0.1
// (DNS rebinding), and nothing can change through it.
func TestRefusesOtherHostsAndWrites(t *testing.T) {
	h := Handler(Options{DataDir: t.TempDir()})
	for _, host := range []string{"evil.example", "evil.example:4319", "192.168.1.5:4319", ""} {
		if r := get(t, h, "GET", "/api/log", host); r.StatusCode != http.StatusForbidden {
			t.Errorf("host %q: %d", host, r.StatusCode)
		}
	}
	for _, host := range []string{"localhost", "127.0.0.1:4319", "[::1]:4319", "plumbline.localhost:4319"} {
		if r := get(t, h, "GET", "/api/log", host); r.StatusCode != 200 {
			t.Errorf("host %q: %d", host, r.StatusCode)
		}
	}
	if r := get(t, Handler(Options{DataDir: t.TempDir(), Hosts: []string{"agent.internal"}}), "GET", "/", "agent.internal:4319"); r.StatusCode != 200 {
		t.Errorf("an extra host: %d", r.StatusCode)
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		if r := get(t, h, m, "/api/log", "localhost"); r.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d", m, r.StatusCode)
		}
	}
}

// The page uses no inline script or style (the CSP forbids them), loads
// nothing from elsewhere, and never puts log text into innerHTML.
func TestThePageStaysWithinItsCSP(t *testing.T) {
	html, _ := os.ReadFile(filepath.Join("static", "index.html"))
	js, _ := os.ReadFile(filepath.Join("static", "dashboard.js"))
	if regexp.MustCompile(`<script>|<style|\son[a-z]+=|style="`).Match(html) {
		t.Error("index.html has inline script, style or handlers")
	}
	if regexp.MustCompile(`(src|href)="(https?:)?//`).Match(html) {
		t.Error("index.html loads something from another origin")
	}
	if bytes.Contains(js, []byte("innerHTML")) || bytes.Contains(js, []byte("eval(")) {
		t.Error("dashboard.js uses innerHTML or eval")
	}
}

// The dashboard carries a copy of the brand tokens (embed cannot reach web/); it must not drift.
func TestBrandTokensMatchTheSite(t *testing.T) {
	site, err := os.ReadFile(filepath.Join("..", "..", "web", "brand.css"))
	if err != nil {
		t.Fatal(err)
	}
	f, _ := static.Open("static/brand.css")
	mine, _ := io.ReadAll(f)
	if !bytes.Equal(site, mine) {
		t.Error("internal/dashboard/static/brand.css differs from web/brand.css: copy it over")
	}
}
