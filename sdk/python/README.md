# plumbline-sdk (Python SDK)

One line to send evidence telemetry from an AI product to the local Plumbline evidence agent.

```bash
pip install plumbline-sdk
```

```python
import plumbline
plumbline.init()
```

The SDK records one span per model call with provider, model, region, token counts, operation, timing and error status, and sends it as OTLP JSON to the agent on `http://127.0.0.1:4318` (or `PLUMBLINE_AGENT_URL`). The agent aggregates per hour and signs the aggregates: it keeps them in a local evidence log (`plumbline-agent local`) or sends them to Plumbline (`plumbline-agent connect`). **The SDK never reads prompts, completions, function arguments, return values or exception messages**, so there is no content to filter.

- No dependencies: standard library only, Python 3.10+.
- Never slows or breaks the app: spans go to a bounded background buffer; when it is full, spans are dropped and counted (`plumbline.stats()`). SDK errors are swallowed and counted.
- `PLUMBLINE_DISABLED=1` turns it off. `init()` is safe to call more than once.
- Proxies are never used: the agent is local.
- Works across threads, `asyncio` tasks and forked workers (gunicorn, uWSGI).

## Automatic instrumentation

`init()` instruments the client libraries that are installed. Nothing to configure:

| Library | Calls |
| --- | --- |
| `openai` | `chat.completions.create` / `parse` / `stream`, `responses.create` / `parse` / `stream`, `embeddings.create`, `completions.create`; sync, async, streaming. Azure OpenAI is reported as `azure.ai.openai`; other OpenAI-compatible endpoints by their host name. |
| `anthropic` | `messages.create` and `messages.stream` (and `beta.messages`); sync, async, streaming. `AnthropicBedrock` and `AnthropicVertex` are reported as `aws.bedrock` and `gcp.vertex_ai` with their region. |
| `boto3` (Bedrock Runtime) | `converse`, `converse_stream`, `invoke_model`, `invoke_model_with_response_stream` |

Per call it reads the request's model and stream flag, and from the response only the model, the usage block, the stop reason and Bedrock's token-count headers. Input tokens follow what the provider bills (Anthropic: uncached + cache writes + cache reads; Bedrock: `inputTokens`, as CloudWatch counts them), so Plumbline can reconcile traced against billed tokens.

Things to know:

- **Streams** are recorded when they end, are closed, or leave a `with` block, once, with the usage they reported. A stream dropped half-read is recorded on the next call or flush.
- **OpenAI streamed chat completions** report usage only if asked. `plumbline.init(stream_usage=True)` sets `stream_options={"include_usage": True}` when the app has not set it; the stream then ends with one extra chunk with no `choices`. Off by default, so the app's streams are unchanged.
- **Bedrock guardrails**: a Converse call with `guardrailConfig` is a guarded call, `block` when the stop reason is `guardrail_intervened`, else `allow`.
- `invoke_model_with_response_stream` reports its token counts only inside the streamed body, which the SDK never reads, so those calls are counted without tokens. `with_raw_response` calls are counted without tokens too.
- `plumbline.init(instrument=False)` turns automatic instrumentation off; the helpers still work.

## What the helpers record

Plumbline's evidence objectives need a few facts that no LLM client knows. Each helper is a context manager (`with` or `async with`) and a decorator.

| Helper | Records |
| --- | --- |
| `plumbline.prompt(name, version)` | the prompt (name and version, never its text) used by calls inside |
| `plumbline.personal_data(redacted=False)` | spans inside handle personal data; `redacted` says it was removed or masked before the model saw it |
| `plumbline.guardrail(decision=None)` | the guardrail decision (`allow`, `block`, `flag`) for calls inside; set it later with `g.allow()`, `g.block()`, `g.flag()` |
| `plumbline.tool(name, high_impact=False, approved=False)` | a tool call; failures inside mark it failed; set `t.approved = True` once approved |
| `plumbline.agent_run(name)` | one agent run; model changes are tracked per agent |
| `plumbline.escalation()` | the request was handed to a person |
| `plumbline.trace()` | groups everything inside into one trace (for example one web request) |
| `plumbline.llm_call(provider, model, operation="chat", region=None)` | a model call the SDK does not instrument; report usage with `c.usage(input_tokens, output_tokens)` |

```python
with plumbline.trace(), plumbline.prompt("claims-triage", "14"), plumbline.personal_data(redacted=True):
    with plumbline.guardrail() as g:
        if not input_policy_ok(request):
            g.block()          # recorded as one blocked call; the model was never called
            return refusal()
        with plumbline.llm_call("openai", "gpt-4.1-mini", region="us-east-1") as c:
            r = client.chat.completions.create(...)
            c.usage(r.usage.prompt_tokens, r.usage.completion_tokens)
        g.flag() if needs_review(r) else g.allow()

@plumbline.tool("issue_refund", high_impact=True)
def issue_refund(claim): ...
```

Names and versions are capped at 128 characters; anything that is not a string or an integer is ignored.

Calls inside a `guardrail` scope are held until the scope ends, so a decision made after the call (an output check) still applies. A `block` with no call inside counts as one blocked call; pass `provider=` and `model=` to attribute it.

## Already on OpenTelemetry?

You can skip the SDK: point an OTLP/HTTP **JSON** exporter at the agent (`http://127.0.0.1:4318/v1/traces`) and set the `plumbline.*` attributes yourself (`spec/semconv-map.json`). Do not run both for the same calls, or they are counted twice.

## Development

```bash
python3 -m unittest discover -s sdk/python/tests
python3 sdk/python/tests/scenario.py   # rewrite sdk/testdata/python-export.jsonl
```
