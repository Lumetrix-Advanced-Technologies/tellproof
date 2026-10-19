package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/agent"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/dashboard"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/enrol"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/localreport"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/policy"
)

// finding is one doctor check's outcome. Fix is a command or setting that
// resolves it, when there is a problem.
type finding struct {
	Check  string `json:"check"`
	Status string `json:"status"` // ok | info | warn | fail
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// doctorNow and doctorHTTP are the clock and the client for the local
// probes; tests replace them.
var (
	doctorNow  = time.Now
	doctorHTTP = &http.Client{Timeout: 2 * time.Second}
)

// doctor runs every check it can and prints them. It talks only to the
// addresses in the agent's own configuration and the app's
// TELLPROOF_AGENT_URL, and sends no spans: the receiver probe is refused
// before anything is read.
func doctor(ctx context.Context, configPath, dataDir, policyPath string, asJSON bool, getenv func(string) string, stdout, stderr io.Writer) int {
	var fs []finding
	add := func(check, status, detail, fix string) {
		fs = append(fs, finding{Check: check, Status: status, Detail: detail, Fix: fix})
	}
	cfg, err := agent.LoadConfig(configPath, getenv)
	if err != nil {
		add("config", "fail", err.Error(), "fix the setting it names (see the README's Configuration reference)")
		return printFindings(fs, asJSON, stdout, stderr)
	}
	src := "defaults and environment"
	if configPath != "" {
		src = configPath
	}
	add("config", "ok", "loaded from "+src, "")
	if dataDir == "" {
		dataDir = cfg.DataDir
	}
	if policyPath == "" {
		policyPath = policy.Find(getenv, dataDir)
	}

	// The data directory, its identity and its secrets.
	mode := "local"
	if cfg.Server != "" {
		mode = "connected"
	}
	if fi, err := os.Stat(dataDir); err != nil {
		add("data directory", "info", dataDir+" does not exist yet; the agent creates it on its first run", "")
	} else if !fi.IsDir() {
		add("data directory", "fail", dataDir+" is not a directory", "set TELLPROOF_DATA_DIR (or data_dir) to a directory")
	} else {
		perm(dataDir, fi, 0o077, "data directory", "chmod 700 "+dataDir, add)
		for _, f := range []string{enrol.KeyFile, "salt.secret"} {
			if fi, err := os.Stat(filepath.Join(dataDir, f)); err == nil {
				perm(filepath.Join(dataDir, f), fi, 0o077, f, "chmod 600 "+filepath.Join(dataDir, f), add)
			}
		}
		if id, err := enrol.Load(dataDir); err == nil {
			switch {
			case id.IsLocal() && mode == "connected":
				add("identity", "fail", fmt.Sprintf("%s holds a local-mode agent (%s); connect and enrol refuse it", dataDir, id.AgentID),
					"use a new data directory (or volume) for connected mode")
			case !id.IsLocal() && mode == "local":
				add("identity", "fail", fmt.Sprintf("%s is enrolled with %s as %s; run --local refuses it", dataDir, id.Server, id.AgentID),
					"use another data directory for local mode, or run it connected: tellproof-agent connect")
			default:
				add("identity", "ok", fmt.Sprintf("%s agent %s", mode, id.AgentID), "")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			add("identity", "fail", err.Error(), "restore the data directory from a backup, or start a new one")
		}
		if ignored, inRepo := gitIgnored(".", dataDir); inRepo && !ignored {
			add("git", "warn", dataDir+" is inside a git work tree and not ignored: the agent's private key could be committed",
				"add it to .gitignore (tellproof-agent init offers to)")
		}
	}
	switch {
	case getenv(cfg.SaltSecretEnv) != "":
		add("salt secret", "ok", "from $"+cfg.SaltSecretEnv, "")
	case cfg.SaltSecretFile != "":
		if fi, err := os.Stat(cfg.SaltSecretFile); err != nil {
			add("salt secret", "fail", cfg.SaltSecretFile+" cannot be read: "+err.Error(), "create it (32 random bytes, hex) with mode 600, or set $"+cfg.SaltSecretEnv)
		} else {
			perm(cfg.SaltSecretFile, fi, 0o077, "salt secret", "chmod 600 "+cfg.SaltSecretFile, add)
		}
	default:
		if _, err := os.Stat(filepath.Join(dataDir, "salt.secret")); err == nil {
			add("salt secret", "ok", "kept in "+filepath.Join(dataDir, "salt.secret"), "")
		} else {
			add("salt secret", "info", "none yet; run --local and connect keep a random one in the data directory", "")
		}
	}

	// The span receiver, and whether the app's SDK will reach it.
	running := probeReceiver(ctx, cfg.OTLPListen, add)
	agentURL := getenv("TELLPROOF_AGENT_URL")
	want := "http://" + cfg.OTLPListen
	if host, port, err := net.SplitHostPort(cfg.OTLPListen); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
			want = "http://" + net.JoinHostPort("127.0.0.1", port)
		}
	}
	if agentURL == "" {
		if _, port, _ := net.SplitHostPort(cfg.OTLPListen); port != "4318" {
			add("SDK target", "warn", "the SDKs send to http://127.0.0.1:4318 by default, but the agent listens on "+cfg.OTLPListen,
				"in the app's environment: TELLPROOF_AGENT_URL="+want)
		} else {
			add("SDK target", "ok", "the SDKs' default, http://127.0.0.1:4318, is where the agent listens (set TELLPROOF_AGENT_URL in the app if it runs elsewhere)", "")
		}
	} else if u, err := url.Parse(agentURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("SDK target", "fail", "TELLPROOF_AGENT_URL is not an http(s) URL", "TELLPROOF_AGENT_URL="+want)
	} else if agentURL = u.Redacted(); strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1/traces") { // never print a password
		add("SDK target", "fail", "TELLPROOF_AGENT_URL ends in /v1/traces, which the SDKs add themselves", "TELLPROOF_AGENT_URL="+strings.TrimSuffix(strings.TrimRight(agentURL, "/"), "/v1/traces"))
	} else if !sameEndpoint(u, cfg.OTLPListen) {
		add("SDK target", "warn", fmt.Sprintf("TELLPROOF_AGENT_URL is %s but this agent listens on %s; fine if the app reaches it through another address", agentURL, cfg.OTLPListen),
			"TELLPROOF_AGENT_URL="+want)
	} else {
		add("SDK target", "ok", "TELLPROOF_AGENT_URL="+agentURL+" is this agent's receiver", "")
	}

	// The dashboard (local mode only).
	if mode == "local" && cfg.DashboardListen != "" && cfg.DashboardListen != "off" {
		probeDashboard(ctx, cfg.DashboardListen, running, add)
	}

	// The evidence log.
	s, err := localreport.Read(dataDir, localreport.Range{})
	switch {
	case mode == "connected":
		add("evidence log", "info", "connected mode: batches go to "+cfg.Server+" (the Tellproof app shows them)", "")
	case err != nil || s.Batches == 0:
		add("evidence log", "info", "no batches yet: the first comes about a minute after the agent receives its first span", "start the agent ("+progName()+" start), then make a model call in the app")
	case !s.Verified:
		add("evidence log", "fail", "verification failed: "+s.Problem+"; figures cover only the batches before it", "restore the data directory from a backup (tellproof-agent report names the batch)")
	default:
		add("evidence log", "ok", fmt.Sprintf("%d batches verify, last window ending %s", s.Batches, s.To), "")
		now := doctorNow()
		if to, err := time.Parse(time.RFC3339, s.To); err == nil {
			var newest time.Time
			if len(s.Recent) > 0 {
				newest, _ = time.Parse(time.RFC3339, s.Recent[0].WindowStart)
			}
			if newest.After(now.Add(10 * time.Minute)) {
				add("clock", "fail", fmt.Sprintf("the newest batch starts at %s, after this machine's time (%s)", newest.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339)),
					"set the system clock (NTP); windows are hourly by the clock")
			} else if age := now.Sub(to); age > 2*time.Hour {
				add("spans", "warn", fmt.Sprintf("no batch for %s: the agent has received no spans since %s", age.Truncate(time.Hour), s.To),
					"check that the app is running and calling tellproof.init(); tellproof.stats() in the app shows exported and export_errors")
			}
		}
		if s.LLMCalls == 0 {
			add("model calls", "warn", "batches arrive but record no model calls",
				"in the app, tellproof.stats()[\"instrumented\"] lists the instrumented client libraries; in Node.js ES modules await tellproof.ready(); or report calls with llm_call / llmCall")
		}
		if s.ContentDropped > 0 {
			add("content", "info", fmt.Sprintf("%d spans arrived with prompt or completion text; the agent dropped the text when it parsed them, and none of it is kept or sent", s.ContentDropped), "")
		}
	}

	// The policy.
	if p, err := policy.Load(policyPath); errors.Is(err, os.ErrNotExist) {
		add("policy", "info", "no policy yet ("+policyPath+")", "tellproof-agent policy init, then tellproof-agent check")
	} else if err != nil {
		add("policy", "fail", err.Error(), "fix the field it names, then tellproof-agent policy validate")
	} else if len(p.DeclaredModels) == 0 && s != nil && len(s.Models) > 0 {
		add("policy", "warn", fmt.Sprintf("%s declares no models, but the log has seen %d", policyPath, len(s.Models)),
			"tellproof-agent policy init --force, then review the file")
	} else {
		add("policy", "ok", fmt.Sprintf("%s is valid: %s, %d models and %d regions declared", policyPath, p.AISystem, len(p.DeclaredModels), len(p.DeclaredRegions)), "")
	}
	return printFindings(fs, asJSON, stdout, stderr)
}

