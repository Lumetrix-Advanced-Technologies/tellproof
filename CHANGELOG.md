# Changelog

Each release's section is its release notes: the release workflow publishes the section whose heading is the tag, and attests it with the binaries.

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
