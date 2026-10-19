"""Tellproof SDK: one line to send evidence telemetry to the local Tellproof agent.

    import tellproof
    tellproof.init()

    @tellproof.guardrail_check      # optional: your guardrail's decisions, and canaries
    def check(text): ...
    @tellproof.redactor             # optional: your redaction ran for this request
    def redact(messages): ...

Spans go as OTLP JSON to the agent (http://127.0.0.1:4318, or
TELLPROOF_AGENT_URL). They carry provider, model, region, token counts,
operation, timing, error status and the helper attributes below; never
prompts, completions, function arguments or exception messages. The SDK has no
dependencies, never blocks a request (a full buffer drops spans and counts
them) and never raises into the app. TELLPROOF_DISABLED=1 turns it off.
"""

from __future__ import annotations

import atexit
import os
import sys
import threading

from . import _canary, _core, _instrument, _otlp
from ._canary import PROBES, arun_canaries, canary, run_canaries, schedule_canaries
from ._core import (Guardrail, agent_run, approve, escalation, guardrail, guardrail_check, llm_call,
                    personal_data, prompt, redactor, tool, trace)
from ._export import Exporter, Transport, http_transport

__version__ = "0.3.1"
__all__ = ["init", "shutdown", "flush", "stats", "is_enabled", "guardrail_check", "redactor", "tool", "approve",
           "escalation", "trace", "prompt", "guardrail", "Guardrail", "personal_data", "agent_run", "llm_call",
           "canary", "run_canaries", "arun_canaries", "schedule_canaries", "PROBES", "__version__"]

DEFAULT_AGENT_URL = "http://127.0.0.1:4318"
_lock = threading.Lock()
_atexit = False
_instrumented: list[str] = []


def _env(name: str) -> str:
    # TELLPROOF_<name>, else PLUMBLINE_<name> from before the rename (D-056).
    return os.environ.get("TELLPROOF_" + name) or os.environ.get("PLUMBLINE_" + name) or ""


def _resource(service: str | None, environment: str | None) -> dict:
    name = service or _env("SERVICE") or os.environ.get("OTEL_SERVICE_NAME")
    if not name:
        prog = os.path.basename(sys.argv[0]) if sys.argv and sys.argv[0] else "python"
        name = f"unknown_service:{prog}"
    r = {"service.name": name[:_core.MAX_VALUE], "telemetry.sdk.name": "tellproof",
         "telemetry.sdk.language": "python", "telemetry.sdk.version": __version__}
    env = environment or _env("ENVIRONMENT")
    if env:
        r["deployment.environment.name"] = env[:_core.MAX_VALUE]
    return r


def _flag(name: str) -> bool:
    return _env(name).strip().lower() in ("1", "true", "yes")


def init(agent_url: str | None = None, service: str | None = None, *, environment: str | None = None,
         agent: str | None = None, prompt: str | None = None, prompt_version: str | None = None,
         personal_data: bool | None = None, requests: bool = True, canaries: bool | float = True,
         instrument: bool = True, stream_usage: bool = False, transport: Transport | None = None) -> bool:
    """Start the SDK and instrument the installed LLM clients (OpenAI,
    Anthropic, Bedrock via boto3) and web frameworks (Flask, FastAPI and
    Starlette, Django: one trace per request). Safe to call more than once;
    returns whether it is on.

    agent, prompt, prompt_version: names every model call carries unless a
    scope says otherwise (or TELLPROOF_AGENT_NAME, TELLPROOF_PROMPT,
    TELLPROOF_PROMPT_VERSION). personal_data: every model call handles
    personal data (TELLPROOF_PERSONAL_DATA=1); a @redactor that ran in the
    request makes it redacted. requests=False leaves web frameworks alone.
    canaries: run the canaries hourly through each @guardrail_check (a
    number is the interval in seconds; False turns them off).

    stream_usage: ask OpenAI for usage on streamed chat completions
    (stream_options.include_usage) when the app has not set stream_options.
    The stream then ends with one extra chunk that has no choices; leave it
    off if the app indexes choices[0] on every chunk.
    """
    global _atexit, _instrumented
    with _lock:
        if _core.current_client().enabled:
            return True
        if _env("DISABLED").strip().lower() in ("1", "true", "yes"):
            return False
        try:
            url = (agent_url or _env("AGENT_URL") or DEFAULT_AGENT_URL).rstrip("/")
            resource = _resource(service, environment)
            exporter = Exporter(transport or http_transport(url + "/v1/traces"),
                                lambda spans: _otlp.encode(spans, resource, __version__))
            _core.configure(agent or _env("AGENT_NAME"), prompt or _env("PROMPT"),
                            prompt_version or _env("PROMPT_VERSION"),
                            _flag("PERSONAL_DATA") if personal_data is None else bool(personal_data))
            _core.set_client(_core.Client(exporter, service=resource["service.name"]))
            _instrumented = _instrument.install(stream_usage, requests=requests) if instrument else []
            if canaries:
                every = 3600.0 if canaries is True else float(canaries)
                _canary.start_auto(every)
            if not _atexit:
                atexit.register(shutdown)
                _atexit = True
        except Exception:
            _core.log.debug("tellproof: init failed; the SDK stays off", exc_info=True)
            return False
        return True


def flush(timeout: float = 2.0) -> bool:
    """Send buffered spans now; True when everything was handed to the agent."""
    c = _core.current_client()
    if c.exporter is None:
        return True
    try:
        _core.run_later()
        return c.exporter.flush(timeout)
    except Exception:
        return False


def shutdown(timeout: float = 2.0) -> bool:
    """Flush and stop. Registered at exit; later spans are not recorded."""
    with _lock:
        c = _core.current_client()
        _core.set_client(_core.Client())
        try:
            _canary.stop_auto()
        except Exception:
            pass
    if c.exporter is None:
        return True
    try:
        return c.exporter.shutdown(timeout)
    except Exception:
        return False


def is_enabled() -> bool:
    return _core.current_client().enabled


def stats() -> dict:
    """Counters for health checks: spans recorded, exported, dropped, failed exports,
    swallowed instrumentation errors, and guardrail decisions or redactions made
    outside any request (unscoped: wrap that work in tellproof.trace())."""
    c = _core.current_client()
    e = c.exporter
    return {"enabled": c.enabled, "spans": c.stats.spans, "instrumentation_errors": c.stats.instrumentation_errors,
            "exported": e.exported if e else 0, "dropped": e.dropped if e else 0,
            "export_errors": e.export_errors if e else 0, "unscoped": c.stats.unscoped,
            "instrumented": list(_instrumented) if c.enabled else []}