// perm reports a file or directory others can read.
func perm(path string, fi os.FileInfo, mask os.FileMode, check, fix string, add func(check, status, detail, fix string)) {
	if runtime.GOOS == "windows" {
		return // ACLs, not mode bits
	}
	if fi.Mode().Perm()&mask != 0 {
		add(check, "warn", fmt.Sprintf("%s is readable by other users (%04o)", path, fi.Mode().Perm()), fix)
		return
	}
	if check == "data directory" {
		add(check, "ok", path+", private to this user", "")
	}
}

// sameEndpoint reports whether u points at the listen address: the same
// port, and the same host or both loopback (or the agent on every interface).
func sameEndpoint(u *url.URL, listen string) bool {
	lh, lp, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	if port != lp {
		return false
	}
	loop := func(h string) bool {
		ip := net.ParseIP(h)
		return h == "localhost" || strings.HasSuffix(h, ".localhost") || ip != nil && ip.IsLoopback()
	}
	if ip := net.ParseIP(lh); ip != nil && ip.IsUnspecified() {
		return true
	}
	return u.Hostname() == lh || loop(u.Hostname()) && loop(lh)
}

// probeReceiver reports whether the agent answers on addr. It posts no spans:
// a protobuf content type, which the receiver refuses before reading the body.
func probeReceiver(ctx context.Context, addr string, add func(check, status, detail, fix string)) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+dialAddr(addr)+"/v1/traces", strings.NewReader(""))
	if err != nil {
		add("receiver", "fail", err.Error(), "")
		return false
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := doctorHTTP.Do(req)
	if err != nil {
		if free(addr) {
			add("receiver", "warn", "the agent is not running: nothing listens on "+addr, "start it: "+progName()+" start")
		} else {
			add("receiver", "fail", addr+" is taken, but not by an HTTP server", "stop what holds the port, or set TELLPROOF_OTLP_LISTEN (and TELLPROOF_AGENT_URL in the app)")
		}
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnsupportedMediaType && strings.Contains(string(body), "OTLP") {
		add("receiver", "ok", "the agent answers on http://"+dialAddr(addr)+"/v1/traces", "")
		return true
	}
	add("receiver", "fail", fmt.Sprintf("something else answers on %s (HTTP %d), so the app's spans would go there", addr, resp.StatusCode),
		"stop it, or move the agent: TELLPROOF_OTLP_LISTEN=127.0.0.1:4328 and, in the app, TELLPROOF_AGENT_URL=http://127.0.0.1:4328")
	return false
}

// probeDashboard reports whether the local dashboard answers on addr and
// which agent version serves it.
func probeDashboard(ctx context.Context, addr string, agentRunning bool, add func(check, status, detail, fix string)) {
	u := "http://" + dialAddr(addr) + "/api/log"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		add("dashboard", "fail", err.Error(), "")
		return
	}
	resp, err := doctorHTTP.Do(req)
	if err != nil {
		switch {
		case free(addr) && agentRunning:
			add("dashboard", "warn", "the agent runs but nothing serves the dashboard on "+addr+" (an agent not in local mode, or with dashboard_listen set elsewhere)", "tellproof-agent dashboard")
		case free(addr):
			add("dashboard", "info", "not running; "+progName()+" start serves it on http://"+dialAddr(addr)+"/", "")
		default:
			add("dashboard", "fail", "port "+addr+" is taken by something else", "TELLPROOF_DASHBOARD_LISTEN=127.0.0.1:4329 (or off)")
		}
		return
	}
	defer resp.Body.Close()
	var api dashboard.API
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&api) != nil || api.Version == "" {
		add("dashboard", "fail", fmt.Sprintf("something else answers on %s (HTTP %d)", addr, resp.StatusCode), "TELLPROOF_DASHBOARD_LISTEN=127.0.0.1:4329 (or off)")
		return
	}
	if api.Version != version {
		add("dashboard", "warn", fmt.Sprintf("http://%s/ is served by tellproof-agent %s, but this command is %s", dialAddr(addr), api.Version, version),
			"restart the agent with the version you installed")
		return
	}
	add("dashboard", "ok", "http://"+dialAddr(addr)+"/", "")
}

