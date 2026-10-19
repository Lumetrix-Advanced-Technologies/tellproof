# Changelog

Each release's section is its release notes: the release workflow publishes the section whose heading is the tag, and attests it with the binaries.

## Unreleased

- **SDKs 0.3.2** require agent 0.5.1 or later (the `[agent]` extra, the npm optional dependency), so upgrading the SDK alone also brings `tellproof --version`.

## v0.5.1

`tellproof --version`, and a stronger supply chain.

- **`tellproof --version`** (also `-v`, `-V` and `version`) shows the agent's version and, run through the SDK's `tellproof` command, the SDK's: `tellproof 0.3.1 (Python SDK)`. In a terminal it comes with the banner and the platform; for scripts the first line is unchanged (`tellproof-agent v0.5.1`). SDKs 0.3.1.
- Troubleshooting: `tellproof: command not found` (npm installs need `npx tellproof`), and `npx` in a folder whose path contains `:`.
- **Supply chain**: every release now carries its signed provenance as a `.intoto.jsonl` asset (added to v0.1.0 to v0.5.0 too); the Python tools the release workflows install are pinned by hash, the agent image's base images by digest, and npm runs at an exact version. `SECURITY.md` gives the private report link, timelines and supported versions.

## v0.5.0

Tellproof is now one install and one line: `pip install "tellproof[agent]"` or `npm install tellproof`, then `tellproof init`. Web requests are scoped automatically, your own guardrail and redaction functions are marked once, and the agent runs in the background with `tellproof start` and `tellproof stop`. This release also closes a content gap the new leak suite found and hardens everything an adopting project runs.

**Upgrading.** Nothing you set up breaks; every helper still works. Three things can change what you see: text that is not an identifier in a model, provider, region or prompt-version attribute is now recorded as `invalid` (it was copied into batches); prompts and completions exported under OpenInference, Traceloop, Langfuse or Vercel AI SDK names now count towards EO-CNT-01, so an app exporting content through them will see it fail until capture is turned off; and the GitHub Action installs its own release of the agent by default (`version: latest` for the newest). The SDKs are 0.3.0 (`sdk-v0.3.0`), released after this agent version, which their `[agent]` extra and optional dependency require.

### Setup and usage

