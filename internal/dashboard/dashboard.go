// Package dashboard serves the local evidence log as a dashboard (D-053): a
// static page (plain HTML, CSS and an ES module, embedded in the agent) and
// one read-only JSON endpoint with the verified log. It listens on loopback,
// answers only GET, refuses Host names other than local ones (so a web page
// cannot read it by DNS rebinding), and its CSP lets the page load nothing
// but its own files.
package dashboard

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/localcheck"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/localreport"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/policy"
)

//go:embed static
var static embed.FS

// Options for the dashboard.
type Options struct {
	DataDir string
	Version string
	// Hosts are extra Host names the dashboard answers to, besides localhost,
	// 127.0.0.1 and [::1] (for example a container's published name).
	Hosts []string
	// PolicyPath is the tellproof.policy.json the Objectives panel checks the
	// log against, re-read on every request ("" or missing: the panel explains
	// how to create one).
	PolicyPath string
	Now        func() time.Time
}

// CheckAPI is /api/check: the evidence objectives for a range.
type CheckAPI struct {
	Available bool               `json:"available"`
	Policy    string             `json:"policy"`
	Message   string             `json:"message,omitempty"`
	Report    *localcheck.Report `json:"report,omitempty"`
}

// API is the JSON the page reads.
type API struct {
	GeneratedAt string               `json:"generated_at"`
	Version     string               `json:"version"`
	EarlyAccess string               `json:"early_access"`
	Log         *localreport.Summary `json:"log,omitempty"`
	Error       string               `json:"error,omitempty"` // no local log yet, or unreadable
}

const csp = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Handler serves the dashboard.
func Handler(o Options) http.Handler {
	files, _ := fs.Sub(static, "static")
	types := map[string]string{".html": "text/html; charset=utf-8", ".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".svg": "image/svg+xml"}
	mux := http.NewServeMux()
	serve := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, err := fs.ReadFile(files, name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", types[name[strings.LastIndex(name, "."):]])
			_, _ = w.Write(b)
		}
	}
	mux.HandleFunc("GET /{$}", serve("index.html"))
	for _, f := range []string{"dashboard.css", "dashboard.js", "brand.css", "favicon.svg"} {
		mux.HandleFunc("GET /"+f, serve(f))
	}
	mux.HandleFunc("GET /api/log", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now
		if o.Now != nil {
			now = o.Now
		}
		out := API{GeneratedAt: now().UTC().Format(time.RFC3339), Version: o.Version, EarlyAccess: localreport.EarlyAccess}
		if s, err := localreport.Read(o.DataDir, localreport.Range{}); err != nil {
			out.Error = "No local evidence log yet. It appears once the agent in local mode has received its first spans."
		} else {
			out.Log = s
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /api/check", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now
		if o.Now != nil {
			now = o.Now
		}
		out := CheckAPI{Policy: o.PolicyPath}
		pol, err := policy.Load(o.PolicyPath)
		switch {
		case o.PolicyPath == "" || errors.Is(err, os.ErrNotExist):
			out.Message = "No policy yet. Declare what this AI system may use, then the eleven evidence objectives are checked here: tellproof-agent policy init"
		case err != nil:
			out.Message = err.Error()
		default:
			loc, _ := time.LoadLocation(pol.Timezone)
			today := now().In(loc)
			days := map[string]int{"24h": 1, "7d": 6, "30d": 29}
			opts := localcheck.Options{DataDir: o.DataDir, Policy: pol, Now: now}
			if d, ok := days[r.URL.Query().Get("range")]; ok {
				opts.Since = today.AddDate(0, 0, -d).Format("2006-01-02")
			} else {
				opts.FromFirst = true // all: from the log's first day
			}
			rep, err := localcheck.Run(opts)
			switch {
			case err != nil && rep != nil && !rep.Log.Verified:
				out.Message = "The log failed verification, so nothing is evaluated: " + rep.Log.Problem
			case err != nil:
				out.Message = err.Error()
			default:
				out.Available, out.Report = true, rep
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	allowed := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	for _, h := range o.Hosts {
		allowed[strings.ToLower(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.ToLower(strings.Trim(host, "[]"))
		if !allowed[host] && !strings.HasSuffix(host, ".localhost") {
			http.Error(w, "the Tellproof dashboard answers only on localhost", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		mux.ServeHTTP(w, r)
	})
}
