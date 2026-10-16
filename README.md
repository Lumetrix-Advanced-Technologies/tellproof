# Plumbline

**Signed, content-free evidence of how your AI system actually runs.** Add one line to an AI app, or to the serving layer of a model you host, and Plumbline keeps a tamper-evident hourly record of every model call: which models and regions, token counts, guardrail decisions, personal-data redaction, agent tool use, prompt and model changes. It never reads a prompt or a completion.

Run it on your own, with nothing leaving your machine. Or connect it to Plumbline's hosted service (early access) to turn that record into monthly evidence packs that a bank, insurer or auditor can verify themselves, offline, without trusting you or us.

| You are | What Plumbline gives you |
| --- | --- |
| **Building an AI-powered app or agent** (OpenAI, Anthropic, Bedrock, any OpenAI-compatible API) | One-line instrumentation, a signed local log of what your AI did each hour, and an answer to "prove it" from customers' vendor-risk teams. |
| **An AI model vendor or inference provider** serving your own models | Instrument your serving layer once, and give your customers evidence of the models, regions and guardrails behind their traffic. |
| **Reviewing an AI vendor** (bank, insurer, auditor, risk team) | Verifiers that check a vendor's evidence pack in your browser or offline, and test vectors to build your own. |

## Quick start: local mode (no account, no server)

**1. Run the agent.** It listens for spans on your machine and keeps signed hourly batches in its data directory.

```bash
docker run -d --name plumbline-agent --restart unless-stopped -p 127.0.0.1:4318:4318 \
  -v plumbline-agent:/data ghcr.io/lumetrix-advanced-technologies/plumbline-agent:latest local
```