- **One install.** `pip install "tellproof[agent]"` installs the SDK and the agent; `npm install tellproof` brings the agent as an optional dependency (only your platform's binary). Both give a `tellproof` command: `tellproof start`, `tellproof stop`, `tellproof init` and the rest are the `tellproof-agent` commands under a shorter name (`npx tellproof ...` from an npm project). Production installs stay lean: `pip install tellproof`, or `npm ci --omit=optional`. The SDK still imports nothing outside the standard library, and never the agent.
- **Less code: `init()` and, at most, one mark on your own functions.** Each web request is its own trace automatically (Flask, FastAPI, Starlette, Django; any Node.js HTTP server, so Express, Fastify, Koa, Nest and Next.js). Mark your guardrail check with `@tellproof.guardrail_check` (`tellproof.guardrailCheck(fn)`): its decisions count for the request it runs in, before or after the model calls, and it gets the canaries hourly with no other code. Mark your redaction step with `@tellproof.redactor`. `@tellproof.tool` and `@tellproof.agent_run` name themselves after the function, and `tellproof.approve()` approves a tool call. `init()` takes `agent`, `prompt`, `prompt_version` and `personal_data` for every call (or environment variables). OpenAI moderation, Bedrock `ApplyGuardrail`, Azure OpenAI's content filter and OpenAI stored prompts are recorded without code. Outside a request, decisions are never guessed onto a call: use `tellproof.trace()` for workers and scripts. Every existing helper still works.
- **`tellproof-agent start` and `stop`.** `start` runs the local agent in the background, with its dashboard, and returns once it listens; `stop` lets it finish and exit, and also ends an agent running in the foreground. `restart`, `status` (exit 3 when not running) and `logs` (`-f` to follow) complete the set. `init` now starts the agent in the background, so you can start your app in the same terminal. A second agent on the same data directory is refused instead of failing on its port.
- **`tellproof-agent init`**: run it in your app's directory. It recognises a Python or Node.js project (and CommonJS or ES modules), shows the SDK line and the file it goes in, writes `tellproof.policy.json` from two questions (or `--yes`), offers to add the data directory to `.gitignore` because it holds the agent's private key, and starts local mode. It changes no source file, and without a terminal it never starts or waits for anything.
- **`tellproof-agent doctor`**: checks the configuration, the data directory and its permissions, the identity against the mode, the salt secret, whether the agent answers on its ports or something else holds them, where the SDKs will send, the dashboard and its version, the evidence log's verification, stale spans, the clock and the policy, with a fix for each problem. It sends no spans and contacts only the agent's own addresses. `--json`; exit 1 when a check fails.
- **`tellproof-agent demo`**: the dashboard on a synthetic, signed week of an imaginary claims assistant, with a policy for it: most objectives pass, and the ones that fail show what the checks catch (a night deploy without the guardrail, missed canaries, no incident, an unrecorded prompt change). Nothing in it comes from your system.
- The welcome screen lists `init`, `demo` and `doctor`.
- **The README is the setup and usage guide**, starting from the one install; how Tellproof works (the parts, what is signed and what never leaves, the objectives, packs, verification, the security model) is in [HOW-IT-WORKS.md](HOW-IT-WORKS.md).

### Security

- **Security hardening** (see the new [threat model](docs/threat-model.md)): on loopback, the agent's receiver now answers only under a local host name, so a web page cannot post spans into your evidence by DNS rebinding; the `tellproof` command never runs a `tellproof-agent` from the working directory; the Node.js SDK no longer follows a redirect with spans or reads the agent's answer, and both SDKs accept only `http(s)` agent URLs; `langfuse.base_url` must be https (http only on localhost); the GitHub Action installs its own release of the agent by default, so pinning the Action pins the agent (`version: latest` for the newest).
- **Values copied into a batch must be identifiers.** A provider, model, region or prompt version that is not one (spaces, punctuation, more than 128 characters) or that looks like a credential is recorded as `invalid`, and such a canary kind as `other`. The suite found that a prompt or key put in one of these attributes by mistake would otherwise have been signed into a batch. Normal names are unaffected.
- **A content-leak regression suite** (`make leak-test`, run by `make check` and CI): a shared corpus of synthetic prompts, personal data and example-format credentials, in every position and attribute spelling, through the agent, the Langfuse puller and both SDKs; nothing of it, and no raw trace ID, may appear in batches, dry-run output, logs, the dashboard, reports or SDK exports.

### Evidence

- **More content spellings count towards EO-CNT-01** (semconv map v4, recorded in each batch): prompts and completions under OpenInference (`input.value`, `llm.input_messages.*`, …), Traceloop, Langfuse and Vercel AI SDK names, and the newer GenAI `gen_ai.system_instructions` and `gen_ai.tool.call.*`. They were never read and still are not; spans carrying them are now counted. An app that exports content through one of these libraries will see the content objective fail until capture is turned off. Tool definitions, prompt templates and generic fields such as `exception.message` do not count.

## v0.4.0

- **A welcome in the terminal.** `tellproof-agent` on its own shows the Tellproof banner and the first commands to run, and `run --local` starts with the banner and a short status block (where spans go, the dashboard, where the signed evidence is kept, the agent ID) instead of the JSON log. Only in an interactive terminal: pipes, files, CI, `TERM=dumb` and `TELLPROOF_NO_BANNER=1` get exactly the output they got before; `NO_COLOR` keeps the banner without colour. It adapts to narrow terminals.
- **npm releases are automated again, with a human in the loop**: the release workflow stages `tellproof-agent` and its six platform packages, and the SDK workflow stages `tellproof`, through npm trusted publishing (no stored token). Nothing reaches npm until a maintainer approves the staged version with two-factor authentication on npmjs.com; the trusted publishers can only stage. Both workflows also install the built pip and npm packages and run them before anything is published or staged.

## v0.3.0

Plumbline is now **Tellproof**, and governance as code.

- The project, packages and commands are renamed: `pip install tellproof` / `npm install tellproof` (SDK), `tellproof-agent`, `tellproof-verify`, `ghcr.io/lumetrix-advanced-technologies/tellproof-agent`. Nothing set up for v0.2 breaks: `PLUMBLINE_*` variables, `plumbline.*` span attributes, `plumbline.policy.json` and a `.plumbline-agent` data directory are still read, the agent keeps its identity and chain, and batches and packs are unchanged. See "Upgrading from Plumbline" in the README.

- `tellproof.policy.json` declares what an AI system may use (models, regions) and keeps its change, incident and evaluation registers, with a published JSON Schema. `tellproof-agent policy init` writes one from what the log has seen; `tellproof-agent policy validate` checks it.
- `tellproof-agent check` evaluates the verified local log against the eleven evidence objectives with the same evaluator that issues signed packs, shows each objective's measurement, target, framework references and any missing register entries, and exits non-zero when an objective fails (`--strict`: or needs attention). `--json` and `--junit` for automation.
- The dashboard's new **Evidence objectives** panel shows the same check for the selected range.
- `tellproof-agent aibom` writes a CycloneDX 1.6 AI bill of materials of every model declared or seen, from the verified log.
- A GitHub Action (`uses: Lumetrix-Advanced-Technologies/tellproof@v0.3.0`) runs `policy validate` or `check` in CI, with JUnit output and a job summary.
- **Guardrail canaries.** The SDKs pass synthetic, known-bad probes (prompt injection, jailbreak, made-up personal data, an example AWS key) to your own guardrail check and record only the kind and whether it was caught: `tellproof.schedule_canaries(check)` / `scheduleCanaries(check)`, `run_canaries`, `canary(kind, caught)`. A new objective, **EO-CAN-01 Guardrail canaries**, passes when every canary was caught (attention when none ran). The probes never leave the process. A window with canaries is signed as batch schema `plumbline-batch/v0.3` (`spec/batch-v0.3.schema.json`); every other window is unchanged v0.2.
- **`tellproof-agent check --self-test`** shows that each objective fails on evidence built to fail it, flags registers that leave no room for a violation (vacuous), and checks that an edited, re-signed or missing batch fails verification, all on copies; exit 1 if anything cannot fail.
- `report` and the dashboard show canaries caught.
- **Evidence for coding agents: `tellproof-agent ci`.** `ci run` records a CI gate's outcome on the commit (exit status, duration, a digest of the command, JUnit counts; never logs, code or command lines); `ci canary --patch` runs a must-fail canary on deliberately broken code in a temporary worktree; `ci attest` writes a signed in-toto statement per commit, including which commits name a coding agent (Claude, Copilot, Cursor, Aider, Devin, Codex, Gemini, Jules, Amazon Q); `ci verify` and `tellproof-verify ci` check it against the policy's new `ci` section. Works with GitHub artifact attestations (`--unsigned --predicate`) or a key of your own (`ci keygen`).
- The evaluator (`internal/evaluate`) and its Python twin (`concierge/evaluate.py`) are now open source: anyone can recompute a pack's objective results.

## v0.2.0

- A local dashboard: `plumbline-agent run --local` serves it on http://127.0.0.1:4319, showing calls and tokens per model and region, guardrail coverage and personal-data redaction over time, tool use, a timeline of changes, anomalies and evaluations, and the verified evidence chain. Plain HTML and SVG embedded in the agent; loopback only; nothing loaded from elsewhere. `plumbline-agent dashboard --data-dir <dir>` shows a copied log.
- Install the agent with pip or npm: `pip install plumbline-agent`, `npx plumbline-agent run --local`. Each package carries the release binary for its platform; no install scripts, no downloads.
- `plumbline-agent run --local` is the command for local mode (`plumbline-agent local` still works).
- The container publishes the dashboard on port 4319.

## v0.1.0

First public release.

- `plumbline-verify`: verifies evidence packs and their chain offline against an issuer's JWKS (key validity windows and revocation included) and proves a pack is in a daily checkpoint.
- Local mode: `plumbline-agent local` runs with no account, server or token, keeping signed, chained hourly batches in its data directory; `plumbline-agent report` verifies that log and summarises it (text or `--json`).
- `plumbline-agent`: receives OpenTelemetry spans from the SDKs or any OTLP exporter, drops all message content at parse time, pulls Langfuse traces and provider usage (OpenAI, Anthropic; Bedrock in the `-bedrock` build), and sends signed hourly aggregates. `--dry-run` shows exactly what would be sent. `connect <token>` enrols and runs in one command.
- Python and TypeScript SDKs (`pip install plumbline-sdk`, `npm install plumbline-sdk`): one line, `plumbline.init()`, instruments OpenAI, Anthropic and Bedrock clients without reading message content; `llm_call` covers models you serve yourself.
- The static verify page, the pack and batch schemas, and the cross-implementation test vectors.
