# Changelog

Each release's section is its release notes: the release workflow publishes the section whose heading is the tag, and attests it with the binaries.

## v0.4.0

- **A welcome in the terminal.** `tellproof-agent` on its own shows the Tellproof banner and the first commands to run, and `run --local` starts with the banner and a short status block (where spans go, the dashboard, where the signed evidence is kept, the agent ID) instead of the JSON log. Only in an interactive terminal: pipes, files, CI, `TERM=dumb` and `TELLPROOF_NO_BANNER=1` get exactly the output they got before; `NO_COLOR` keeps the banner without colour. It adapts to narrow terminals.
- **npm releases are automated again**, like PyPI: the release workflow publishes `tellproof-agent` and its six platform packages, and the SDK workflow publishes `tellproof`, through npm trusted publishing with provenance, after approval. Both workflows now install the built pip and npm packages and run them before anything is published.

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
