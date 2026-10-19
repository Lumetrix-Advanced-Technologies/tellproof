package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/aggregate"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/batchsign"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/commitment"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/dsse"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/enrol"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/langfuse"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/localreport"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/merkle"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/otlp"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/queue"
	"github.com/lumetrix-advanced-technologies/tellproof/internal/usage"
)

// Options are the run-time choices that are not configuration.
type Options struct {
	DryRun bool // print batches to Out; send, queue and store nothing
	// Local keeps signed batches in the data directory's local/ log instead
	// of sending them (D-052): no server, no enrolment, a key of its own.
	Local  bool
	Out    io.Writer        // dry-run output (default os.Stdout)
	Log    *slog.Logger     // default: JSON to stderr
	Now    func() time.Time // default time.Now
	HTTP   *http.Client     // for usage, Langfuse and delivery (tests)
	Getenv func(string) string
	// TickEvery is how often Run closes windows and delivers; default one minute.
	TickEvery time.Duration
}

// Agent is one running evidence agent.
type Agent struct {
	cfg     Config
	opt     Options
	id      *enrol.Identity
	secret  []byte
	mapping *otlp.Mapping
	agg     *aggregate.Aggregator
	builder *batchsign.Builder
	queue   *queue.Queue // nil in dry-run
	sender  *queue.Sender
	pollers []usage.Poller
	lf      *langfuse.Puller
	grace   time.Duration

	mu         sync.Mutex
	dryState   batchsign.State // chain state of printed batches in dry-run
	pulledThru time.Time       // Langfuse hours pulled up to here
	firstSpans time.Time       // when the first spans of this run arrived
	firstSent  bool            // the early first batch has been sent

	onListening func() // called by Run once the receiver listens
}

// OnListening sets a function Run calls once the OTLP receiver listens.
func (a *Agent) OnListening(f func()) { a.onListening = f }

// LocalDir is the local evidence log in the data directory (localreport reads it).
const LocalDir = localreport.Dir

// DryRunAgentID is the identity a dry run uses before enrolment.
const DryRunAgentID = "agt_dryrun0000000000"

