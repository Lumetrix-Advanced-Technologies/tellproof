# tellproof (Python SDK)

One line to send evidence telemetry from an AI product to the local Tellproof evidence agent.

```bash
pip install "tellproof[agent]"   # the SDK and the agent, in one step
tellproof start                  # the agent, in the background, with a dashboard on http://127.0.0.1:4319
```

```python
import tellproof
tellproof.init()
```

`tellproof stop` stops the agent, `tellproof status` says whether it runs. In production the agent usually runs beside the app (a sidecar or a service), so the app needs only `pip install tellproof`.

The SDK records one span per model call with provider, model, region, token counts, operation, timing and error status, and sends it as OTLP JSON to the agent on `http://127.0.0.1:4318` (or `TELLPROOF_AGENT_URL`). The agent aggregates per hour and signs the aggregates: it keeps them in a local evidence log (`tellproof start`) or sends them to Tellproof (`tellproof-agent connect`). **The SDK never reads prompts, completions, function arguments, return values or exception messages**, so there is no content to filter.

- No dependencies: standard library only, Python 3.10+. The `[agent]` extra adds the agent package, which the SDK never imports.
- Never slows or breaks the app: spans go to a bounded background buffer; when it is full, spans are dropped and counted (`tellproof.stats()`). SDK errors are swallowed and counted.
- `TELLPROOF_DISABLED=1` turns it off. `init()` is safe to call more than once.
- Proxies are never used: the agent is local.
- Works across threads, `asyncio` tasks and forked workers (gunicorn, uWSGI).

## Automatic instrumentation

`init()` instruments the client libraries that are installed. Nothing to configure:

| Library | Calls |
| --- | --- |
| `openai` | `chat.completions.create` / `parse` / `stream`, `responses.create` / `parse` / `stream`, `embeddings.create`, `completions.create`; sync, async, streaming. Azure OpenAI is reported as `azure.ai.openai`; other OpenAI-compatible endpoints by their host name. |
| `anthropic` | `messages.create` and `messages.stream` (and `beta.messages`); sync, async, streaming. `AnthropicBedrock` and `AnthropicVertex` are reported as `aws.bedrock` and `gcp.vertex_ai` with their region. |
| `boto3` (Bedrock Runtime) | `converse`, `converse_stream`, `invoke_model`, `invoke_model_with_response_stream` |

Per call it reads the request's model and stream flag, and from the response only the model, the usage block, the stop reason and Bedrock's token-count headers. Input tokens follow what the provider bills (Anthropic: uncached + cache writes + cache reads; Bedrock: `inputTokens`, as CloudWatch counts them), so Tellproof can reconcile traced against billed tokens.

Things to know:

- **Streams** are recorded when they end, are closed, or leave a `with` block, once, with the usage they reported. A stream dropped half-read is recorded on the next call or flush.
- **OpenAI streamed chat completions** report usage only if asked. `tellproof.init(stream_usage=True)` sets `stream_options={"include_usage": True}` when the app has not set it; the stream then ends with one extra chunk with no `choices`. Off by default, so the app's streams are unchanged.
- **Bedrock guardrails**: a Converse call with `guardrailConfig` is a guarded call, `block` when the stop reason is `guardrail_intervened`, else `allow`.
- `invoke_model_with_response_stream` reports its token counts only inside the streamed body, which the SDK never reads, so those calls are counted without tokens. `with_raw_response` calls are counted without tokens too.
- `tellproof.init(instrument=False)` turns automatic instrumentation off; the decorators and scopes still work.

## Tell it the rest

`init()` takes the facts that hold for every call: `agent="triage"`, `prompt="claims-triage", prompt_version="14"`, `personal_data=True` (or `TELLPROOF_AGENT_NAME`, `TELLPROOF_PROMPT`, `TELLPROOF_PROMPT_VERSION`, `TELLPROOF_PERSONAL_DATA=1`). Web requests are scoped for you (Flask, FastAPI, Starlette, Django; `requests=False` turns that off), so for the rest, mark the functions you already have:

```python
@tellproof.guardrail_check            # True (or "block"/"flag") stops the text: decides for this request's calls, and gets canaries hourly
def check(text): ...

@tellproof.redactor                   # this request's calls handle personal data, redacted
def redact(messages): ...

@tellproof.tool(high_impact=True)     # a tool call, named after the function; an exception marks it failed
def issue_refund(claim):
    tellproof.approve()               # a person or a policy approved it
```

OpenAI moderation, Bedrock `ApplyGuardrail` and `guardrailConfig`, Azure OpenAI's content filter and OpenAI stored prompts (their ID and version) are recorded without code. A check's decision counts for its request, before or after the calls; the strictest wins; a request blocked before any call is one blocked call. Outside a web request (a worker, a script), put `@tellproof.trace` on the unit of work; until then decisions are not attributed (`stats()["unscoped"]` counts them).

## Scopes

Scopes say the same explicitly for a block of code and win over the above. Each is a context manager (`with` or `async with`) and a decorator.

| Helper | Records |
| --- | --- |
| `tellproof.trace()` | one request (a trace, its decisions and redaction); automatic for web requests |
| `tellproof.prompt(name, version)` | the prompt (name and version, never its text) used by calls inside |
| `tellproof.personal_data(redacted=False)` | spans inside handle personal data; `redacted` says it was removed or masked before the model saw it |
| `tellproof.guardrail(decision=None)` | the guardrail decision (`allow`, `block`, `flag`) for calls inside; set it later with `g.allow()`, `g.block()`, `g.flag()` |
| `tellproof.tool(name, high_impact=False, approved=False)` | a tool call; failures inside mark it failed; set `t.approved = True` (or `tellproof.approve()`) once approved |
| `tellproof.agent_run(name)` | one agent run; model changes are tracked per agent (`@tellproof.agent_run` names it after the function) |
| `tellproof.escalation()` | the request was handed to a person |
| `tellproof.llm_call(provider, model, operation="chat", region=None)` | a model call the SDK does not instrument; report usage with `c.usage(input_tokens, output_tokens)` |

Names and versions are capped at 128 characters; anything that is not a string or an integer is ignored.

Calls inside a `guardrail` scope are held until the scope ends, so a decision made after the call (an output check) still applies. A `block` with no call inside counts as one blocked call; pass `provider=` and `model=` to attribute it.

## Guardrail canaries

Show that your guardrail catches something, not only that it ran. The SDK passes synthetic, known-bad probes (prompt injection, jailbreak, made-up personal data, a documented example AWS key) to your own guardrail check and records only the kind and whether it was caught (EO-CAN-01):

```python
tellproof.schedule_canaries(lambda text: my_guardrail.scan(text).blocked, every=3600)  # now, then hourly
tellproof.run_canaries(check)          # once: {"prompt_injection": True, ...}; arun_canaries for an async check
tellproof.canary("toxicity", True)     # a canary you run with your own probe
```

The check returns `True`, `"block"` or `"flag"` when it would stop the text; anything else, or an exception, counts as missed. Pass probes to the guardrail only, never to the model. The probes never leave the process (`tellproof.PROBES`); with the SDK off the check is never called.

## Already on OpenTelemetry?

You can skip the SDK: point an OTLP/HTTP **JSON** exporter at the agent (`http://127.0.0.1:4318/v1/traces`) and set the `tellproof.*` attributes yourself (`spec/semconv-map.json`). Do not run both for the same calls, or they are counted twice.

## Development

```bash
python3 -m unittest discover -s sdk/python/tests
python3 sdk/python/tests/scenario.py   # rewrite sdk/testdata/python-export.jsonl
```