// dialAddr is addr with an unspecified host made loopback.
func dialAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// free reports whether addr could be listened on now.
func free(addr string) bool {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func printFindings(fs []finding, asJSON bool, stdout, stderr io.Writer) int {
	failed := false
	for _, f := range fs {
		failed = failed || f.Status == "fail"
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"version": version, "ok": !failed, "checks": fs}); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	} else {
		icons := map[string]string{"ok": "✓ ok  ", "info": "· info", "warn": "! warn", "fail": "✗ FAIL"}
		fmt.Fprintf(stdout, "tellproof-agent %s doctor\n\n", version)
		for _, f := range fs {
			fmt.Fprintf(stdout, "  %s  %-15s %s\n", icons[f.Status], f.Check, f.Detail)
			if f.Fix != "" && f.Status != "ok" {
				fmt.Fprintf(stdout, "  %6s  %-15s → %s\n", "", "", f.Fix)
			}
		}
		var warn int
		for _, f := range fs {
			if f.Status == "warn" {
				warn++
			}
		}
		switch {
		case failed:
			fmt.Fprintln(stdout, "\nSomething needs fixing: see the FAIL lines.")
		case warn > 0:
			fmt.Fprintf(stdout, "\nNothing failed; %d warning(s).\n", warn)
		default:
			fmt.Fprintln(stdout, "\nAll good.")
		}
		fmt.Fprintln(stdout, "The app's side: tellproof.stats() shows spans, exported, dropped and export_errors.")
	}
	if failed {
		return 1
	}
	return 0
}