// New builds an agent from config. In dry-run it works without enrolment
// (an ephemeral key) and touches nothing on disk.
func New(cfg Config, opt Options) (*Agent, error) {
	if opt.Out == nil {
		opt.Out = os.Stdout
	}
	if opt.Log == nil {
		opt.Log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Getenv == nil {
		opt.Getenv = os.Getenv
	}
	a := &Agent{cfg: cfg, opt: opt, grace: time.Duration(cfg.Aggregation.GraceMinutes) * time.Minute}
	var err error
	if a.mapping, err = otlp.DefaultMapping(); err != nil {
		return nil, err
	}
	if a.secret, err = commitment.LoadSecret(opt.Getenv(cfg.SaltSecretEnv), cfg.SaltSecretFile); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	if opt.DryRun {
		if a.id, err = enrol.Load(cfg.DataDir); err != nil {
			_, priv, _ := ed25519.GenerateKey(rand.Reader)
			a.id = &enrol.Identity{AgentID: DryRunAgentID, Key: priv}
		}
	} else if opt.Local {
		if a.id, err = enrol.LocalIdentity(cfg.DataDir); err != nil {
			return nil, err
		}
		if a.queue, err = queue.Open(filepath.Join(cfg.DataDir, LocalDir)); err != nil {
			return nil, err
		}
	} else {
		if cfg.Server == "" {
			return nil, errors.New("agent: server is required (or use --dry-run)")
		}
		if a.id, err = enrol.Load(cfg.DataDir); err != nil {
			return nil, err
		}
		if a.id.IsLocal() {
			return nil, errors.New("agent: this data directory holds a local-mode agent; connect to a server with a new data directory")
		}
		if a.queue, err = queue.Open(filepath.Join(cfg.DataDir, "queue")); err != nil {
			return nil, err
		}
		a.sender = &queue.Sender{Queue: a.queue, Server: cfg.Server, HTTP: opt.HTTP}
	}
	if a.builder, err = batchsign.NewBuilder(a.id, a.mapping.Version); err != nil {
		return nil, err
	}
	d := aggregate.DefaultConfig()
	d.Grace = a.grace
	d.MinCalls, d.ErrorRate, d.GuardrailBlock = cfg.Aggregation.Anomaly.MinCalls, cfg.Aggregation.Anomaly.ErrorRate, cfg.Aggregation.Anomaly.GuardrailBlockRate
	a.agg = aggregate.New(d)
	if err := a.setupSources(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Agent) setupSources() error {
	u := a.cfg.Usage
	if u.OpenAI.Enabled {
		k, err := secret(a.opt.Getenv, u.OpenAI.AdminKeyEnv, "usage.openai")
		if err != nil {
			return err
		}
		a.pollers = append(a.pollers, &usage.OpenAI{AdminKey: k, ProjectIDs: u.OpenAI.ProjectIDs, HTTP: a.opt.HTTP})
	}
	if u.Anthropic.Enabled {
		k, err := secret(a.opt.Getenv, u.Anthropic.AdminKeyEnv, "usage.anthropic")
		if err != nil {
			return err
		}
		a.pollers = append(a.pollers, &usage.Anthropic{AdminKey: k, WorkspaceIDs: u.Anthropic.WorkspaceIDs, HTTP: a.opt.HTTP})
	}
	if u.Bedrock.Enabled {
		p, err := newBedrock(u.Bedrock.Region)
		if err != nil {
			return err
		}
		a.pollers = append(a.pollers, p)
	}
	if lf := a.cfg.Langfuse; lf.Enabled {
		pk, err := secret(a.opt.Getenv, lf.PublicKeyEnv, "langfuse")
		if err != nil {
			return err
		}
		sk, err := secret(a.opt.Getenv, lf.SecretKeyEnv, "langfuse")
		if err != nil {
			return err
		}
		var evals []langfuse.EvalScore
		for _, e := range lf.EvalScores {
			evals = append(evals, e.toLangfuse())
		}
		a.lf = &langfuse.Puller{BaseURL: lf.BaseURL, PublicKey: pk, SecretKey: sk, HTTP: a.opt.HTTP, Mapping: a.mapping, EvalScores: evals}
	}
	return nil
}

// AgentID is the identity batches are signed under.
func (a *Agent) AgentID() string { return a.id.AgentID }

// Receiver is the OTLP/HTTP JSON endpoint; mount it at /v1/traces.
func (a *Agent) Receiver() http.Handler {
	return &otlp.Receiver{Mapping: a.mapping, Sink: func(r otlp.Result) {
		now := a.opt.Now()
		a.agg.Add(r.Spans, now)
		a.mu.Lock()
		if a.firstSpans.IsZero() && len(r.Spans) > 0 {
			a.firstSpans = now
		}
		a.mu.Unlock()
	}}
}

// LocalHostOnly refuses requests whose Host is not a local name: localhost,
// a *.localhost name or a loopback address. A receiver that listens on
// loopback only is reached under such a name by every real client; a web page
// that rebinds its own domain to 127.0.0.1 is not, so it cannot post spans
// into the evidence (a JSON POST from another origin is already refused, as
// the receiver answers no CORS preflight).
func LocalHostOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if hh, _, err := net.SplitHostPort(host); err == nil {
			host = hh
		}
		host = strings.ToLower(strings.Trim(host, "[]"))
		ip := net.ParseIP(host)
		if host != "localhost" && !strings.HasSuffix(host, ".localhost") && (ip == nil || !ip.IsLoopback()) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"code":403,"message":"this agent accepts spans only under a local host name (127.0.0.1 or localhost); set otlp_allow_remote to receive from other hosts"}`+"\n")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Replay feeds an OTLP JSON export (one request per line, or one document)
// through the agent, as if it had been received.
func (a *Agent) Replay(r io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(r, 1<<30))
	if err != nil {
		return err
	}
	lines := bytes.Split(data, []byte("\n"))
	if json.Valid(data) {
		lines = [][]byte{data}
	}
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		res, err := otlp.Parse(line, a.mapping)
		if err != nil {
			return fmt.Errorf("agent: replay line %d: %w", i+1, err)
		}
		a.agg.Add(res.Spans, a.opt.Now())
	}
	return nil
}

// Tick pulls Langfuse hours that have ended, then closes every window past its
// grace and emits a batch for each. Call it about once a minute.
func (a *Agent) Tick(ctx context.Context) error {
	now := a.opt.Now()
	if err := a.pullLangfuse(ctx, now); err != nil {
		a.opt.Log.Warn("langfuse pull failed; will retry", "error", err.Error())
	}
	if err := a.emit(ctx, a.agg.Flush(now)); err != nil {
		return err
	}
	// Once per start: send the open hour early so the server sees a new agent within minutes.
	a.mu.Lock()
	due := !a.firstSent && !a.firstSpans.IsZero() && a.cfg.Aggregation.FirstBatchSeconds > 0 &&
		!now.Before(a.firstSpans.Add(time.Duration(a.cfg.Aggregation.FirstBatchSeconds)*time.Second))
	if due {
		a.firstSent = true
	}
	a.mu.Unlock()
	if due {
		a.opt.Log.Info("sending the first batch early; later batches follow each hour")
		return a.emit(ctx, a.agg.FlushOpen())
	}
	return nil
}

// Shutdown closes every open window and emits it.
func (a *Agent) Shutdown(ctx context.Context) error {
	return a.emit(ctx, a.agg.FlushAll())
}

// pullLangfuse pulls each hour that ended at least a minute ago, before the
// hour's window closes at its grace.
func (a *Agent) pullLangfuse(ctx context.Context, now time.Time) error {
	if a.lf == nil {
		return nil
	}
	a.mu.Lock()
	from := a.pulledThru
	a.mu.Unlock()
	last := now.Add(-time.Minute).UTC().Truncate(time.Hour) // hours before this have ended
	if from.IsZero() || from.Before(last.Add(-24*time.Hour)) {
		from = last.Add(-time.Hour) // first pull, or a long gap: the last hour only
	}
	for h := from; h.Before(last); h = h.Add(time.Hour) {
		res, err := a.lf.Pull(ctx, h, h.Add(time.Hour))
		if err != nil {
			return err
		}
		a.agg.Add(res.Spans, now)
		a.agg.AddEvals(res.Evals, now)
		a.mu.Lock()
		a.pulledThru = h.Add(time.Hour)
		a.mu.Unlock()
	}
	return nil
}

func (a *Agent) emit(ctx context.Context, windows []aggregate.Window) error {
	for _, w := range windows {
		c := commitment.ForWindow(a.secret, w.TraceIDs)
		u := a.usageFor(ctx, w.Start)
		a.mu.Lock()
		st := a.dryState
		if a.queue != nil {
			st = a.queue.State()
		}
		env, payload, next, err := a.builder.Build(ctx, st, w, c.Commit, u)
		if err != nil {
			a.mu.Unlock()
			return err
		}
		if a.opt.DryRun {
			a.dryState = next
			a.mu.Unlock()
			if err := a.printDry(env, payload); err != nil {
				return err
			}
			continue
		}
		if err := a.saveLeaves(next.Seq, c.Leaves); err != nil {
			a.mu.Unlock()
			return err
		}
		err = a.queue.Enqueue(env, next)
		a.mu.Unlock()
		if err != nil {
			return err
		}
		msg := "batch queued"
		if a.opt.Local {
			msg = "batch stored in the local evidence log"
		}
		a.opt.Log.Info(msg, "agent_id", a.id.AgentID, "seq", next.Seq, "window", w.Window.Start,
			"digest", *next.Prev, "traces", c.Commit.Leaves)
	}
	return nil
}

func (a *Agent) usageFor(ctx context.Context, h time.Time) []batch.Usage {
	var out []batch.Usage
	for _, p := range a.pollers {
		hours, err := p.Poll(ctx, h, h.Add(time.Hour))
		if err != nil {
			a.opt.Log.Warn("usage poll failed; the batch carries no usage from this provider", "window", h.Format(time.RFC3339), "error", err.Error())
			continue
		}
		for _, hu := range hours {
			if hu.Start.Equal(h) {
				out = append(out, hu.Usage...)
			}
		}
	}
	return out
}

// saveLeaves keeps the window's salted leaves so a trace can be disclosed later.
func (a *Agent) saveLeaves(seq int64, leaves []merkle.Hash) error {
	dir := filepath.Join(a.cfg.DataDir, "leaves")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	hexes := make([]string, len(leaves))
	for i, l := range leaves {
		hexes[i] = hex.EncodeToString(l[:])
	}
	b, _ := json.Marshal(hexes)
	return os.WriteFile(filepath.Join(dir, fmt.Sprintf("%020d.json", seq)), b, 0o600)
}

// printDry writes exactly what would be sent: the envelope, and its payload
// decoded so a person can read it.
func (a *Agent) printDry(env *dsse.Envelope, payload []byte) error {
	raw, err := env.Encode()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(map[string]json.RawMessage{"envelope": raw, "payload": payload}, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.opt.Out, "%s\n", b)
	return err
}

// Deliver sends queued batches until the queue is empty or ctx ends.
func (a *Agent) Deliver(ctx context.Context) error {
	if a.sender == nil {
		return nil
	}
	return a.sender.Drain(ctx)
}

// Run serves the OTLP receiver and runs the tick and delivery loops until ctx
// ends, then shuts down gracefully.
func (a *Agent) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	recv := a.Receiver()
	if !a.cfg.OTLPAllowRemote {
		recv = LocalHostOnly(recv)
	}
	mux.Handle("POST /v1/traces", recv)
	ln, err := net.Listen("tcp", a.cfg.OTLPListen)
	if err != nil {
		return fmt.Errorf("agent: listen %s: %w", a.cfg.OTLPListen, err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	if a.onListening != nil {
		a.onListening()
	}
	a.opt.Log.Info("agent started", "agent_id", a.id.AgentID, "otlp", a.cfg.OTLPListen, "server", a.cfg.Server, "dry_run", a.opt.DryRun, "local", a.opt.Local)

	every := a.opt.TickEvery
	if every <= 0 {
		every = time.Minute
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	deliver := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-deliver:
				if err := a.Deliver(ctx); err != nil && ctx.Err() == nil {
					a.opt.Log.Error("delivery stopped", "error", err.Error())
				}
			}
		}
	}()
	poke := func() {
		select {
		case deliver <- struct{}{}:
		default:
		}
	}
	poke()
	for {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
			if err := a.Shutdown(sctx); err != nil {
				return err
			}
			return a.Deliver(sctx)
		case <-tick.C:
			if err := a.Tick(ctx); err != nil {
				a.opt.Log.Error("tick failed", "error", err.Error())
			}
			poke()
		}
	}
}
