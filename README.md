<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/logo-dark.svg">
    <img src="docs/logo-light.svg" alt="Tellproof" width="400">
  </picture>
</p>

<h3 align="center">Signed, content-free evidence of how your AI system actually runs.</h3>

<p align="center">
  Add one line to an AI app, an agent or a model server. Get a tamper-evident, hour-by-hour record of every model call,<br>
  a local dashboard, governance checks for CI, and evidence a bank, insurer or auditor can verify without trusting you.
</p>

<p align="center">
  <a href="https://github.com/Lumetrix-Advanced-Technologies/tellproof/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/Lumetrix-Advanced-Technologies/tellproof/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://scorecard.dev/viewer/?uri=github.com/Lumetrix-Advanced-Technologies/tellproof"><img alt="OpenSSF Scorecard" src="https://api.scorecard.dev/projects/github.com/Lumetrix-Advanced-Technologies/tellproof/badge"></a>
  <a href="https://pypi.org/project/tellproof/"><img alt="PyPI: tellproof" src="https://img.shields.io/pypi/v/tellproof?label=pip%20install%20tellproof%5Bagent%5D&color=1f56c9"></a>
  <a href="https://www.npmjs.com/package/tellproof"><img alt="npm: tellproof" src="https://img.shields.io/npm/v/tellproof?label=npm%20install%20tellproof&color=1f56c9"></a>
  <a href="https://pypi.org/project/tellproof-agent/"><img alt="PyPI: tellproof-agent" src="https://img.shields.io/pypi/v/tellproof-agent?label=tellproof-agent&color=1e6b43"></a>
  <a href="LICENSE"><img alt="Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-565d69"></a>
</p>

<p align="center">
  <a href="https://lumetrix-advanced-technologies.github.io/tellproof/"><b>Website</b></a> ·
  <a href="#quick-start-two-minutes-no-account"><b>Quick start</b></a> ·
  <a href="#instrument-your-app">Instrument</a> ·
  <a href="#run-the-agent">Run the agent</a> ·
  <a href="#governance-as-code-policy-check-and-ai-bom">Governance as code</a> ·
  <a href="#cli-reference">CLI</a> ·
  <a href="HOW-IT-WORKS.md"><b>How it works</b></a>
</p>

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/dashboard-dark.png">
    <img src="docs/dashboard.png" alt="The local dashboard: totals, the eleven evidence objectives with pass, attention or fail, and a week of model calls per hour by model" width="900">
  </picture>
</p>

