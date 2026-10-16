// Package agent runs the evidence agent (docs/architecture.md §3): it receives
// spans (OTLP JSON) and pulls them (Langfuse), aggregates them per hour, polls
// provider usage, commits to the hour's traces, signs a batch and delivers it
// from a durable queue to one server. Only aggregates and salted roots leave;
// --dry-run prints exactly what would be sent and sends nothing.
package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/lumetrix-advanced-technologies/plumbline/internal/aggregate"
	"github.com/lumetrix-advanced-technologies/plumbline/internal/langfuse"
)

// Config is the agent's JSON config file (agent.example.json). Secrets are
// never in it: it names the environment variables (or files) that hold them.
type Config struct {
	Server          string `json:"server"`
	DataDir         string `json:"data_dir"`
	OTLPListen      string `json:"otlp_listen"`
	OTLPAllowRemote bool   `json:"otlp_allow_remote"` // listen beyond loopback (the receiver has no auth)
	Langfuse        struct {
		Enabled      bool        `json:"enabled"`
		BaseURL      string      `json:"base_url"`
		PublicKeyEnv string      `json:"public_key_env"`
		SecretKeyEnv string      `json:"secret_key_env"`
		EvalScores   []EvalScore `json:"eval_scores"`
	} `json:"langfuse"`
	Usage struct {
		OpenAI struct {
			Enabled     bool     `json:"enabled"`
			AdminKeyEnv string   `json:"admin_key_env"`
			ProjectIDs  []string `json:"project_ids"`
		} `json:"openai"`
		Anthropic struct {
			Enabled      bool     `json:"enabled"`
			AdminKeyEnv  string   `json:"admin_key_env"`
			WorkspaceIDs []string `json:"workspace_ids"`
		} `json:"anthropic"`
		Bedrock struct {
			Enabled bool   `json:"enabled"`
			Region  string `json:"region"`
		} `json:"bedrock"`
	} `json:"usage"`
	SaltSecretEnv  string `json:"salt_secret_env"`
	SaltSecretFile string `json:"salt_secret_file"`
	Aggregation    struct {
		GraceMinutes int `json:"grace_minutes"`
		// FirstBatchSeconds: after the first spans arrive, send what has been
		// aggregated so far this soon (once per start), so a new agent shows up
		// in minutes rather than after the hour (S-5). 0 turns it off.
		FirstBatchSeconds int `json:"first_batch_seconds"`
		Anomaly           struct {
			MinCalls           int     `json:"min_calls"`
			ErrorRate          float64 `json:"error_rate"`
			GuardrailBlockRate float64 `json:"guardrail_block_rate"`
		} `json:"anomaly"`
	} `json:"aggregation"`
}

// EvalScore is a Langfuse score that records an eval suite (langfuse.EvalScore).
type EvalScore struct {
	Name        string  `json:"name"`
	Suite       string  `json:"suite"`
	PassAtLeast float64 `json:"pass_at_least"`
}

func (e EvalScore) toLangfuse() langfuse.EvalScore {
	return langfuse.EvalScore{Name: e.Name, Suite: e.Suite, PassAtLeast: e.PassAtLeast}
}

// LoadConfig reads a config file (empty path: defaults only), then applies
// the environment overrides PLUMBLINE_SERVER, PLUMBLINE_DATA_DIR and
// PLUMBLINE_OTLP_LISTEN, and validates the result.
func LoadConfig(path string, getenv func(string) string) (Config, error) {
	c := Config{DataDir: "./.plumbline-agent", OTLPListen: "127.0.0.1:4318", SaltSecretEnv: "PLUMBLINE_SALT_SECRET"}
	d := aggregate.DefaultConfig()
	c.Aggregation.GraceMinutes = int(d.Grace / time.Minute)
	c.Aggregation.FirstBatchSeconds = 60
	c.Aggregation.Anomaly.MinCalls, c.Aggregation.Anomaly.ErrorRate, c.Aggregation.Anomaly.GuardrailBlockRate = d.MinCalls, d.ErrorRate, d.GuardrailBlock
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return c, fmt.Errorf("agent: config: %w", err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return c, fmt.Errorf("agent: config %s: %w", path, err)
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return c, fmt.Errorf("agent: config %s: unexpected data after the JSON document", path)
		}
	}
	for env, field := range map[string]*string{"PLUMBLINE_SERVER": &c.Server, "PLUMBLINE_DATA_DIR": &c.DataDir, "PLUMBLINE_OTLP_LISTEN": &c.OTLPListen} {
		if v := getenv(env); v != "" {
			*field = v
		}
	}
	// The container image listens on all interfaces inside its network namespace;
	// the docker run line publishes the port on the host's loopback only.
	if getenv("PLUMBLINE_OTLP_ALLOW_REMOTE") == "1" {
		c.OTLPAllowRemote = true
	}
	return c, c.validate()
}

func (c Config) validate() error {
	if c.Server != "" {
		u, err := url.Parse(c.Server)
		if err != nil || u.Host == "" {
			return fmt.Errorf("agent: server %q is not a URL", c.Server)
		}
		h := u.Hostname()
		local := h == "localhost" || (net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback())
		if u.Scheme != "https" && !(u.Scheme == "http" && local) {
			return errors.New("agent: server must use https (plain http is allowed only for localhost)")
		}
	}
	host, _, err := net.SplitHostPort(c.OTLPListen)
	if err != nil {
		return fmt.Errorf("agent: otlp_listen %q: %w", c.OTLPListen, err)
	}
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !loopback && !c.OTLPAllowRemote {
		return fmt.Errorf("agent: otlp_listen %q is not a loopback address; the receiver has no authentication, so set otlp_allow_remote only behind a network you control", c.OTLPListen)
	}
	if c.Aggregation.GraceMinutes < 1 || c.Aggregation.GraceMinutes > 55 {
		return errors.New("agent: aggregation.grace_minutes must be between 1 and 55")
	}
	if c.Aggregation.FirstBatchSeconds < 0 || c.Aggregation.FirstBatchSeconds > 3600 {
		return errors.New("agent: aggregation.first_batch_seconds must be between 0 and 3600")
	}
	if c.DataDir == "" {
		return errors.New("agent: data_dir is required")
	}
	return nil
}

// secret reads the value of a named environment variable; the error names the
// variable, never a value.
func secret(getenv func(string) string, envName, what string) (string, error) {
	if envName == "" {
		return "", fmt.Errorf("agent: %s is enabled but no environment variable is named for its key", what)
	}
	v := getenv(envName)
	if v == "" {
		return "", fmt.Errorf("agent: %s is enabled but %s is not set", what, envName)
	}
	return v, nil
}
