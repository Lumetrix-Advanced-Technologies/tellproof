# tellproof (Node.js and TypeScript)

One line to send evidence telemetry from an AI product to the local Tellproof evidence agent.

```bash
npm install tellproof
```

```js
import * as tellproof from 'tellproof'; // or: const tellproof = require('tellproof')
tellproof.init();
```

The SDK records one span per model call with provider, model, region, token counts, operation, timing and error status, and sends it as OTLP JSON to the agent on `http://127.0.0.1:4318` (or `TELLPROOF_AGENT_URL`). The agent aggregates per hour and signs the aggregates: it keeps them in a local evidence log (`tellproof-agent local`) or sends them to Tellproof (`tellproof-agent connect`). **The SDK never reads prompts, completions, function arguments, return values or error messages.**

- No dependencies: Node 18+ built-ins only (`fetch`, `AsyncLocalStorage`). Plain JavaScript with TypeScript types; no build step. `import` and `require` share one instance.
- Never slows or breaks the app: spans go to a bounded buffer; when it is full they are dropped and counted (`tellproof.stats()`). SDK errors are swallowed and counted. Its timers never keep the process alive.
- `TELLPROOF_DISABLED=1` turns it off. `init()` is safe to call more than once.
- Buffered spans are flushed when the event loop drains. In serverless handlers and short scripts, `await tellproof.flush()` before returning.

## Automatic instrumentation

`init()` instruments the client libraries the app has installed, both their CommonJS and their ES module copies:

| Library | Calls |
| --- | --- |
| `openai` | `chat.completions.create` (and `parse`, `stream`, `runTools`, which go through it), `responses.create` (and `stream`, `parse`), `embeddings.create`, `completions.create`. Azure OpenAI is reported as `azure.ai.openai`; other OpenAI-compatible endpoints by their host name. |
| `@anthropic-ai/sdk` | `messages.create` (and `messages.stream`), `beta.messages.create`. `AnthropicBedrock` and `AnthropicVertex` are reported as `aws.bedrock` and `gcp.vertex_ai` with their region. |
| `@aws-sdk/client-bedrock-runtime` | `ConverseCommand`, `ConverseStreamCommand`, `InvokeModelCommand`, `InvokeModelWithResponseStreamCommand` sent with `client.send()` (also `BedrockRuntime`'s methods) |

Per call it reads the request's model and stream flag, and from the response only the model, the usage block, the stop reason and Bedrock's token-count headers. Input tokens follow what the provider bills (Anthropic: uncached + cache writes + cache reads; Bedrock: `inputTokens`, as CloudWatch counts them). You get the library's own objects back: `withResponse()` still works and streams are the same objects.

- **ES modules**: the CommonJS copies are instrumented inside `init()`, the ES module copies a moment later. If a module calls a model straight after `init()`, `await tellproof.ready()` first.
- **Bundled apps or strict package managers** where the SDK cannot find the libraries: `tellproof.instrument({ openai: OpenAI, anthropic: Anthropic, bedrock: await import('@aws-sdk/client-bedrock-runtime') })`.
- **Streams** are recorded once, when they end, error, or are left early (`break`). A stream dropped half-read is recorded after it is garbage-collected. Streams split with `tee()` or `toReadableStream()` are counted without tokens.
- **OpenAI streamed chat completions** report usage only if asked. `tellproof.init({ streamUsage: true })` asks for it (`stream_options.include_usage`) when the app has not set `stream_options`; the stream then ends with one extra chunk with no `choices`. The app's own request object is never changed.
- **Bedrock guardrails**: a Converse call with `guardrailConfig` is a guarded call, `block` when the stop reason is `guardrail_intervened`, else `allow`. `InvokeModelWithResponseStream` reports token counts only inside the streamed body, which the SDK never reads, so those calls are counted without tokens. Callback-style `send(command, callback)` is not instrumented.
- `tellproof.init({ instrument: false })` turns automatic instrumentation off; the helpers still work.

## What the helpers record

Each helper runs a function inside a scope and returns whatever it returns (a promise for async functions). Context follows `await`, timers and callbacks, so concurrent requests never mix.

| Helper | Records |
| --- | --- |
| `prompt(name, version, fn)` | the prompt (name and version, never its text) used by calls inside |
| `personalData({ redacted }, fn)` | spans inside handle personal data; `redacted`: removed or masked before the model saw it |
| `guardrail(decision?, fn)` | the guardrail decision (`allow`, `block`, `flag`) for calls inside; `fn` gets `g`: `g.allow()`, `g.block()`, `g.flag()` |
| `tool(name, { highImpact, approved }, fn)` | a tool call; a throw or rejection marks it failed; `fn` gets `t`: set `t.approved = true` once approved |
| `agentRun(name, fn)` | one agent run; model changes are tracked per agent |
| `escalation()` | the request was handed to a person |
| `trace(fn)` | groups everything inside into one trace (for example one web request) |
| `llmCall({ provider, model, operation, region }, fn)` | a model call the SDK does not instrument; `fn` gets `c`: `c.usage(inputTokens, outputTokens)` |

```js
app.post('/claims/:id/triage', (req, res) =>
  tellproof.trace(() =>
    tellproof.prompt('claims-triage', '14', () =>
      tellproof.personalData({ redacted: true }, () =>
        tellproof.guardrail(async (g) => {
          if (!inputPolicyOk(req.body)) {
            g.block(); // recorded as one blocked call; the model was never called
            return res.status(422).end();
          }
          const r = await tellproof.llmCall({ provider: 'openai', model: 'gpt-4.1-mini' }, async (c) => {
            const r = await callModel(req.body);
            c.usage(r.usage.prompt_tokens, r.usage.completion_tokens);
            return r;
          });
          needsReview(r) ? g.flag() : g.allow();
          res.json(r);
        }),
      ),
    ),
  ),
);
```

Names and versions are capped at 128 characters; anything that is not a string or an integer is ignored. Calls inside a `guardrail` scope are held until it ends, so a decision made after the call still applies.

## Guardrail canaries

Show that your guardrail catches something, not only that it ran. The SDK passes synthetic, known-bad probes (prompt injection, jailbreak, made-up personal data, a documented example AWS key) to your own guardrail check and records only the kind and whether it was caught (EO-CAN-01):

```js
tellproof.scheduleCanaries(async (text) => (await myGuardrail.scan(text)).blocked, { every: 3_600_000 }); // now, then hourly
await tellproof.runCanaries(check);   // once: { prompt_injection: true, ... }
tellproof.canary('toxicity', true);   // a canary you run with your own probe
```

The check (sync or async) returns `true`, `'block'` or `'flag'` when it would stop the text; anything else, or a throw, counts as missed. Pass probes to the guardrail only, never to the model. The probes never leave the process (`tellproof.PROBES`); the timer is unref'd, and with the SDK off the check is never called.

## Already on OpenTelemetry?

Point an OTLP/HTTP **JSON** exporter at the agent (`http://127.0.0.1:4318/v1/traces`) and set the `tellproof.*` attributes yourself (`spec/semconv-map.json`). Do not run both for the same calls, or they are counted twice.

## Development

```bash
node --test sdk/js/test/sdk.test.mjs sdk/js/test/instrument.test.mjs
node sdk/js/test/scenario.mjs   # rewrite sdk/testdata/js-export.jsonl
```