> [!IMPORTANT]
> **No content leaves your machine.** The SDKs never read prompts or completions, the agent drops message attributes the moment they arrive, and what it keeps is counts, short names and salted commitments. In local mode nothing is sent anywhere at all; `tellproof-agent run --dry-run` shows exactly what connected mode would send. [How that works](HOW-IT-WORKS.md#what-leaves-your-machine).

This page is the setup and usage guide. For the architecture, the evidence model, what is signed and how it is verified, and the security model, read **[How Tellproof works](HOW-IT-WORKS.md)**.

## What you get

- **One line in your app.** `tellproof.init()` records every OpenAI, Anthropic and Bedrock call (Python or Node.js, no dependencies): models, regions, tokens, guardrail decisions, redaction, tool use. Never the content.
- **A signed, tamper-evident log and a local dashboard.** One signed, chained batch per hour, kept on your machine; a dashboard on `http://127.0.0.1:4319`. No account.
- **Governance as code.** Declare models and regions in `tellproof.policy.json`; `check` evaluates the eleven evidence objectives locally and in CI, `check --self-test` proves each could fail, `aibom` writes a CycloneDX AI-BOM.
- **Evidence for coding agents.** Signed per-commit proof that CI gates ran, tested something, and could have failed.
- **Packs reviewers verify themselves** (hosted, early access): the same evaluation, signed by an independent issuer, checked in the reviewer's browser.

## Contents

1. [Quick start: two minutes, no account](#quick-start-two-minutes-no-account)
2. [Install](#install)
3. [Instrument your app](#instrument-your-app)
4. [Run the agent](#run-the-agent)
5. [The dashboard](#the-dashboard)
6. [Reports in the terminal](#reports-in-the-terminal)
7. [Governance as code: policy, check and AI-BOM](#governance-as-code-policy-check-and-ai-bom)
8. [Evidence for coding agents: CI gates per commit](#evidence-for-coding-agents-ci-gates-per-commit)
9. [Configuration reference](#configuration-reference)
10. [More evidence sources: Langfuse and provider usage](#more-evidence-sources-langfuse-and-provider-usage)
11. [Deploying the agent](#deploying-the-agent)
12. [Signed evidence packs (early access)](#signed-evidence-packs-early-access)
13. [Verify a pack](#verify-a-pack)
14. [CLI reference](#cli-reference)
15. [Troubleshooting](#troubleshooting)
16. [Develop and contribute](#develop-and-contribute)
17. [Security reports and licence](#security-reports-and-licence)

---

## Quick start: two minutes, no account

**1. Install the SDK and the agent, and set up**, in your app's directory.

Python 3.10+:

```bash
pip install "tellproof[agent]"
tellproof init
```

Node.js 18+ (npm keeps a package's commands in `node_modules/.bin`, not on your PATH, so run them with `npx`):

```bash
npm install tellproof
npx tellproof init
```

The agent comes with the SDK (on npm as an optional dependency). Prefer a plain `tellproof` command in Node.js too? `npm install -g tellproof` puts it on your PATH; keep the project's own `npm install tellproof` for the SDK.

`init` recognises a Python or Node.js project, shows the one line to add and the file it goes in, writes `tellproof.policy.json`, offers to add the agent's data directory to `.gitignore`, and starts the agent in the background. It changes no source file. The line it shows is:

```python
import tellproof
tellproof.init()                   # once, at start-up, before your first model call
```

```js
import * as tellproof from 'tellproof';
tellproof.init();
```

**2. Run your app, make a model call, and open http://127.0.0.1:4319.** The first batch appears about a minute after the first call, and every hour after that. Nothing showing? `tellproof doctor` checks the setup and prints a fix for each problem.

**3. When you are done:** `tellproof stop` (`npx tellproof stop` in a Node.js project). `tellproof start` brings it back; `tellproof status` says whether it runs and where; `tellproof --version` shows the agent's and the SDK's versions.

> [!TIP]
> Only looking? `tellproof demo` serves the dashboard on a synthetic, signed week (nothing from your system), with a policy, so you can try `report`, `check` and `check --self-test` on it. Next steps: declare what the system may use and check the evidence against the eleven objectives with `tellproof policy init`, then `tellproof check` ([governance as code](#governance-as-code-policy-check-and-ai-bom)).

> [!NOTE]
> **One command, three spellings.** `tellproof` (installed with the SDK), `npx tellproof` (inside an npm project) and `tellproof-agent` (the agent on its own: a release binary, the container, `pip install tellproof-agent`) run the same agent with the same commands. The rest of this guide writes `tellproof-agent`, which works in every install.

In a terminal the agent greets you; run it with no arguments for the welcome and first steps:

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/banner-dark.png">
    <img src="docs/banner.png" alt="The Tellproof welcome banner in a terminal: the TELLPROOF wordmark beside the bolt-and-stripe mark, then the first commands to run" width="720">
  </picture>
</p>

The banner appears only in an interactive terminal (from v0.4.0). Pipes, files, CI and `TELLPROOF_NO_BANNER=1` get the plain lines and the JSON log, unchanged; `NO_COLOR` keeps the banner without colour.

---

## Install

### The SDK and the agent together

| You use | Install | You get |
| --- | --- | --- |
| **Python** | `pip install "tellproof[agent]"` | The SDK (`import tellproof`), the agent, and the `tellproof` and `tellproof-agent` commands. |
| **Node.js** | `npm install tellproof` | The SDK (`import`/`require('tellproof')`), and the agent as an optional dependency: `npx tellproof`. pnpm, Yarn and Bun work too. |

In production the agent usually runs beside the app (a sidecar, a service or a container: see [Deploying the agent](#deploying-the-agent)), so the app needs only the SDK: `pip install tellproof`, or `npm ci --omit=optional` to leave the agent out of the image. The SDK imports nothing outside the standard library (Node built-ins), and never the agent.

### The agent on its own

| Method | Command | Notes |
| --- | --- | --- |
| **pip** | `pip install tellproof-agent` | Python 3.8+. Platform wheels for Linux (glibc and musl) x86_64 and aarch64, macOS 12+ x86_64 and arm64, Windows x64 and arm64. Each holds the release's static binary; nothing is compiled or downloaded at install time. `python -m tellproof_agent` works too. |
| **npm** | `npm install -g tellproof-agent`, or `npx tellproof-agent` | The right platform binary comes as an optional dependency; no install scripts. Don't install with `--omit=optional`. |
| **Docker** | `ghcr.io/lumetrix-advanced-technologies/tellproof-agent:<version>` | A static binary on distroless, non-root, for linux/amd64 and linux/arm64. Data in `/data`. See below. |
| **Binary** | [Releases](https://github.com/Lumetrix-Advanced-Technologies/tellproof/releases) | `tellproof-agent-<os>-<arch>`; use `tellproof-agent-bedrock-*` for Amazon Bedrock usage from CloudWatch. |
| **Source** | `go install github.com/lumetrix-advanced-technologies/tellproof/cmd/agent@latest` | Go 1.26+. Add `-tags bedrock` for the Bedrock usage poller. |

With Docker, in local mode, publishing the receiver and the dashboard on the host's loopback only:

```bash
docker run -d --name tellproof-agent -p 127.0.0.1:4318:4318 -p 127.0.0.1:4319:4319 \
  -v tellproof-agent:/data ghcr.io/lumetrix-advanced-technologies/tellproof-agent:latest run --local
docker stop tellproof-agent                        # and docker start tellproof-agent
```

<details>
<summary><b>More about the npm packages</b></summary>

- **How it installs:** `tellproof-agent` is a small launcher; the binary for your platform comes in one of six packages (`tellproof-agent-linux-x64`, `-linux-arm64`, `-darwin-x64`, `-darwin-arm64`, `-win32-x64`, `-win32-arm64`) that npm picks through `optionalDependencies`. Nothing runs at install time: no install scripts, no downloads.
- **"no binary for …" or "the Tellproof agent is not installed":** your platform is not one of those six, or optional dependencies were left out (`--omit=optional`, `--no-optional`, or `optional=false` in `.npmrc`). Reinstall without that, or use a [release binary](https://github.com/Lumetrix-Advanced-Technologies/tellproof/releases).
- **Upgrade and remove:** `npm install tellproof@latest` (or `npm install -g tellproof-agent@latest`); `npm uninstall`. The data directory (`./.tellproof-agent`) is yours and stays where it is.
- **The SDK** ships CommonJS and ES modules with TypeScript types: `import * as tellproof from 'tellproof'` or `const tellproof = require('tellproof')`.

</details>

### Check a download

Every release binary, `SHA256SUMS`, the release notes, the wheels, the npm tarballs and the container image carry signed build provenance (SLSA, through GitHub artifact attestations) that ties them to this repository's source and release workflows. npm packages come from the release workflow through staged publishing: the workflow stages the attested tarballs and a maintainer approves each version with two-factor authentication on npmjs.com, so nothing reaches npm without a person. v0.3.0's were published by hand from the same attested tarballs; check them against the tarballs on the release page.

```bash
sha256sum --ignore-missing -c SHA256SUMS
gh attestation verify tellproof-agent-linux-amd64 --repo Lumetrix-Advanced-Technologies/tellproof
gh attestation verify oci://ghcr.io/lumetrix-advanced-technologies/tellproof-agent:v0.2.0 --repo Lumetrix-Advanced-Technologies/tellproof
npm audit signatures            # in a project that installed tellproof-agent or tellproof
```

Each release also carries its provenance as `tellproof-<version>.intoto.jsonl`, the same Sigstore bundle, for verifiers that read release assets.

### The verifier

`tellproof-verify-<os>-<arch>` from [Releases](https://github.com/Lumetrix-Advanced-Technologies/tellproof/releases), or `go install github.com/lumetrix-advanced-technologies/tellproof/cmd/verify@latest`.

---

## Instrument your app

### One line

```python
import tellproof
tellproof.init()
```

```js
import * as tellproof from 'tellproof';          // or: const tellproof = require('tellproof');
tellproof.init();
```

For most apps that is all. Every OpenAI, Anthropic and Amazon Bedrock call is recorded (models, regions, tokens, errors; never content), each web request is its own trace (Flask, FastAPI, Starlette and Django in Python; any Node.js HTTP server, so Express, Fastify, Koa, Nest and Next.js), and guardrails the SDK can see are recorded on their own: [OpenAI moderation, Bedrock Guardrails, Azure OpenAI content filters](#detected-without-code). `init()` is safe to call more than once and returns whether the SDK is on; `TELLPROOF_DISABLED=1` turns it off without a code change.

### Settings, in `init()`

| Python | Node.js | Environment | Meaning |
| --- | --- | --- | --- |
| `agent` | `agent` | `TELLPROOF_AGENT_NAME` | The agent every model call belongs to. Model changes are tracked per agent and operation: a model new to the agent is a change, models it uses side by side are not ([what a change is](HOW-IT-WORKS.md#from-a-model-call-to-a-signed-batch)). |
| `prompt`, `prompt_version` | `prompt`, `promptVersion` | `TELLPROOF_PROMPT`, `TELLPROOF_PROMPT_VERSION` | The prompt (its name and version, never its text) every model call uses. Set the version from your deploy, so every prompt change shows in the evidence (and the change check asks for it in the register). |
| `personal_data` | `personalData` | `TELLPROOF_PERSONAL_DATA=1` | Every model call handles personal data. It counts as redacted only in requests where your [`redactor`](#mark-the-functions-you-already-have) ran. |
| `service`, `environment` | `service`, `environment` | `TELLPROOF_SERVICE` (or `OTEL_SERVICE_NAME`), `TELLPROOF_ENVIRONMENT` | The service name and deployment environment on every span. |
| `agent_url` | `agentUrl` | `TELLPROOF_AGENT_URL` | Where the agent listens (default `http://127.0.0.1:4318`). |
| `requests` | `requests` | | `False` leaves web frameworks alone (no trace per request). |
| `canaries` | `canaries` | | `False` turns the [canaries](#guardrail-canaries) off; a number is the interval (seconds in Python, milliseconds in Node.js; default an hour). |
| `instrument` | `instrument` | | `False` leaves the client libraries alone. |
| `stream_usage` | `streamUsage` | | Ask OpenAI for usage on streamed chat completions the app has not configured. The stream then ends with one extra chunk with no `choices`. |

Settings in a scope (below) or on a call win over `init()`'s.

### Mark the functions you already have

Your app already has a guardrail check, a redaction step and tools. Mark each once, where it is defined, and the SDK records what it does for the request it runs in; no call site changes.

```python
@tellproof.guardrail_check              # your guardrail: returns True (or "block"/"flag") to stop the text
def check(text): ...

@tellproof.redactor                     # your redaction step
def redact(messages): ...

@tellproof.tool(high_impact=True)       # a tool with real-world effect
def issue_refund(claim):
    if approved_by_policy(claim):
        tellproof.approve()             # a person or a policy approved this call
    ...
```

```js
const check = tellproof.guardrailCheck((text) => myGuardrail.check(text));
const redact = tellproof.redactor(redactPII);
const issueRefund = tellproof.tool(async function issueRefund(claim) {
  if (approvedByPolicy(claim)) tellproof.approve();
  ...
}, { highImpact: true });
```

Then your request handlers need no Tellproof code at all:

```python
@app.post("/claims/{claim_id}/triage")
def triage(claim_id: str, req: Request):
    if check(req.text):                 # blocked: recorded as one blocked call
        return refusal()
    r = client.chat.completions.create(model="gpt-4.1-mini", messages=redact(req.messages))
    if check(r.choices[0].message.content):
        return refusal()                # an output check that blocks: this request's call counts as blocked
    return r
```

How the decisions count:

- **Per request.** A check's decision applies to the model calls of the request it ran in, before or after them; with several checks, the strictest wins (block, then flag, then allow). A request that is blocked before any model call is recorded as one blocked call. `True` or `"block"` blocks, `"flag"` flags, anything else allows; the text and the result are never recorded.
- **Canaries come with it.** Each `guardrail_check` also receives the [canary probes](#guardrail-canaries) hourly. Mark an output-only check `@tellproof.guardrail_check(canaries=False)` (`guardrailCheck(fn, { canaries: false })`) if it is not meant to catch prompt injections.
- **A `redactor`** that returns normally marks its request's model calls as personal data, redacted. With `init(personal_data=True)`, calls in requests where it did not run count as unredacted.
- **Outside a web request** (a queue worker, a script), give each unit of work its own request: `@tellproof.trace` on the handler, or `with tellproof.trace():` (`tellproof.trace(() => ...)` in Node.js). Until then a check's allow or flag is not attributed to anything (a block still counts as one blocked call), and `stats()["unscoped"]` counts what was left out: Tellproof never guesses which call a decision was for.
- Also: `@tellproof.agent_run` (`tellproof.agentRun(fn)`) makes a function one agent run; `tellproof.escalation()` records that a request was handed to a person.

### Detected without code

| What your app already does | What is recorded |
| --- | --- |
| OpenAI moderation (`moderations.create`) | A guardrail decision for the request: flag when a result is flagged, else allow. Only the `flagged` booleans are read. |
| Bedrock Guardrails: `ApplyGuardrail`, or `guardrailConfig` on a Converse call | Block when the guardrail intervened, else allow. Only the action and the stop reason are read. |
| Azure OpenAI (content filters on by default) | Each call is guarded: block when a choice ended on `content_filter` or the prompt was refused with the error code `content_filter`, else allow. Only the presence of the filter results, the finish reasons and the error code are read. |
| OpenAI stored prompts (`responses.create(prompt={"id": ..., "version": ...})`) | The prompt's ID and version as the call's prompt name and version; never its text or variables. |

### What is instrumented automatically

| Library | Calls |
| --- | --- |
| `openai` (Python and Node.js) | `chat.completions.create` / `parse` / `stream`, `responses.create` / `parse` / `stream`, `embeddings.create`, `completions.create`; sync, async and streaming. Azure OpenAI is reported as `azure.ai.openai`, other OpenAI-compatible endpoints by their host name. |
| `anthropic` / `@anthropic-ai/sdk` | `messages.create` and `messages.stream`, and `beta.messages`. `AnthropicBedrock` and `AnthropicVertex` are reported as `aws.bedrock` and `gcp.vertex_ai`, with their region. |
| `boto3` Bedrock Runtime / `@aws-sdk/client-bedrock-runtime` | `converse`, `converse_stream`, `invoke_model`, `invoke_model_with_response_stream` (in Node.js, the matching `*Command`s through `client.send()`). |
| Web frameworks | One trace per request: Flask, FastAPI and Starlette, Django; in Node.js every server on `node:http` or `node:https`. |

A library is patched when the app loads it; the SDK never loads one itself, so an installed library the app does not use (boto3 beside an OpenAI app, Django beside Flask) costs no memory, and `stats()["instrumented"]` lists only what the app has loaded.

Per call, the SDK reads the request's model and stream flag, and from the response only the model, the usage block, the stop reason and Bedrock's token-count headers. Input tokens follow what the provider bills (Anthropic: uncached + cache writes + cache reads), so traced tokens can be reconciled with billed usage. Streams are recorded once, when they end. Calls through `invoke_model_with_response_stream` and `with_raw_response` are counted without tokens, because their counts sit only inside bodies the SDK never reads.

Other functions: `flush()` sends buffered spans now (in Node.js, `await tellproof.flush()` before a serverless handler or a short script returns); `shutdown()` flushes and stops (Python also runs it at exit); `is_enabled()` / `isEnabled()`; `stats()` returns counters (`spans`, `exported`, `dropped`, `export_errors`, `instrumentation_errors`, `unscoped`, `instrumented`). In Node.js, `await tellproof.ready()` once the ES-module copies of the client libraries are instrumented, if a module calls a model straight after `init()`, and `tellproof.instrument({ openai: OpenAI, anthropic: Anthropic, bedrock: await import('@aws-sdk/client-bedrock-runtime') })` for bundled apps, strict package managers where the SDK cannot find the libraries itself, and CommonJS apps that load a client with `import()`. The Python SDK works across threads, `asyncio` tasks and forked workers (gunicorn, uWSGI).

### Guardrail canaries

Guardrail coverage (EO-GRD-01) shows a guardrail *ran* on every call; it cannot show the guardrail *works*. Canaries close that gap: the SDK passes a few synthetic, known-bad probes (a prompt injection, a jailbreak, made-up personal data, a documented example AWS key) to **your own guardrail check**, and records only the kind and whether it was caught. EO-CAN-01 passes when every canary was caught; a missed one fails it and names the kind. ([Why](HOW-IT-WORKS.md#evidence-that-can-fail-canaries-and-the-self-test).)

With `@tellproof.guardrail_check` (`guardrailCheck`) they run on their own, from the check's first call and then hourly, through the same function (a method gets its own instance): a sync Python check on a daemon thread, an async one on your app's event loop, in Node.js on an unref'd timer. Decorate or wrap a check once, where it is defined (at most 32 are registered). To run them yourself instead:

```python
tellproof.schedule_canaries(guardrail_check, every=3600)   # now, then hourly, on a daemon thread
tellproof.run_canaries(guardrail_check)                     # or once, e.g. from a health check: {"pii": True, ...}
await tellproof.arun_canaries(async_guardrail_check)        # for an async check
tellproof.canary("toxicity", caught=True)                   # a canary you run yourself, with your own probe
```

```js
tellproof.scheduleCanaries(check, { every: 3_600_000 });   // now, then hourly, on an unref'd timer
await tellproof.runCanaries(check);                        // { prompt_injection: true, ... }
tellproof.canary('toxicity', true);
```

- **Only the result leaves.** The probe text stays in the process (it is in `tellproof.PROBES` and [`spec/canaries.json`](spec/canaries.json)); the span carries `tellproof.canary.kind` and `tellproof.canary.caught`, in its own trace, so it never counts as a model call, a decision, personal data or a prompt version.
- **Probes go to the guardrail only, never to the model.** The check should call your guardrail and nothing else.
- **A check that throws, or returns anything but `True`, `"block"` or `"flag"`, counts the canary as missed.** If your guardrail raises to block, catch that and return `True`.
- **Off means off.** With the SDK disabled the check is never called. Scheduled runs are at least a minute apart.
- Without canaries, EO-CAN-01 is *attention* ("No guardrail canaries run"), never fail.

### Scopes, for everything else

The decorators above cover most apps. Scopes say the same things explicitly for a block of code, and win over `init()` and the decorators. In Python each is a context manager (`with` / `async with`) and a decorator; in Node.js a callback scope that returns whatever the callback returns, and follows `await`, timers and callbacks.

<details>
<summary><b>Every scope</b></summary>

| Python | Node.js | Records |
| --- | --- | --- |
| `tellproof.trace()` | `trace(fn)` | One request: a trace of its own, the request's decisions and redaction. Automatic for web requests. |
| `tellproof.prompt(name, version)` | `prompt(name, version, fn)` | The prompt (name and version, never its text) used by calls inside. |
| `tellproof.personal_data(redacted=False)` | `personalData({ redacted }, fn)` | Spans inside handle personal data; `redacted` means it was removed or masked before the model saw it. |
| `tellproof.guardrail(decision=None)` | `guardrail(decision?, fn)` | The guardrail decision for calls inside: `allow`, `block` or `flag`, set later with `g.allow()`, `g.block()`, `g.flag()`. A block with no call inside counts as one blocked call. |
| `tellproof.tool(name, high_impact=False, approved=False)` | `tool(name, { highImpact, approved }, fn)` | A tool call; an exception marks it failed. Set `t.approved = True`, or call `tellproof.approve()`, once a person or policy approves it. |
| `tellproof.agent_run(name)` | `agentRun(name, fn)` | One agent run; model changes are tracked per agent. |
| `tellproof.llm_call(provider, model, operation="chat", region=None)` | `llmCall({ provider, model, operation, region }, fn)` | A model call the SDK does not instrument; report its usage with `c.usage(input_tokens, output_tokens)`. |

```python
with tellproof.prompt("claims-triage", "14"), tellproof.personal_data(redacted=True):
    with tellproof.guardrail() as g:
        r = client.chat.completions.create(model="gpt-4.1-mini", messages=redact(req.messages))
        g.flag() if needs_review(r) else g.allow()
```

Calls inside a `guardrail` scope are held until it ends, so a decision made after the call (an output check) still applies. Names and versions are capped at 128 characters.

</details>

### For model vendors and inference providers

If you serve models yourself, there is no client library to patch. Wrap each inference request in your serving layer with `llm_call`, naming your own provider and model, and report the tokens you already count:

```python
@app.post("/v1/chat/completions")
def chat(req):
    with tellproof.trace(), tellproof.llm_call("acme-ai", req.model, region="eu-west-1") as c:
        with tellproof.guardrail() as g:
            out = engine.generate(req)
            g.block() if out.filtered else g.allow()
        c.usage(out.prompt_tokens, out.completion_tokens)
        return out
```

```js
app.post('/v1/chat/completions', (req, res) =>
  tellproof.trace(() => tellproof.llmCall({ provider: 'acme-ai', model: req.body.model, region: 'eu-west-1' }, async (c) => {
    const out = await engine.generate(req.body);
    c.usage(out.promptTokens, out.completionTokens);
    res.json(out);
  })));
```

### Already on OpenTelemetry?

Skip the SDK and point an OTLP/HTTP **JSON** exporter at `http://127.0.0.1:4318/v1/traces`. The agent reads the OpenTelemetry GenAI conventions (old and new attribute names) and a few `tellproof.*` attributes, as listed in [`spec/semconv-map.json`](spec/semconv-map.json).

<details>
<summary><b>The attributes the agent reads</b></summary>

| Fact | Attributes the agent reads |
| --- | --- |
| Provider | `gen_ai.provider.name`, `gen_ai.system` |
| Model | `gen_ai.request.model`, `gen_ai.response.model` |
| Tokens | `gen_ai.usage.input_tokens` or `prompt_tokens`; `gen_ai.usage.output_tokens` or `completion_tokens` |
| Operation | `gen_ai.operation.name`, `llm.request.type` (`chat`, `text_completion`, `generate_content`, `embeddings`); `invoke_agent` and `execute_tool` mark agent runs and tool calls |
| Region | `cloud.region`, `aws.region` |
| Agent | `gen_ai.agent.name` |
| Prompt | `tellproof.prompt.name`, `tellproof.prompt.version` |
| Personal data | `tellproof.data_class` = `personal`; `tellproof.redaction.applied` |
| Guardrail | `tellproof.guardrail.decision` (`allow`, `block`, `flag`) |
| Tools | `tellproof.tool.high_impact`, `tellproof.tool.approved` |
| Escalation | `tellproof.escalation` |

</details>

Message attributes are never read: their presence is counted (`content_present_spans`, objective EO-CNT-01) and their values dropped at parse time. They are the GenAI and OpenLLMetry names (`gen_ai.input.messages`, `gen_ai.output.messages`, `gen_ai.system_instructions`, `gen_ai.tool.call.arguments`, `gen_ai.tool.call.result`, `gen_ai.prompt*`, `gen_ai.completion*`, `llm.prompts.*`, `llm.completions.*`), OpenInference (`input.value`, `output.value`, `llm.input_messages.*`, `llm.output_messages.*`), Traceloop (`traceloop.entity.input`, `traceloop.entity.output`), Langfuse (`langfuse.observation.input`, `langfuse.observation.output`) and the Vercel AI SDK (`ai.prompt`, `ai.prompt.messages`, `ai.response.text`, `ai.toolCall.args`, `ai.toolCall.result`); the list is `presence_only` in [`spec/semconv-map.json`](spec/semconv-map.json). If EO-CNT-01 fails, turn content capture off in your instrumentation: for example `TRACELOOP_TRACE_CONTENT=false`, `OPENINFERENCE_HIDE_INPUTS=true` and `OPENINFERENCE_HIDE_OUTPUTS=true`, or `recordInputs: false` and `recordOutputs: false` in the Vercel AI SDK's `experimental_telemetry`. Use either the SDK or your exporter for the same calls, not both, or they are counted twice.

---

## Run the agent

### Locally

| Command | What it does |
| --- | --- |
| `tellproof-agent start` | **Local mode, in the background.** No account, server or token. Signs batches with its own key, keeps them in `<data-dir>/local`, serves the dashboard on `127.0.0.1:4319`, and returns once it listens. Its output goes to `<data-dir>/agent.log`. `--open` opens the dashboard. |
| `tellproof-agent stop` | Lets it finish and exit; the evidence stays. It also stops an agent running in the foreground. `--force` ends one that has not stopped after 15 seconds. |
| `tellproof-agent status` | Whether it runs, its pid, since when, and where (spans, dashboard, evidence, log). Exit 0 running, 3 not. `--json`. |
| `tellproof-agent restart` | Stop, then start: for example after upgrading. |
| `tellproof-agent logs [-f]` | The background agent's log; `-f` follows it. |
| `tellproof-agent run --local` | The same agent in the foreground, for a terminal you keep open or a container. Ctrl-C, or `stop` from another terminal, ends it. (`tellproof-agent local` does the same.) |

The commands find the agent through its data directory (`./.tellproof-agent` by default), so run them in the directory you started it from, or pass the same `--data-dir` or `--config`. Only one agent runs per data directory.

### Connected, and other modes

| Command | What it does |
| --- | --- |
| `tellproof-agent connect <token>` | **Connected mode, in one command.** Enrols with a one-time token from the Tellproof app (unless the data directory is already enrolled), keeps a salt secret in the data directory, then runs and sends signed batches to `TELLPROOF_SERVER`. Running it again later just runs. Run it under a service manager or in a container. |
| `tellproof-agent enrol --config agent.json` | Enrol only, with the token from `--token` or `TELLPROOF_ENROL_TOKEN`. |
| `tellproof-agent run --config agent.json` | Run an enrolled agent with a config file (sources, aggregation settings). |
| `tellproof-agent run --config agent.json --dry-run` | Print every batch it would send, with its payload decoded; send, queue and store nothing. Works before enrolment. |
| `tellproof-agent run --config agent.json --dry-run --replay export.jsonl` | Feed an OTLP JSON export through the pipeline, print every window and exit: the fastest way to see what Tellproof would make of traces you already have. |
| `tellproof-agent dashboard --data-dir <dir>` | Serve the dashboard on its own for a data directory (a copied log, or one written elsewhere). |

A local-mode data directory and an enrolled one never mix: `connect` refuses a local data directory, and local mode refuses an enrolled one. Batches are hourly (UTC); the first one appears about a minute after the first span. [How windows, chaining and commitments work](HOW-IT-WORKS.md#from-a-model-call-to-a-signed-batch).

---

## The dashboard

`tellproof-agent start` (or `run --local`) serves it on **http://127.0.0.1:4319**. It reads the same verified log as `report`, so the two can never disagree.

| Panel | Shows |
| --- | --- |
| **Status bar** | ✓ Verified or ✕ Failed (with the first problem), the number of batches, the agent ID, and when it last refreshed. |
| **Totals** | Model calls and models, tokens in and out, guardrail coverage and blocked calls, personal-data redaction, spans whose content was dropped, tool calls and failures, traces committed. |
| **Model calls** | Calls per hour (per day for long ranges), stacked by model, with a legend. The six busiest models keep their colours across ranges; the rest fold into "Other". |
| **Coverage** | Guardrail coverage and personal-data redaction over time, as percentages. |
| **Tokens** | Input and output tokens over time. |
| **Models and regions** | Every provider, model and region in the range, with calls, share and tokens. |
| **Timeline** | Model and prompt changes, anomaly hours (unusual error or guardrail-block rates) and evaluation runs. |
| **Evidence chain** | The newest batches, with sequence numbers, hours, calls, traces and full digests. |

Choose a range with the buttons (24 hours, 7 days, 30 days, All); a link can pick one with `#range=7d`. Hover a chart, or focus it and use the arrow keys, for an hour's figures; every chart also has a table view. It refreshes every 30 seconds, follows your system's light or dark theme, and works at phone width.

It is served on loopback only and is read-only ([details](HOW-IT-WORKS.md#security-model)). Move it with `dashboard_listen` or `TELLPROOF_DASHBOARD_LISTEN`, or turn it off with `off`. To look at a log somewhere else: `tellproof-agent dashboard --data-dir /path/to/copy --listen 127.0.0.1:4320`.

---

## Reports in the terminal

```bash
tellproof-agent report                                  # the whole log
tellproof-agent report --since 2026-10-01 --until 2026-10-31
tellproof-agent report --json > october.json            # the same, as JSON
tellproof-agent report --data-dir /path/to/copy         # a copied log
docker exec tellproof-agent /tellproof-agent report     # inside the container
```

```
Tellproof local evidence log, agent agt_localf444634e45d1d9b9d0208705
  Verified: 2 batches, signed by this agent's key and chained from batch 1 to 2.
  Latest batch digest: sha256:00a34bc8802ae6135dbd64864373a0b3e391beb4b43e84199eb5786f631a6258
  Windows: 2026-09-01T00:00:00Z to 2026-09-01T02:00:00Z (UTC)

Model calls
  provider     model                        region          calls  input tokens  output tokens
  aws.bedrock  anthropic.claude-sonnet-4-5  ap-southeast-2  200    259900        60594
  openai       gpt-4.1-mini                 us-east-1       80     7200          320
  aws.bedrock  anthropic.claude-sonnet-4-6  ap-southeast-2  40     56780         12121

Guardrails      240 of 320 model calls had a guardrail decision (75.0%); 10 blocked
Personal data   54 of 60 spans with personal data were redacted (90.0%)
Content         240 spans arrived with message content; the agent dropped it
Agents          240 runs, 240 tool calls (8 failed), 24 high-impact calls (12 approved), 1 escalations
Traces          240 committed (salted Merkle roots; disclose one later from this data directory)
Change          2026-09-01T01:15:16Z: prompt 13 -> 14
Change          2026-09-01T01:40:13Z: model anthropic.claude-sonnet-4-5 -> anthropic.claude-sonnet-4-6
Anomaly         2026-09-01T00:00:00Z to 2026-09-01T01:00:00Z: guardrail_rate
```

The report checks every batch's signature against `agent.pub`, that the sequence runs from 1 without gaps, that each batch names the previous batch's digest, and that no batch is missing from the end. If any check fails it says where, and exits **1**, so it can gate a CI job or a cron alert. `--since` and `--until` limit the figures, never the checks.

> [!NOTE]
> A local log proves it has not changed since it was written, relative to `agent.pub`. Keep a copy of `agent.pub` somewhere else, or give it to a reviewer up front. For evidence signed by an independent issuer, see [signed evidence packs](#signed-evidence-packs-early-access).

---

## Governance as code: policy, check and AI-BOM

Write down what your AI system is meant to be, next to its code, and check the evidence against it: locally, in CI, and with the same evaluator that issues signed packs, so your own check and a reviewer's pack cannot disagree.

### 1. Declare: `tellproof.policy.json`

```bash
tellproof-agent policy init --name "Claims Triage Agent" --timezone Australia/Sydney
```

This writes a policy declaring the models and regions the log has seen.

> [!WARNING]
> Review it before you rely on it. A declaration says what the system *may* use, so remove anything that should not be there. `policy init` never fills the registers: a change or an incident is something a person records.

<details open>
<summary><b>An example <code>tellproof.policy.json</code></b></summary>

```json
{
  "$schema": "https://raw.githubusercontent.com/Lumetrix-Advanced-Technologies/tellproof/main/spec/policy.schema.json",
  "organisation": "Kestrel Insurance",
  "ai_system": "Claims Triage Agent",
  "environment": "production",
  "timezone": "Australia/Sydney",
  "declared_models": [
    { "provider": "openai", "model": "gpt-4.1-mini", "role": "triage" },
    { "provider": "aws.bedrock", "model": "anthropic.claude-sonnet-4-6", "role": "summaries" }
  ],
  "declared_regions": ["us-east-1", "ap-southeast-2"],
  "changes": [
    { "date": "2026-10-14", "kind": "model", "what": "Summaries moved from Sonnet 4.5 to 4.6 after the regression suite passed" }
  ],
  "incidents": [
    { "id": "INC-42", "opened": "2026-10-13T03:10:00Z", "acknowledged": "2026-10-13T03:25:00Z", "summary": "Guardrail block spike after a prompt deploy" }
  ],
  "evals": [
    { "suite": "triage-regression", "model": "gpt-4.1-mini", "prompt_version": "14", "ran_at": "2026-10-12T00:00:00Z", "pass_rate": 0.96 }
  ],
  "ci": { "required_gates": ["lint", "tests"], "canary_gates": ["tests"] }
}
```

</details>

| Field | Used by | Meaning |
| --- | --- | --- |
| `ai_system`, `organisation`, `environment` | all | Who and what is being assessed. |
| `timezone` | all | IANA time zone for periods and change dates (default UTC). |
| `declared_models` | EO-LIN-01, AI-BOM | Every model the system may use, as the agent reports provider and model; `role` is optional. |
| `declared_regions` | EO-RES-01 | Every region inference may run in. |
| `changes` | EO-CHG-01 | The material-change register: each model or prompt change, by local date (`kind` is `model` or `prompt`). A change the telemetry shows matches an entry of the same kind within a day. |
| `incidents` | EO-INC-01 | The incident register. An anomaly hour is covered by an incident opened within an hour of it and acknowledged within an hour of opening. |
| `evals` | EO-EVAL-01 | Evaluation runs recorded outside the telemetry, added to those the agent sees (Langfuse scores, SDK spans). |
| `ci` | `ci verify` | Optional. `required_gates` must have run and passed on every commit; each of `canary_gates` must have had its must-fail canary caught ([coding-agent evidence](#evidence-for-coding-agents-ci-gates-per-commit)). |

The [JSON Schema](spec/policy.schema.json) gives editors completion and validation; `tellproof-agent policy validate` checks the file the way `check` reads it (unknown keys are refused, so a typo never silently weakens a declaration). The agent looks for the policy at `--policy`, then `$TELLPROOF_POLICY`, then `./tellproof.policy.json`, then `<data-dir>/tellproof.policy.json` (where a container keeps it).

### 2. Check: `tellproof-agent check`

```bash
tellproof-agent check                                   # the last 30 days with data
tellproof-agent check --since 2026-10-01 --until 2026-10-31 --junit check.xml
tellproof-agent check --json > october-check.json
tellproof-agent check --strict                          # attention fails too
```

```
Tellproof check: Claims Triage Agent (production), 2026-10-09 to 2026-10-17 (Australia/Sydney)
  Evidence: 168 batches from agent agt_locald74e27b542973a66bedec4fc, verified (signatures and chain).

! ATTENTION  EO-LOG-01  Trace coverage
             No provider usage supplied
             target: >= 98.0% of provider-billed tokens traced
             supports: EU AI Act Art. 12 Record-keeping (automatic logging); ISO/IEC 42001 A.6.2.8 AI system event logs*; ...
✓ PASS       EO-LIN-01  Model and provider lineage
             6 of 6 models observed are declared
             target: Every model in use is on the declared inventory
✕ FAIL       EO-CHG-01  Material-change log
             0 change(s) logged, 2 unlogged transition(s)
             target: Every model or prompt change seen in telemetry is in the change register
             missing change: 2026-10-14T03:17:00Z model anthropic.claude-sonnet-4-5 -> anthropic.claude-sonnet-4-6 (add to "changes")
...
2 pass, 1 attention, 7 fail.
* Framework mapping not yet verified against the framework's text.
```

The log is verified first; a log that fails verification is never evaluated. Each objective shows what was measured, its target, the framework references it supports, and, where a register is short, exactly which entries are missing. Exit status: **0** no objective failed (with `--strict`, none needs attention either), **1** one did or the log failed verification, **2** a usage or policy error. `--junit` writes one test case per objective for any CI test reporter.

The dashboard shows the same check for the selected range in its **Evidence objectives** panel, re-read on every refresh, so a policy edit shows at once:

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/objectives-dark.png">
  <img src="docs/objectives.png" alt="The dashboard's Evidence objectives panel: eleven objectives with pass, attention or fail, what was measured and the target">
</picture>

> [!NOTE]
> `check` is a self-assessment: the evidence is signed by your own agent's key and evaluated by you. A [pack](#signed-evidence-packs-early-access) is the same evaluation, signed by an independent issuer.

### Prove it can fail: `check --self-test`

A green check is worth something only if the same check would have gone red. `--self-test` asks that of your own evidence and policy: for each objective it adds the least synthetic evidence that should fail it (an undeclared model, unredacted personal data, a missed canary, an unregistered change, ...) to a copy of the input and evaluates again, and it tampers with a copy of the log (an edited number, a batch re-signed with another key, a missing batch) and requires verification to refuse each one. Your log and policy are never changed, and the agent's private key is never copied.

```bash
tellproof-agent check --self-test
tellproof-agent check --self-test --json > self-test.json
```

```
Tellproof self-test: Claims Triage Agent, 2026-10-09 to 2026-10-17
  Can each objective fail on this evidence and policy? A violation is added to a copy of the evidence; the objective must fail.

✓ CAN FAIL     EO-LIN-01  Model and provider lineage (now pass)
               added one call to an undeclared model: fail
✕ VACUOUS      EO-CHG-01  Material-change log (now pass)
               the change register has a model and a prompt change within a day of every day in the period, so no change can ever be unlogged
✓ CAN FAIL     EO-CAN-01  Guardrail canaries (now pass)
               added 14500 guardrail canaries missed: fail
...
Is tampering with the log detected?
✓ DETECTED     one number edited in the newest batch
✓ DETECTED     the newest batch re-signed with another key
✓ DETECTED     the first batch removed

10 of 11 objectives can fail; 1 vacuous, 0 cannot fail, 0 untested.
```

**Vacuous** is the finding to act on: a register written so that nothing could ever be missing from it (a change logged for every day, an incident open every hour) turns its objective into a rubber stamp. Exit status **1** for a vacuous or unfailable objective or undetected tampering, so the self-test can run in CI next to `check`. Two limits are printed with every run: deleting the newest batches cannot be detected from the log alone (the hosted chain and daily public checkpoints catch that), and the self-test cannot show the telemetry itself is complete, which is what trace coverage and canaries measure.

### In CI

The repository is also a GitHub Action. Validate the policy on every pull request that changes it:

```yaml
- uses: Lumetrix-Advanced-Technologies/tellproof@v0.5.2
  with: { command: validate, policy: tellproof.policy.json }
```

Or gate a release on the evidence, with a copy of the agent's data directory (from a scheduled job, an artifact or a volume snapshot):

```yaml
- uses: Lumetrix-Advanced-Technologies/tellproof@v0.5.2
  with: { command: check, policy: tellproof.policy.json, data-dir: evidence/, strict: "true" }
```

Add `command: self-test` as a second step to show the check could have failed.

It installs the `tellproof-agent` release it belongs to from PyPI (so pinning the Action pins the agent; `version:` picks another, `latest` the newest), writes `tellproof-check.xml` (JUnit), puts the result in the job summary, and fails the job when an objective fails.

### 3. Inventory: `tellproof-agent aibom`

```bash
tellproof-agent aibom --out aibom.cdx.json
tellproof-agent aibom --since 2026-10-01 --until 2026-10-31 > october.cdx.json
```

A [CycloneDX 1.6](https://cyclonedx.org/) AI bill of materials, built from the verified log rather than a hand-kept list. The AI system is the root component; every model declared or seen is a `machine-learning-model` component with its provider as supplier and properties for whether it was declared and observed, its regions, calls, input and output tokens, first and last hour seen, and prompt versions. A model that is declared but never seen, or seen but never declared, is visible at a glance. The BOM's metadata names the agent and the latest batch digest it was built from. Any CycloneDX tool can read it (Dependency-Track, the CycloneDX CLI, SBOM platforms).

## Evidence for coding agents: CI gates per commit

A green CI badge says the last run passed. It does not say which checks ran on *this* commit, whether the test step ran any tests, or whether the tests could have caught a broken change. That matters more as coding agents write more of the code. `tellproof-agent ci` turns a CI run into evidence about one commit:

| Command | What it does |
| --- | --- |
| `ci run --gate <name> [--junit <file>] -- <command>` | Runs a gate (tests, lint, a scan) with its output and exit status passed straight through, and records the gate's name, exit status, duration, a SHA-256 of the command line and, with `--junit`, the report's counts (cases counted, not the summary attributes). Never the output, the code or the command line itself. |
| `ci canary --gate <name> --patch <file> -- <command>` | A **must-fail canary**: applies a patch that breaks the code to a temporary worktree (your checkout is untouched), runs the same gate there and requires it to **fail**. Exit 0 when the gate caught the breakage, 1 when it passed (the gate cannot tell broken code from working code). |
| `ci attest [--base <rev>] [--key <file> \| --unsigned]` | Writes the commit's in-toto statement: every gate and canary (the latest run of each, with the number of runs, so a gate retried until green shows it), the CI run and its URL, and how many commits in `base..HEAD` name a coding agent (Claude, Copilot, Cursor, Aider, Devin, Codex, Gemini, Jules, Amazon Q), by name and count only. Signed with an Ed25519 key in a DSSE envelope, or bare for GitHub artifact attestations. |
| `ci verify [--pub <file> \| --unsigned] <statement>` | Checks the signature, the commit (the checked-out one by default), that every required gate passed, that a test gate ran at least one test and has no failing tests in its report, and that every canary gate's canary was caught. Requirements come from the policy's `ci` section or `--require` and `--canary`. |
| `ci keygen --out <file>` | Makes a signing key (and `<file>.pub`). Store the key as a CI secret, `TELLPROOF_CI_KEY`; publish the public key. |

```bash
tellproof-agent ci run --gate lint -- golangci-lint run
tellproof-agent ci run --gate tests --junit report.xml -- pytest --junitxml=report.xml   # or gotestsum --junitfile report.xml
tellproof-agent ci canary --gate tests --patch ci/canaries/skip-auth-check.patch -- pytest tests/test_auth.py
tellproof-agent ci attest --base origin/main --out ci-evidence.json             # signed with $TELLPROOF_CI_KEY
tellproof-agent ci verify --pub ci.pub ci-evidence.json
```

```
Tellproof CI evidence: git+https://github.com/acme/app at 421bbd4a7238b1db67d8384bc838a373143a4728 (signature verified)
  Run: https://github.com/acme/app/actions/runs/1234567
✓ OK   commit                   statement is for 421bbd4a7238…, expected 421bbd4a7238…
✓ OK   gate lint                passed
✓ OK   gate tests               passed: 412 tests, 3 skipped
✓ OK   canary tests             caught: the gate failed on known-bad input (exit 1)
Coding agents: 4 of 5 commits in origin/main..421bbd4a7238… name one (claude 4).
```

A canary patch is a small, deliberate bug in code that matters: an authorisation check removed, a boundary flipped, a validation skipped. Keep a few under `ci/canaries/`; if a canary stops being caught, your tests stopped covering that code. Add `.tellproof-ci/` (where runs are recorded until `attest`) to `.gitignore`.

<details>
<summary><b>A GitHub workflow</b>, signed with GitHub artifact attestations (no key to manage)</summary>

```yaml
permissions: { contents: read, id-token: write, attestations: write }
steps:
  - uses: actions/checkout@v4
    with: { fetch-depth: 0 }
  - run: pip install tellproof-agent
  - run: tellproof-agent ci run --gate lint -- make lint
  - run: tellproof-agent ci run --gate tests --junit report.xml -- make test-junit
  - run: tellproof-agent ci canary --gate tests --patch ci/canaries/skip-auth-check.patch -- make test
  - run: tellproof-agent ci attest --unsigned --base origin/${{ github.base_ref || 'main' }} --out ci-evidence.json --predicate ci-predicate.json
  - uses: actions/attest@v2
    with:
      subject-path: ci-evidence.json
      predicate-type: https://lumetrix.example/tellproof/ci/v0.1
      predicate-path: ci-predicate.json
  - run: tellproof-agent ci verify --unsigned ci-evidence.json   # gate the merge on the policy's ci section
```

Anyone can then check it: `gh attestation verify ci-evidence.json -R acme/app`, then `tellproof-verify ci --unsigned --commit <sha> --require lint,tests --canary tests ci-evidence.json`.

</details>

---

---

## Configuration reference

`run --local` and `connect` need no file. For anything more, write a JSON file and pass `--config agent.json`.

<details>
<summary><b>A complete config file, with every source off</b> (<a href="agent.example.json"><code>agent.example.json</code></a>)</summary>

```json
{
  "server": "https://<server>",
  "data_dir": "./.tellproof-agent",
  "otlp_listen": "127.0.0.1:4318",
  "dashboard_listen": "127.0.0.1:4319",
  "langfuse": { "enabled": false, "base_url": "https://cloud.langfuse.com",
                "public_key_env": "LANGFUSE_PUBLIC_KEY", "secret_key_env": "LANGFUSE_SECRET_KEY", "eval_scores": [] },
  "usage": {
    "openai":    { "enabled": false, "admin_key_env": "OPENAI_ADMIN_KEY", "project_ids": [] },
    "anthropic": { "enabled": false, "admin_key_env": "ANTHROPIC_ADMIN_KEY", "workspace_ids": [] },
    "bedrock":   { "enabled": false, "region": "ap-southeast-2" }
  },
  "salt_secret_env": "TELLPROOF_SALT_SECRET",
  "salt_secret_file": "",
  "aggregation": { "grace_minutes": 5, "first_batch_seconds": 60,
                   "anomaly": { "min_calls": 20, "error_rate": 0.05, "guardrail_block_rate": 0.03 } }
}
```

</details>

| Key | Default | Meaning |
| --- | --- | --- |
| `server` | none | The Tellproof server, for connected mode. `https`, or `http` for localhost only. |
| `data_dir` | `./.tellproof-agent` (`/data` in the container) | The key, queue, local log, commitment leaves and salt. |
| `otlp_listen` | `127.0.0.1:4318` | The span receiver. A non-loopback address needs `otlp_allow_remote`. |
| `otlp_allow_remote` | `false` | Allow listening beyond loopback. The receiver has no authentication, so do this only on a network you control. |
| `dashboard_listen` | `127.0.0.1:4319` | The local-mode dashboard; `off` turns it off. Same loopback rule. |
| `langfuse.*` | off | The [Langfuse puller](#langfuse). |
| `usage.*` | off | The [provider usage pollers](#provider-usage). |
| `salt_secret_env` / `salt_secret_file` | `TELLPROOF_SALT_SECRET` | The secret that salts trace commitments. With `connect` or `run --local` and none set, a random one is kept in `<data-dir>/salt.secret`. |
| `aggregation.grace_minutes` | `5` | How long after the hour a window stays open (1 to 55). |
| `aggregation.first_batch_seconds` | `60` | When the open hour is sent once after a start (0 to 3600). |
| `aggregation.anomaly.*` | `20`, `0.05`, `0.03` | An hour with at least `min_calls` calls and an error rate or guardrail-block rate above these is an anomaly window. |

Secrets are never written in the file. Each `*_env` key names the environment variable that holds a secret, and the salt can come from a file with mode `0600`.

### Environment variables

| Variable | Overrides | Notes |
| --- | --- | --- |
| `TELLPROOF_SERVER` | `server` | |
| `TELLPROOF_DATA_DIR` | `data_dir` | |
| `TELLPROOF_OTLP_LISTEN` | `otlp_listen` | |
| `TELLPROOF_DASHBOARD_LISTEN` | `dashboard_listen` | |
| `TELLPROOF_OTLP_ALLOW_REMOTE=1` | `otlp_allow_remote` | Set in the container image, whose documented commands publish ports on the host's loopback only. |
| `TELLPROOF_ENROL_TOKEN` | | The token for `enrol` and `connect` when it is not given on the command line. |
| `TELLPROOF_SALT_SECRET` | | The default salt variable. |
| `TELLPROOF_NO_BANNER=1` | | No welcome banner, even in a terminal (also off in pipes, files, CI and `TERM=dumb`). `NO_COLOR` keeps it without colour. |
| SDK: `TELLPROOF_AGENT_URL`, `TELLPROOF_SERVICE`, `TELLPROOF_ENVIRONMENT`, `TELLPROOF_DISABLED` | | See [Instrument your app](#instrument-your-app). |

---

## More evidence sources: Langfuse and provider usage

Each source is read-only and off until you turn it on. Content is dropped the same way as for spans.

### Langfuse

```json
"langfuse": { "enabled": true, "base_url": "https://cloud.langfuse.com",
              "public_key_env": "LANGFUSE_PUBLIC_KEY", "secret_key_env": "LANGFUSE_SECRET_KEY",
              "eval_scores": [ { "name": "faithfulness", "suite": "rag-eval", "pass_at_least": 0.8 } ] }
```

The agent pulls traces from your Langfuse project with its keys. `eval_scores` turns Langfuse scores into evaluation runs: scores named `name` are recorded as runs of `suite`, passing when the score is at least `pass_at_least`.

### Provider usage

```json
"usage": {
  "openai":    { "enabled": true, "admin_key_env": "OPENAI_ADMIN_KEY", "project_ids": ["proj_..."] },
  "anthropic": { "enabled": true, "admin_key_env": "ANTHROPIC_ADMIN_KEY", "workspace_ids": ["wrkspc_..."] },
  "bedrock":   { "enabled": true, "region": "ap-southeast-2" }
}
```

The agent polls the providers' usage APIs with an admin key (limit it to this system's projects or workspaces) and puts each hour's billed tokens in the batch, so traced tokens can be compared with billed ones: that is the trace-coverage objective. Bedrock usage comes from CloudWatch in `region`, with the default AWS credential chain, and needs a `tellproof-agent-bedrock-*` binary (or `-tags bedrock`).

---

## Deploying the agent

### Next to the app on one host

For development, `tellproof-agent start`. For a server, run the agent as a service (systemd, launchd, or a container with `--restart unless-stopped`) and leave the defaults: the SDK finds it on `127.0.0.1:4318`.

**Memory.** The agent adds spans up as they arrive and keeps no span: about 12 MB idle, and around 40 MB with 100,000 model calls in one hour, each in its own trace. On a small device, cap it with Go's soft limit, `GOMEMLIMIT=32MiB`; it collects garbage sooner as it nears the limit. The SDKs buffer at most 2,048 spans and drop new ones while the buffer is full (the agent is away), so the app never grows waiting for it.

### Docker Compose

```yaml
services:
  app:
    build: .
    environment:
      TELLPROOF_AGENT_URL: http://tellproof-agent:4318
      TELLPROOF_SERVICE: claims-api
      TELLPROOF_ENVIRONMENT: production
  tellproof-agent:
    image: ghcr.io/lumetrix-advanced-technologies/tellproof-agent:latest
    command: ["run", "--local"]                 # or ["connect", "<token>"], with TELLPROOF_SERVER set
    ports: ["127.0.0.1:4319:4319"]              # the dashboard, on the host's loopback only
    volumes: ["tellproof-agent:/data"]
volumes:
  tellproof-agent: {}
```

### Kubernetes

Run the agent as a sidecar in the app's pod, so spans never leave the pod, and keep the data directory on a persistent volume.

```yaml
containers:
  - name: app
    env:
      - { name: TELLPROOF_AGENT_URL, value: "http://127.0.0.1:4318" }
  - name: tellproof-agent
    image: ghcr.io/lumetrix-advanced-technologies/tellproof-agent:latest
    args: ["run", "--local"]
    volumeMounts: [{ name: tellproof-data, mountPath: /data }]
```

`kubectl port-forward pod/<pod> 4319` opens the dashboard. If one agent serves several pods, restrict who can reach port 4318 with a NetworkPolicy: the receiver has no authentication.

### Serverless and short scripts

Call `tellproof.flush()` (in Node.js, `await tellproof.flush()`) before the handler returns, and run the agent where the function can reach it, set with `TELLPROOF_AGENT_URL`.

---

## Signed evidence packs (early access)

The local log is signed by your own key, and `check` is your own evaluation of it. Reviewers usually need evidence signed by someone other than the vendor. The hosted Tellproof service adds that: the same evaluation of the eleven objectives, signed by an independent issuer (a key held in AWS KMS), chained month to month, listed in a public daily checkpoint, and shared with reviewers by link.

[Request early access](https://github.com/Lumetrix-Advanced-Technologies/tellproof/issues/new?template=early-access.yml); everything else on this page works today without it. Once you have an AI system and an enrolment token in the Tellproof app, run the agent connected, keeping the SDK line in your app as before:

```bash
TELLPROOF_SERVER=https://<server> tellproof-agent connect <token>
```

[What a pack contains, the step-by-step workflow, and the objectives](HOW-IT-WORKS.md#signed-evidence-packs-the-hosted-service).

---

## Verify a pack

### In the browser

Open a share link: the verify page checks the pack in your browser. Or open the verify page with no link, choose the issuer's JWKS file and one or more pack files, and it checks them offline, with no network at all.

### Offline, with tellproof-verify

```bash
tellproof-verify --jwks issuer-jwks.json pack.dsse.json
tellproof-verify --jwks https://<issuer>/.well-known/jwks.json 2026-09.dsse.json 2026-10.dsse.json   # oldest first: also checks the chain
tellproof-verify --json --jwks issuer-jwks.json pack.dsse.json                                      # a machine-readable report
```

```
spec/vectors/pass/KST-CTA-0002.dsse.json
  pack ................. KST-CTA-0002 (seq 2)
  digest ............... sha256:91ceb4233b19d3164c6a58292166c052cae2a5cf1cfa680b55f4ad5f6bb9c7c8
  signature ............ valid (key test-p256-2026-10)
  subject digest ....... matches the pack contents
  chain link ........... links to the previous pack
  disclosed trace ...... included in the commitment
  result ............... VERIFIED
RESULT: VERIFIED
```

It checks the issuer's signature (against a key that was valid when the pack was issued and has not been revoked), that the subject digest matches the contents, the link to the previous pack, and any disclosed trace's Merkle proof. Exit status: **0** all verified, **1** a check failed, **2** a usage or input error.

**Prove a pack is in a daily checkpoint:**

```bash
tellproof-verify checkpoint --jwks issuer-jwks.json --prev 2026-10-15.json 2026-10-16.json pack.dsse.json
```

This checks the checkpoint's signature, its Merkle root and its link to the previous day, and that each pack given is in it.

### In Python

```bash
pip install cryptography
python3 concierge/verify.py issuer-jwks.json pack.dsse.json
```

To write your own verifier, see [`spec/vectors/README.md`](spec/vectors/README.md) and [Verification](HOW-IT-WORKS.md#verification).

---

## CLI reference

### tellproof-agent

Also `tellproof` (installed with the SDK) and `npx tellproof` (in an npm project): the same commands.

```
tellproof-agent                                   # in a terminal: the welcome banner and first steps
tellproof-agent init [--name <ai system>] [--timezone <tz>] [--yes] [--no-start] [--force]
tellproof-agent doctor [--config <agent.json>] [--data-dir <dir>] [--policy <file>] [--json]
tellproof-agent demo [--data-dir <new dir>] [--days 7] [--listen <host:port>]
tellproof-agent start | restart [--config <agent.json>] [--data-dir <dir>] [--open]
tellproof-agent stop [--config <agent.json>] [--data-dir <dir>] [--force]
tellproof-agent status [--config <agent.json>] [--data-dir <dir>] [--json]
tellproof-agent logs [--config <agent.json>] [--data-dir <dir>] [-n <lines>] [-f]
tellproof-agent run --local [--config <agent.json>]
tellproof-agent report [--config <agent.json>] [--data-dir <dir>] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--json]
tellproof-agent dashboard [--config <agent.json>] [--data-dir <dir>] [--listen <host:port>]
tellproof-agent policy init [--policy <file>] [--data-dir <dir>] [--name <ai system>] [--timezone <tz>] [--force]
tellproof-agent policy validate [--policy <file>]
tellproof-agent check [--policy <file>] [--data-dir <dir>] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--json] [--junit <file>] [--strict]
tellproof-agent check --self-test [--policy <file>] [--data-dir <dir>] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--json]
tellproof-agent aibom [--policy <file>] [--data-dir <dir>] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--out <file>]
tellproof-agent ci run --gate <name> [--junit <file>] [--dir <dir>] -- <command> [args...]
tellproof-agent ci canary --gate <name> [--patch <file>] [--dir <dir>] -- <command> [args...]
tellproof-agent ci attest [--dir <dir>] [--base <rev>] [--key <file>] [--unsigned] [--out <file>] [--predicate <file>]
tellproof-agent ci verify [--policy <file>] [--require a,b] [--canary a,b] [--commit <sha>] [--pub <file>] [--unsigned] [--json] <statement>
tellproof-agent ci keygen --out <file>
tellproof-agent connect [--config <agent.json>] <token>
tellproof-agent enrol --config <agent.json> [--token <token>]
tellproof-agent [run] --config <agent.json> [--dry-run] [--replay <export.jsonl>]
tellproof-agent --version                          # also -v or version; in a terminal, with the banner
```

| Flag | Commands | Meaning |
| --- | --- | --- |
| `--config <file>` | all | The agent config file (optional for `run --local`, `connect`, `report` and `dashboard`). |
| `--name`, `--timezone` | `init` | The AI system's name (default: the directory's) and IANA time zone (default `$TZ`, else UTC); in a terminal, init asks. |
| `--yes`, `--no-start`, `--force` | `init` | Take every default and ask nothing; do not start the agent (without a terminal it never does; otherwise it starts in the background); replace an existing `tellproof.policy.json`. |
| `--data-dir <dir>` | `start`, `stop`, `restart`, `status`, `logs` | The data directory of the agent to act on (default from the config, `./.tellproof-agent`). |
| `--open` | `start`, `restart` | Open the dashboard in the browser. |
| `--force` | `stop` | End the agent if it has not stopped 15 seconds after being asked. |
| `--json` | `status` | Print JSON: `running`, `data_dir` and the agent's pid, start time, version, ID and addresses. |
| `-n <lines>`, `-f` | `logs` | How many lines to print (default 40); keep printing new ones. |
| `--json` | `doctor` | Print the checks as JSON (`ok`, and each check's `status`, `detail` and `fix`). |
| `--data-dir`, `--days`, `--listen` | `demo` | A new or empty directory for the synthetic log (default a temporary one), how many days (1 to 90), and the dashboard's address. |
| `--local` | `run` | Local mode: no server, signed batches kept in the data directory, dashboard served. |
| `--dry-run` | `run` | Print every batch with its payload decoded; send, queue and store nothing. |
| `--replay <file>` | `run` | Feed an OTLP JSON export (one request per line) through the pipeline, then exit. |
| `--token <token>` | `enrol` | The one-time enrolment token (default `TELLPROOF_ENROL_TOKEN`). |
| `--data-dir <dir>` | `report`, `dashboard` | Read this data directory instead of the configured one. |
| `--since`, `--until` | `report` | The first and last day to sum (UTC dates). Verification always covers the whole log. |
| `--json` | `report` | Print the summary as JSON. |
| `--listen <host:port>` | `dashboard` | Where to serve it (default `dashboard_listen`, else `127.0.0.1:4319`). |
| `--policy <file>` | `policy`, `check`, `aibom` | The policy file (default `$TELLPROOF_POLICY`, `./tellproof.policy.json`, then `<data-dir>/tellproof.policy.json`). |
| `--name`, `--timezone`, `--force` | `policy init` | The AI system's name, its IANA time zone (default `$TZ`, else UTC), and replace an existing file. |
| `--since`, `--until` | `check`, `aibom` | The first and last local day (the policy's time zone). `check` defaults to the last 30 days with data. |
| `--json`, `--junit <file>`, `--strict` | `check` | Print JSON; also write JUnit XML; fail on attention as well as fail. |
| `--self-test` | `check` | Show that each objective fails on evidence built to fail it and that tampering with the log is detected ([details](#prove-it-can-fail-check---self-test)). |
| `--out <file>` | `aibom` | Write the BOM to a file instead of standard output. |

Exit status: **0** ok, **1** a runtime error (for `status`, **3** means not running; for `doctor`, a failed check; for `report`, a verification failure; for `check`, a failed objective or an unverified log; for `policy validate`, an invalid policy), **2** a usage or configuration error.

### tellproof-verify

```
tellproof-verify --jwks <file|https-url> [--json] pack1.json [pack2.json ...]
tellproof-verify checkpoint --jwks <file|https-url> [--prev yesterday.json] checkpoint.json [pack.json ...]
tellproof-verify ci (--pub <key.pem> | --unsigned) --commit <sha> [--require a,b] [--canary a,b] [--json] statement.json
tellproof-verify --version
```

Exit status: **0** all verified, **1** a check failed, **2** a usage or input error.

---

## Troubleshooting

Start with `tellproof-agent doctor`. It checks the configuration, the data directory and its permissions (and whether git would commit the agent's key), whether the agent answers on its ports or something else holds them, where the SDKs will send (`TELLPROOF_AGENT_URL`), the evidence log's verification, whether spans are still arriving, how many spans arrived with content (dropped at parse time), the clock, and the policy; each problem comes with its fix. It sends no spans and contacts only the agent's own addresses. `--json` for scripts; exit 1 when a check fails. What doctor cannot see is the app's side: `tellproof.stats()` there shows `spans`, `exported`, `dropped` and `export_errors`.

| Symptom | What to check |
| --- | --- |
| `tellproof: command not found` | Installed with npm: run it as `npx tellproof` (npm keeps commands in `node_modules/.bin`, not on your PATH). Installed with pip: activate the virtual environment you installed into, or add pip's script directory (`~/.local/bin` with `--user`) to PATH. `tellproof --version` then shows the agent's and the SDK's versions. |
| `npx tellproof` says `tellproof: not found`, in a folder whose path has a `:` | `:` separates PATH entries, so npm cannot add that folder's `node_modules/.bin` to PATH. Run `./node_modules/.bin/tellproof`, or move the project to a path without `:`. |
| `tellproof: the Tellproof agent is not installed` | Install it with the SDK: `pip install "tellproof[agent]"`, or `npm install tellproof` without `--omit=optional`. |
| `start` says the agent stopped while starting | It prints the reason from the log: usually a port in use (`TELLPROOF_OTLP_LISTEN`, `TELLPROOF_DASHBOARD_LISTEN`) or a data directory enrolled for connected mode. |
| "an agent already runs on …" | One agent per data directory: `tellproof-agent status` shows it, `tellproof-agent stop` stops it. |
| `stop` or `status` finds no agent, but one is running | They look in the data directory of the current directory. Run them where you started the agent, or pass `--data-dir`. |
| The dashboard says "Waiting for your first model call" | Is the app calling `tellproof.init()` before its first model call, and can it reach `TELLPROOF_AGENT_URL`? `tellproof.stats()` shows `exported` and `export_errors`. The first batch comes about a minute after the first span. |
| `stats()` shows `spans: 0` | The client library is not instrumented: check `stats()["instrumented"]`. In Node.js ES modules, `await tellproof.ready()`; in bundled apps, use `tellproof.instrument({...})`. Or wrap the call in `llm_call`. |
| Counts are doubled | The SDK and another OpenTelemetry exporter both send the same calls. Use one. |
| Streamed OpenAI calls have no tokens | Turn on `stream_usage` (`streamUsage`), or set `stream_options` yourself. |
| `npx tellproof-agent` says "no binary for …" | Your platform is not linux, darwin or win32 on x64 or arm64, or optional dependencies were omitted (`--omit=optional`). Download a binary from Releases instead. |
| "otlp_listen … is not a loopback address" | Set `otlp_allow_remote` (or `TELLPROOF_OTLP_ALLOW_REMOTE=1`) and restrict the port at the network. |
| The dashboard says it "answers only on localhost" | Open it as `http://127.0.0.1:4319` or `http://localhost:4319` (or a `*.localhost` name). |
| Port 4319 is in use | `TELLPROOF_DASHBOARD_LISTEN=127.0.0.1:4329`, or `off`. |
| `report` says FAILED | The message names the first bad batch: edited (signature), missing, or out of order. The figures cover only the batches before it. Restore the data directory from a backup. |
| `check` says "No policy yet" or a policy error | `tellproof-agent policy init`, then review the file; `tellproof-agent policy validate` names the field at fault. |
| `check --self-test` says VACUOUS | A register leaves no room for a violation, for example a change recorded for every day. Record the changes and incidents that happened, not blanket entries. |
| EO-CAN-01 fails | Your guardrail let a known-bad probe through; the measurement names the kind. Fix the guardrail, or check that your canary check returns `True` (or `"block"`/`"flag"`) when it blocks, and does not raise. |
| `ci verify` says a gate "ran no tests" | The gate's JUnit report has no test cases: the test command matched nothing or wrote the report elsewhere. A gate that tests nothing cannot fail. |
| `ci canary` says MISSED | The gate passed with the canary patch applied, so no test covers that code. Add a test that fails on the patch. If the patch does not apply, update it to the current code. |
| `ci attest` finds no gate runs | Gates are recorded per commit in `.tellproof-ci/<commit>/`: run `ci run` in the same job and checkout, or pass the same `--dir`. |
| `check` fails EO-CHG-01 or EO-INC-01 | The output lists each missing change or incident; record the ones you approve in the policy's `changes` or `incidents`. |
| `connect` refuses the data directory | It holds a local-mode agent. Use a new data directory (or volume) for connected mode. |
| No agent appears in the Tellproof app | Check the agent's log for an enrolment error. A used or expired token needs a new one. |

### Upgrading from Plumbline

Tellproof was called Plumbline until v0.2. Everything from v0.2 keeps working:

| v0.2 (Plumbline) | v0.3 and later (Tellproof) | Do you need to change it? |
| --- | --- | --- |
| `pip install plumbline-sdk`, `import plumbline` | `pip install tellproof`, `import tellproof` | Yes: the old packages have been removed from PyPI and npm. |
| `npm install plumbline-sdk` | `npm install tellproof` | Yes. |
| `plumbline-agent` (pip, npm, binary, image) | `tellproof-agent` | Yes. The new agent picks up the old data directory, identity and hash chain. The v0.1 and v0.2 release binaries stay on the Releases page. |
| `PLUMBLINE_*` environment variables | `TELLPROOF_*` | No: the old names are read when the new ones are unset. |
| `plumbline.*` span attributes | `tellproof.*` | No: both are mapped (`spec/semconv-map.json` since v3). |
| `plumbline.policy.json`, `./.plumbline-agent` | `tellproof.policy.json`, `./.tellproof-agent` | No: the old names are used when the new ones do not exist. |
| Batches (`plumbline-batch/v0.2`), packs, signatures | the same | No: the wire formats are frozen, so every existing batch and pack still verifies. |

---

## Develop and contribute

```bash
git clone https://github.com/Lumetrix-Advanced-Technologies/tellproof.git && cd tellproof
make check          # gofmt, vet, staticcheck, govulncheck, race tests, every verifier on every vector, SDK and packaging tests
```

The tests need Go 1.26+, Node.js 22+ and Python 3.10+ with `cryptography`. The Go code uses only the standard library, plus the AWS SDK in the `bedrock` build.

| Target | What it does |
| --- | --- |
| `make check` | All of the test targets below. |
| `make test` | `go test -race ./...`, plus the `bedrock` build's usage poller. |
| `make vectors` | All three verifiers on every test vector. |
| `make sdk-test` | Both SDKs' tests. |
| `make package-test` | The pip and npm packaging tests. |
| `make agent-dry` | A dry run over the sample OTLP export. |
| `make dashboard-demo` | The dashboard on a week of synthetic, signed data (`tools/demolog`). |
| `make release-binaries [GOOS=… GOARCH=…] VERSION=…` | Static binaries for every platform, or for one. |
| `make packages VERSION=…` | The wheels and npm packages, from those binaries. |
| `make agent-image VERSION=…` | The container image. |

| Path | What it is |
| --- | --- |
| `cmd/agent`, `cmd/verify` | The two programs. |
| `internal/` | `otlp` (parsing and content dropping), `aggregate` (hourly windows), `commitment` and `merkle`, `batch` and `batchsign`, `queue`, `enrol`, `langfuse`, `usage`, `agent`, `localreport`, `dashboard`, `policy`, `localcheck`, `aibom`; the evaluator, `evaluate` (the same code that issues packs); and the trust core, `jcs`, `dsse`, `pack` and `checkpoint`. |
| `sdk/python`, `sdk/js` | The SDKs, each with its own README. |
| `web/verify` | The static verify page; `web/security` and `web/docs` are the site's other pages. |
| `spec/` | Schemas, objectives, framework references, the attribute map and the test vectors. |
| `concierge/` | The reference Python verifier and evaluator, and the ingest they are checked against. |
| `action.yml` | The GitHub Action for `policy validate` and `check`. |
| `packaging/` | The pip and npm package builders. |
| `examples/` | Sample Python and Node.js apps with the one-line integration. |

See [CONTRIBUTING.md](CONTRIBUTING.md) for how pull requests land, the [Code of Conduct](CODE_OF_CONDUCT.md), and [How Tellproof works](HOW-IT-WORKS.md) for the design.

Every change keeps two rules: **no content leaves the customer** (no code path in the agent or the SDKs may send prompt or completion text, span content or raw trace IDs), and **the verifiers agree** (test vectors are added, never changed). The SDKs must never break or block the host app.

**Releases.** Pushing a tag `vX.Y.Z` runs the release workflow: checks, six-platform builds, checksums, attestations, a draft release and the container image. Publishing the release runs `packages.yml`, which builds the pip and npm packages from the release's own attested binaries. The SDKs release on `sdk-vX.Y.Z`. See [CHANGELOG.md](CHANGELOG.md).

---

## Security reports and licence

Report vulnerabilities privately, through GitHub's private vulnerability reporting (the Security tab), not in a public issue: see [SECURITY.md](SECURITY.md). Apache License 2.0: see [LICENSE](LICENSE) and [NOTICE](NOTICE).

---

<p align="center">
  <sub>Tellproof is built by <b>Lumetrix</b>. ⭐ Star the repository to follow it, <a href="https://github.com/Lumetrix-Advanced-Technologies/tellproof/issues/new?template=early-access.yml">request early access</a> to signed packs, or open an issue with what your reviewers ask for.</sub>
</p>