Or download a binary from [Releases](https://github.com/lumetrix-advanced-technologies/plumbline/releases) and run `./plumbline-agent-linux-amd64 local`.

**2. Add one line to your app.**

```bash
pip install plumbline-sdk        # Python 3.10+, no dependencies
npm install plumbline-sdk        # Node.js 18+, no dependencies
```

```python
import plumbline; plumbline.init()                              # Python
```

```js
import * as plumbline from 'plumbline-sdk'; plumbline.init();   // Node.js
```

`init()` instruments the OpenAI, Anthropic and Bedrock client libraries you already use. Nothing else changes in your code.

**3. See your evidence.** About a minute after the first model call, and every hour after that:

```bash
docker exec plumbline-agent /plumbline-agent report
```

```
Plumbline local evidence log, agent agt_localf444634e45d1d9b9d0208705
  Verified: 2 batches, signed by this agent's key and chained from batch 1 to 2.
  Windows: 2026-09-01T00:00:00Z to 2026-09-01T02:00:00Z (UTC)

Model calls
  provider     model                        region          calls  input tokens  output tokens
  aws.bedrock  anthropic.claude-sonnet-4-5  ap-southeast-2  200    259900        60594
  openai       gpt-4.1-mini                 us-east-1       80     7200          320

Guardrails      240 of 320 model calls had a guardrail decision (75.0%); 10 blocked
Personal data   54 of 60 spans with personal data were redacted (90.0%)
Content         240 spans arrived with message content; the agent dropped it
Change          2026-09-01T01:40:13Z: model anthropic.claude-sonnet-4-5 -> anthropic.claude-sonnet-4-6
```

`report --json` gives the same as JSON, and `--since` / `--until` (YYYY-MM-DD) pick days. The report checks every batch's signature and the chain from the first batch, and fails if a batch was edited or removed. Keep a copy of `agent.pub` from the data directory somewhere else: the log proves it has not changed relative to that key.

## Record what no client library knows

The helpers add the facts reviewers ask about. Each is a context manager and a decorator in Python, and a callback scope in Node.js.

```python
with plumbline.prompt("claims-triage", "14"), plumbline.personal_data(redacted=True):
    with plumbline.guardrail() as g:
        r = client.chat.completions.create(...)
        g.flag() if needs_review(r) else g.allow()

@plumbline.tool("issue_refund", high_impact=True)
def issue_refund(claim): ...
```

`prompt`, `personal_data`, `guardrail`, `tool`, `agent_run`, `escalation`, `trace` and `llm_call` are documented in [`sdk/python`](sdk/python/README.md) and [`sdk/js`](sdk/js/README.md).

## For model vendors and inference providers

If you serve models yourself, there is no client library to patch: wrap each inference request in your serving layer with `llm_call`, naming your own provider and model, and report the tokens you already count.

```python
@app.post("/v1/chat/completions")
def chat(req):
    with plumbline.trace(), plumbline.llm_call("acme-ai", req.model, region="eu-west-1") as c:
        out = engine.generate(req)
        c.usage(out.prompt_tokens, out.completion_tokens)
        return out
```

```js
app.post('/v1/chat/completions', (req, res) =>
  plumbline.trace(() => plumbline.llmCall({ provider: 'acme-ai', model: req.body.model, region: 'eu-west-1' }, async (c) => {
    const out = await engine.generate(req.body);
    c.usage(out.promptTokens, out.completionTokens);
    res.json(out);
  })));
```

Already on OpenTelemetry? Point an OTLP/HTTP JSON exporter at `http://127.0.0.1:4318/v1/traces` and set the `plumbline.*` attributes from [`spec/semconv-map.json`](spec/semconv-map.json) instead. Use one or the other for the same calls, not both.

## What leaves your machine

In local mode, nothing. When connected, the agent sends one signed batch of counts and short names per hour, defined by [`spec/batch.schema.json`](spec/batch.schema.json): token and call counts per provider, model and region; personal-data, guardrail, evaluation and agent counts; which models and prompt versions were live; and one Merkle root over that hour's traces. Never prompts, completions, span content, tool arguments, end-user identifiers or raw trace IDs. `plumbline-agent run --dry-run` prints every batch it would send, decoded, and sends nothing.

## Get signed evidence packs (early access)

The hosted Plumbline service evaluates the agent's batches against ten evidence objectives (trace coverage against provider billing, data residency, guardrail coverage, personal-data redaction, evaluation freshness, change and incident registers and more) and issues a monthly pack: an in-toto statement in a DSSE envelope, signed with a key held in AWS KMS, chained to the previous pack, and listed in a public daily checkpoint. You share it with a link, and the reviewer's browser verifies it.

It is in early access. [Request access](https://github.com/lumetrix-advanced-technologies/plumbline/issues/new?template=early-access.yml) and choose what fits:

- **Hosted packs**: connect your agent (`plumbline-agent connect <token>`) and issue packs for your AI systems.
- **Design partner**: shape the evidence objectives with us for a regulated buyer you are selling to.
- **Model vendors**: evidence for the models you serve, which your customers can pass on to their reviewers.
- **Reviewer**: you assess AI vendors and want packs, or the verifiers, in your process.

Your local log does not move to the hosted service: connecting starts a new agent with its own data directory.

## Verify a pack

```bash
plumbline-verify --jwks issuer-jwks.json pack.dsse.json
plumbline-verify checkpoint --jwks issuer-jwks.json [--prev yesterday.json] checkpoint.json pack.dsse.json
```

Pass several packs oldest first to check the chain between them; `--json` prints a report. The verify page (`web/verify`) runs the same checks in the browser, and `concierge/verify.py` is a third, independent verifier. All three must agree on every case in [`spec/vectors`](spec/vectors/README.md), which also explains how to write your own.

## Install and check a release

Every release binary, `SHA256SUMS` and the release notes carry signed build provenance tied to this repository's source:

```bash
sha256sum --ignore-missing -c SHA256SUMS
gh attestation verify plumbline-agent-linux-amd64 --repo lumetrix-advanced-technologies/plumbline
```

Use the `plumbline-agent-bedrock-*` binaries for Amazon Bedrock usage from CloudWatch. From source (Go 1.26+): `go install github.com/lumetrix-advanced-technologies/plumbline/cmd/agent@latest` and `…/cmd/verify@latest`. Configuration, sources and networking: [the agent docs](web/docs/agent/index.html).

## Develop

```bash
make check    # gofmt, vet, staticcheck, govulncheck, tests, every verifier on every vector, SDK tests
make agent-dry
```

The tests need Node 22+ and Python 3.10+ with `cryptography`. Go code uses only the standard library, plus the AWS SDK in the `bedrock` build. Two rules every change keeps: **no content leaves the customer** (no code path in the agent or the SDKs may send prompt or completion text, span content or raw trace IDs), and **the verifiers agree** (vectors are added, never changed).

## Security and licence

Report vulnerabilities privately: see [SECURITY.md](SECURITY.md). Apache License 2.0: see [LICENSE](LICENSE) and [NOTICE](NOTICE).
