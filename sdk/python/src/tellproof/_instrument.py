"""Automatic instrumentation of the LLM clients an app already uses.

init() patches, in the modules the app imports (never importing one itself): openai (chat completions,
responses, embeddings, legacy completions; sync, async, streaming), anthropic
(messages and beta messages; create and stream; sync and async) and botocore
for Bedrock Runtime (Converse, ConverseStream, InvokeModel,
InvokeModelWithResponseStream).

Each call becomes one LLM span. Only these are read: the request's model
name and stream flag, whether a Bedrock guardrail was configured, and from the
response its model, usage block, stop reason and HTTP token-count headers.
Messages, inputs, outputs, tool arguments and error messages are never read.
"""

from __future__ import annotations

import contextvars
import functools
import importlib
import importlib.util
import inspect
import sys
import threading
from typing import Any, Callable
from urllib.parse import urlsplit

from . import _core

_MARK = "__tellproof_wrapped__"


def _available(name: str) -> bool:
    if name in sys.modules:
        return sys.modules[name] is not None
    try:
        return importlib.util.find_spec(name) is not None
    except (ImportError, ValueError):
        return False


def _int(v: Any) -> int | None:
    if isinstance(v, bool):
        return None
    if isinstance(v, int) and v >= 0:
        return v
    if isinstance(v, str) and v.isdigit():
        return int(v)
    return None


def _get(obj: Any, name: str) -> Any:
    """An attribute or mapping key, or None. Only ever called with the names
    of usage and metadata fields."""
    if obj is None:
        return None
    if isinstance(obj, dict):
        return obj.get(name)
    try:
        return getattr(obj, name, None)
    except Exception:
        return None


class Call:
    """One model call in flight. Finishes exactly once."""

    def __init__(self, provider: str, model: Any, operation: str, region: Any = None) -> None:
        c = _core.current_client()
        self.frame = _core.current_frame()
        self.start = c.clock()
        self.span_id = c.ids(8)
        self.trace_id = None if self.frame else c.ids(16)
        self.provider = _core.clean(provider) or "unknown"
        self.model = _core.clean(model) or "unknown"
        self.operation = operation
        self.region = _core.clean(region)
        self.response_model: str | None = None
        self.input_tokens: int | None = None
        self.output_tokens: int | None = None
        self.guardrail: str | None = None
        self.prompt: tuple[str, str | None] | None = None  # a stored prompt's ID and version (OpenAI Responses)
        self.error = False
        self.done = False

    def finish(self, error: bool = False) -> None:
        if self.done:
            return
        self.done = True
        try:
            a = {_core.OPERATION: self.operation, _core.PROVIDER: self.provider, _core.REQUEST_MODEL: self.model}
            rm = _core.clean(self.response_model)
            if rm:
                a[_core.RESPONSE_MODEL] = rm
            if self.region:
                a[_core.REGION] = self.region
            if self.input_tokens is not None:
                a[_core.INPUT_TOKENS] = self.input_tokens
            if self.output_tokens is not None:
                a[_core.OUTPUT_TOKENS] = self.output_tokens
            if self.guardrail:
                a[_core.GUARDRAIL] = self.guardrail
            if self.prompt:
                a[_core.PROMPT_NAME] = self.prompt[0]
                if self.prompt[1]:
                    a[_core.PROMPT_VERSION] = self.prompt[1]
            _core.record(self.operation, 3, self.start, _core.current_client().clock(), a, error or self.error,
                         llm=True, span_id=self.span_id, trace_id=self.trace_id, frame=self.frame)
        except Exception:
            _core._fail("finish")


def _safe(fn: Callable, *args) -> None:
    try:
        fn(*args)
    except Exception:
        _core._fail(getattr(fn, "__name__", "observer"))


class _Proxy:
    """Stands in for a client library's stream: same iteration, context
    manager and attributes (isinstance still works), and finishes the call
    when the stream ends, is closed, or is dropped unfinished."""

    def __init__(self, inner: Any, call: Call, observe: Callable[[Call, Any], None]) -> None:
        object.__setattr__(self, "_p_inner", inner)
        object.__setattr__(self, "_p_call", call)
        object.__setattr__(self, "_p_observe", observe)
        object.__setattr__(self, "_p_it", None)

    @property  # type: ignore[misc]
    def __class__(self):
        return type(self._p_inner)

    def __getattr__(self, name: str) -> Any:
        return getattr(self._p_inner, name)

    def __setattr__(self, name: str, value: Any) -> None:
        setattr(self._p_inner, name, value)

    def __repr__(self) -> str:
        return repr(self._p_inner)

    def __del__(self) -> None:
        call = self._p_call
        if not call.done:
            _core.later(call.finish)  # no locks in a finalizer


class SyncStream(_Proxy):
    def __iter__(self):
        return self

    def __next__(self):
        if self._p_it is None:
            object.__setattr__(self, "_p_it", iter(self._p_inner))
        try:
            item = next(self._p_it)
        except StopIteration:
            self._p_call.finish()
            raise
        except BaseException:
            self._p_call.finish(error=True)
            raise
        _safe(self._p_observe, self._p_call, item)
        return item

    def __enter__(self):
        enter = getattr(self._p_inner, "__enter__", None)
        if enter is not None:
            enter()
        return self

    def __exit__(self, *exc):
        try:
            ex = getattr(self._p_inner, "__exit__", None)
            return ex(*exc) if ex is not None else None
        finally:
            self._p_call.finish()

    def close(self, *a, **k):
        try:
            close = getattr(self._p_inner, "close", None)
            return close(*a, **k) if close is not None else None
        finally:
            self._p_call.finish()


class AsyncStream(_Proxy):
    def __aiter__(self):
        return self

    async def __anext__(self):
        if self._p_it is None:
            object.__setattr__(self, "_p_it", self._p_inner.__aiter__())
        try:
            item = await self._p_it.__anext__()
        except StopAsyncIteration:
            self._p_call.finish()
            raise
        except BaseException:
            self._p_call.finish(error=True)
            raise
        _safe(self._p_observe, self._p_call, item)
        return item

    async def __aenter__(self):
        enter = getattr(self._p_inner, "__aenter__", None)
        if enter is not None:
            await enter()
        return self

    async def __aexit__(self, *exc):
        try:
            ex = getattr(self._p_inner, "__aexit__", None)
            return (await ex(*exc)) if ex is not None else None
        finally:
            self._p_call.finish()

    async def close(self, *a, **k):
        try:
            close = getattr(self._p_inner, "close", None)
            if close is not None:
                r = close(*a, **k)
                return (await r) if inspect.isawaitable(r) else r
        finally:
            self._p_call.finish()

    async def aclose(self):
        return await self.close()


def _patch(owner: Any, name: str, make: Callable[[Callable], Callable]) -> None:
    orig = getattr(owner, name, None)
    if orig is None or getattr(orig, _MARK, None) is not None:
        return
    wrapped = make(orig)
    setattr(wrapped, _MARK, orig)
    setattr(owner, name, wrapped)


def wrap_call(orig: Callable, start: Callable[..., Call | None], done: Callable[[Call, Any], Any]) -> Callable:
    """Wrap a client method (sync or async). start(self, args, kwargs) makes
    the Call (or None to pass through); done(call, result) reads the result
    and returns it, or a stream proxy that finishes the call later."""
    if inspect.iscoroutinefunction(orig):
        @functools.wraps(orig)
        async def awrapper(self, *args, **kwargs):
            call = _begin(start, self, args, kwargs)
            if call is None:
                return await orig(self, *args, **kwargs)
            try:
                result = await orig(self, *args, **kwargs)
            except BaseException as e:
                _safe(_refused, call, e)
                call.finish(error=True)
                raise
            return _end(done, call, result)
        return awrapper

    @functools.wraps(orig)
    def wrapper(self, *args, **kwargs):
        call = _begin(start, self, args, kwargs)
        if call is None:
            return orig(self, *args, **kwargs)
        try:
            result = orig(self, *args, **kwargs)
        except BaseException as e:
            _safe(_refused, call, e)
            call.finish(error=True)
            raise
        return _end(done, call, result)
    return wrapper


def _refused(call: Call, err: BaseException) -> None:
    """Azure OpenAI refuses a prompt its content filter stops with the error
    code content_filter: a guardrail block. Only the code is read."""
    if call.provider == "azure.ai.openai" and getattr(err, "code", None) == "content_filter":
        call.guardrail = "block"


def _begin(start, self, args, kwargs) -> Call | None:
    if not _core.current_client().enabled:
        return None
    try:
        return start(self, args, kwargs)
    except Exception:
        _core._fail("start")
        return None


def _end(done, call: Call, result: Any) -> Any:
    try:
        return done(call, result)
    except Exception:
        _core._fail("done")
        call.finish()
        return result


# --- OpenAI -----------------------------------------------------------------

_stream_usage = False


def _openai_provider(resource: Any) -> tuple[str, str | None]:
    client = _get(resource, "_client")
    if "Azure" in type(client).__name__:
        return "azure.ai.openai", None
    host = urlsplit(str(_get(client, "base_url") or "")).hostname or ""
    if host == "" or host == "api.openai.com" or host.endswith(".openai.com"):
        return "openai", None
    if host.endswith(".openai.azure.com"):
        return "azure.ai.openai", None
    return host, None  # an OpenAI-compatible endpoint: name it by its host


def _openai_usage(call: Call, usage: Any) -> None:
    if usage is None:
        return
    # Chat and legacy completions, embeddings: prompt/completion; responses: input/output.
    # OpenAI's input counts already include cached tokens (D-028).
    i = _int(_get(usage, "prompt_tokens"))
    if i is None:
        i = _int(_get(usage, "input_tokens"))
    o = _int(_get(usage, "completion_tokens"))
    if o is None:
        o = _int(_get(usage, "output_tokens"))
    if i is not None:
        call.input_tokens = i
    if o is not None:
        call.output_tokens = o


def _given(kwargs: dict, name: str) -> bool:
    """openai's own helpers pass omitted arguments as sentinel objects."""
    return name in kwargs and type(kwargs[name]).__name__ not in ("NotGiven", "Omit")


def _openai_start(operation: str, chat: bool = False, responses: bool = False):
    def start(self, args, kwargs):
        provider, region = _openai_provider(self)
        if chat and _stream_usage and kwargs.get("stream") is True and not _given(kwargs, "stream_options"):
            kwargs["stream_options"] = {"include_usage": True}
        call = Call(provider, kwargs.get("model"), operation, region)
        if responses and _given(kwargs, "prompt"):
            # A stored prompt is named by reference: its ID and version, never its text or variables.
            ref = kwargs["prompt"]
            pid, ver = _core.clean(_get(ref, "id")), _core.clean(_get(ref, "version"))
            if pid:
                call.prompt = (pid, ver)
        return call
    return start


def _azure_filter(call: Call, result: Any) -> None:
    """Azure OpenAI's content filter ran when a response carries its results:
    the call was guarded, and blocked when a choice ended on content_filter.
    Only the presence of the results and the finish reasons are read."""
    if call.provider != "azure.ai.openai":
        return
    choices = _get(result, "choices") or []
    if not isinstance(choices, (list, tuple)):
        return
    ran = _get(result, "prompt_filter_results") is not None or any(_get(c, "content_filter_results") is not None for c in choices)
    if any(_get(c, "finish_reason") == "content_filter" for c in choices):
        call.guardrail = "block"
    elif ran and call.guardrail is None:
        call.guardrail = "allow"


def _openai_observe(call: Call, item: Any) -> None:
    kind = _get(item, "type")
    if isinstance(kind, str) and kind.startswith("response."):  # Responses API events
        if kind in ("response.completed", "response.incomplete", "response.failed"):
            r = _get(item, "response")
            call.response_model = _get(r, "model") or call.response_model
            _openai_usage(call, _get(r, "usage"))
            if kind == "response.failed":
                call.error = True
        return
    m = _get(item, "model")  # chat completion chunk
    if isinstance(m, str):
        call.response_model = m
    _openai_usage(call, _get(item, "usage"))
    _azure_filter(call, item)


def _openai_done(call: Call, result: Any) -> Any:
    if hasattr(result, "__aiter__") and not hasattr(result, "usage"):
        return AsyncStream(result, call, _openai_observe)
    if hasattr(result, "__iter__") and hasattr(result, "__next__") and not hasattr(result, "usage"):
        return SyncStream(result, call, _openai_observe)
    m = _get(result, "model")
    if isinstance(m, str):
        call.response_model = m
    _openai_usage(call, _get(result, "usage"))
    _azure_filter(call, result)
    if _get(result, "status") == "failed":
        call.error = True
    call.finish()
    return result


def _moderation_done(result: Any) -> None:
    """An OpenAI moderation check is a guardrail: flagged or not, for the
    request's model calls. Only the flagged booleans are read."""
    results = _get(result, "results") or []
    if isinstance(results, (list, tuple)) and results:
        _core.decide("flag" if any(_get(r, "flagged") is True for r in results) else "allow")


def _observed(after: Callable[[Any], None]):
    """Wrap a method (sync or async) that is not a model call, to observe its result."""
    def make(orig: Callable) -> Callable:
        if inspect.iscoroutinefunction(orig):
            @functools.wraps(orig)
            async def awrapper(self, *args, **kwargs):
                result = await orig(self, *args, **kwargs)
                if _core.current_client().enabled:
                    _safe(after, result)
                return result
            return awrapper

        @functools.wraps(orig)
        def wrapper(self, *args, **kwargs):
            result = orig(self, *args, **kwargs)
            if _core.current_client().enabled:
                _safe(after, result)
            return result
        return wrapper
    return make


def _openai_classes(classes: tuple[str, ...], methods: tuple[str, ...], op: str, chat: bool, resp: bool) -> Callable[[Any], bool]:
    def run(mod: Any) -> bool:
        done = False
        for cls_name in classes:
            cls = getattr(mod, cls_name, None)
            for m in methods if cls is not None else ():
                _patch(cls, m, lambda orig, op=op, chat=chat, resp=resp: wrap_call(orig, _openai_start(op, chat, resp), _openai_done))
                done = True
        return done
    return run


def _openai_moderations(mod: Any) -> bool:
    for cls_name in ("Moderations", "AsyncModerations"):
        cls = getattr(mod, cls_name, None)
        if cls is not None:
            _patch(cls, "create", _observed(_moderation_done))
    return False  # a guardrail, not a model call: does not make openai "instrumented"


_OPENAI = [
    ("openai.resources.chat.completions", _openai_classes(("Completions", "AsyncCompletions"), ("create", "parse"), "chat", True, False)),
    ("openai.resources.responses", _openai_classes(("Responses", "AsyncResponses"), ("create", "parse"), "chat", False, True)),
    ("openai.resources.embeddings", _openai_classes(("Embeddings", "AsyncEmbeddings"), ("create",), "embeddings", False, False)),
    ("openai.resources.completions", _openai_classes(("Completions", "AsyncCompletions"), ("create",), "text_completion", False, False)),
    ("openai.resources.moderations", _openai_moderations),
]


def _import_and_run(targets: list[tuple[str, Callable[[Any], bool]]]) -> bool:
    done = False
    for mod_name, run in targets:
        try:
            mod = importlib.import_module(mod_name)
        except Exception:
            continue
        done = run(mod) or done
    return done


def instrument_openai() -> bool:
    """Import and patch now (explicit use); init() patches as the app imports."""
    return _available("openai") and _import_and_run(_OPENAI)


# --- Anthropic --------------------------------------------------------------

def _anthropic_provider(resource: Any) -> tuple[str, Any]:
    client = _get(resource, "_client")
    name = type(client).__name__
    if "Bedrock" in name:
        return "aws.bedrock", _get(client, "aws_region")
    if "Vertex" in name:
        return "gcp.vertex_ai", _get(client, "region")
    return "anthropic", None


_ANTHROPIC_INPUT = ("input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens")


def _anthropic_usage(call: Call, usage: Any) -> None:
    """Billed input = uncached + cache writes + cache reads (D-028). Stream
    deltas are cumulative and may carry only some fields, so each field keeps
    its latest reported value."""
    if usage is None:
        return
    parts = call.__dict__.setdefault("anthropic_input", {})
    for k in _ANTHROPIC_INPUT:
        v = _int(_get(usage, k))
        if v is not None:
            parts[k] = v
    if parts:
        call.input_tokens = sum(parts.values())
    o = _int(_get(usage, "output_tokens"))
    if o is not None:
        call.output_tokens = o


def _anthropic_message(call: Call, msg: Any) -> None:
    m = _get(msg, "model")
    if isinstance(m, str):
        call.response_model = m
    _anthropic_usage(call, _get(msg, "usage"))


def _anthropic_observe(call: Call, ev: Any) -> None:
    kind = _get(ev, "type")
    if kind == "message_start":
        _anthropic_message(call, _get(ev, "message"))
    elif kind == "message_delta":
        _anthropic_usage(call, _get(ev, "usage"))
    elif kind == "error":
        call.error = True


def _anthropic_start(self, args, kwargs) -> Call:
    provider, region = _anthropic_provider(self)
    return Call(provider, kwargs.get("model"), "chat", region)


def _anthropic_done(call: Call, result: Any) -> Any:
    if hasattr(result, "__aiter__") and not hasattr(result, "usage"):
        return AsyncStream(result, call, _anthropic_observe)
    if hasattr(result, "__iter__") and hasattr(result, "__next__") and not hasattr(result, "usage"):
        return SyncStream(result, call, _anthropic_observe)
    _anthropic_message(call, result)
    call.finish()
    return result


def _snapshot_usage(call: Call, stream: Any) -> None:
    get = getattr(stream, "_current_snapshot_or_none", None)
    snap = get() if callable(get) else None
    if snap is not None:
        _anthropic_message(call, snap)


class _StreamManager(_Proxy):
    """messages.stream(...) returns a manager; the call ends when it exits."""

    def __del__(self) -> None:
        if self._p_it is not None and not self._p_call.done:  # entered but never exited
            _core.later(self._p_call.finish)

    def __enter__(self):
        try:
            stream = self._p_inner.__enter__()
        except BaseException:
            self._p_call.finish(error=True)
            raise
        object.__setattr__(self, "_p_it", stream)
        return stream

    def __exit__(self, exc_type, exc, tb):
        try:
            return self._p_inner.__exit__(exc_type, exc, tb)
        finally:
            _safe(_snapshot_usage, self._p_call, self._p_it)
            self._p_call.finish()

    async def __aenter__(self):
        try:
            stream = await self._p_inner.__aenter__()
        except BaseException:
            self._p_call.finish(error=True)
            raise
        object.__setattr__(self, "_p_it", stream)
        return stream

    async def __aexit__(self, exc_type, exc, tb):
        try:
            return await self._p_inner.__aexit__(exc_type, exc, tb)
        finally:
            _safe(_snapshot_usage, self._p_call, self._p_it)
            self._p_call.finish()


def _wrap_stream_method(orig: Callable) -> Callable:
    @functools.wraps(orig)
    def wrapper(self, *args, **kwargs):
        call = _begin(_anthropic_start, self, args, kwargs)
        mgr = orig(self, *args, **kwargs)
        if call is None:
            return mgr
        return _StreamManager(mgr, call, _anthropic_observe)
    return wrapper


def _anthropic_messages(mod: Any) -> bool:
    done = False
    for cls_name in ("Messages", "AsyncMessages"):
        cls = getattr(mod, cls_name, None)
        if cls is None:
            continue
        _patch(cls, "create", lambda orig: wrap_call(orig, _anthropic_start, _anthropic_done))
        _patch(cls, "stream", _wrap_stream_method)
        done = True
    return done


_ANTHROPIC = [("anthropic.resources.messages", _anthropic_messages), ("anthropic.resources.beta.messages", _anthropic_messages)]


def instrument_anthropic() -> bool:
    return _available("anthropic") and _import_and_run(_ANTHROPIC)


# --- Amazon Bedrock (botocore) ----------------------------------------------

_BEDROCK_OPS = {"Converse", "ConverseStream", "InvokeModel", "InvokeModelWithResponseStream"}


def _bedrock_converse_usage(call: Call, usage: Any) -> None:
    # inputTokens excludes cache reads and writes, as CloudWatch's
    # InputTokenCount does, so traced and billed tokens compare (D-028).
    i, o = _int(_get(usage, "inputTokens")), _int(_get(usage, "outputTokens"))
    if i is not None:
        call.input_tokens = i
    if o is not None:
        call.output_tokens = o


def _bedrock_stop(call: Call, reason: Any, guarded: bool) -> None:
    if guarded:
        call.guardrail = "block" if reason == "guardrail_intervened" else "allow"


def _bedrock_observe_for(guarded: bool):
    def observe(call: Call, ev: Any) -> None:
        if not isinstance(ev, dict):
            return
        if "metadata" in ev:
            _bedrock_converse_usage(call, _get(ev["metadata"], "usage"))
        if "messageStop" in ev:
            _bedrock_stop(call, _get(ev["messageStop"], "stopReason"), guarded)
        if any(k.endswith("Exception") for k in ev):
            call.error = True
    return observe


def _apply_guardrail_done(resp: Any) -> None:
    """Bedrock's ApplyGuardrail is a guardrail check: blocked when it
    intervened. Only the action is read."""
    action = _get(resp, "action")
    if isinstance(action, str):
        _core.decide("block" if action == "GUARDRAIL_INTERVENED" else "allow")


def _make_api_call(orig: Callable) -> Callable:
    @functools.wraps(orig)
    def wrapper(self, operation_name, api_params):
        if operation_name == "ApplyGuardrail" and _core.current_client().enabled:
            resp = orig(self, operation_name, api_params)
            try:
                if self.meta.service_model.service_name == "bedrock-runtime":
                    _apply_guardrail_done(resp)
            except Exception:
                _core._fail("apply guardrail")
            return resp
        call = None
        guarded = False
        if operation_name in _BEDROCK_OPS and _core.current_client().enabled:
            try:
                meta = self.meta
                if meta.service_model.service_name == "bedrock-runtime":
                    p = api_params if isinstance(api_params, dict) else {}
                    guarded = "guardrailConfig" in p or "guardrailIdentifier" in p
                    call = Call("aws.bedrock", p.get("modelId"), "chat", meta.region_name)
            except Exception:
                _core._fail("bedrock start")
                call = None
        if call is None:
            return orig(self, operation_name, api_params)
        try:
            resp = orig(self, operation_name, api_params)
        except BaseException:
            call.finish(error=True)
            raise
        try:
            if operation_name == "Converse":
                _bedrock_converse_usage(call, _get(resp, "usage"))
                _bedrock_stop(call, _get(resp, "stopReason"), guarded)
            elif operation_name == "ConverseStream" and isinstance(resp, dict) and resp.get("stream") is not None:
                resp["stream"] = SyncStream(resp["stream"], call, _bedrock_observe_for(guarded))
                return resp
            elif operation_name == "InvokeModel":
                # The body holds the completion and is never read; Bedrock
                # reports the counts in response headers.
                h = _get(_get(resp, "ResponseMetadata"), "HTTPHeaders") or {}
                i = _int(_get(h, "x-amzn-bedrock-input-token-count"))
                o = _int(_get(h, "x-amzn-bedrock-output-token-count"))
                if i is not None:
                    call.input_tokens = i
                if o is not None:
                    call.output_tokens = o
            elif operation_name == "InvokeModelWithResponseStream" and isinstance(resp, dict) and resp.get("body") is not None:
                # Token counts arrive only inside the streamed body; the call is recorded without them.
                resp["body"] = SyncStream(resp["body"], call, lambda c, ev: None)
                return resp
        except Exception:
            _core._fail("bedrock done")
        call.finish()
        return resp
    return wrapper


def _botocore_client(mod: Any) -> bool:
    base = getattr(mod, "BaseClient", None)
    if base is None:
        return False
    _patch(base, "_make_api_call", _make_api_call)
    return True


_BOTOCORE = [("botocore.client", _botocore_client)]


def instrument_botocore() -> bool:
    return _available("botocore") and _import_and_run(_BOTOCORE)


# --- Web frameworks: one trace (request) per request ---------------------------

def _scoped(orig: Callable) -> Callable:
    """Run a sync request handler inside its own trace."""
    @functools.wraps(orig)
    def wrapper(*args, **kwargs):
        if not _core.current_client().enabled:
            return orig(*args, **kwargs)
        with _core.trace():
            return orig(*args, **kwargs)
    return wrapper


def _ascoped(orig: Callable) -> Callable:
    """Run an async request handler inside its own trace."""
    @functools.wraps(orig)
    async def wrapper(*args, **kwargs):
        if not _core.current_client().enabled:
            return await orig(*args, **kwargs)
        async with _core.trace():
            return await orig(*args, **kwargs)
    return wrapper


def _asgi_scoped(orig: Callable) -> Callable:
    """An ASGI app's __call__: HTTP requests only (not lifespan or websockets)."""
    @functools.wraps(orig)
    async def call(self, scope, receive, send):
        if not _core.current_client().enabled or not isinstance(scope, dict) or scope.get("type") != "http":
            return await orig(self, scope, receive, send)
        async with _core.trace():
            return await orig(self, scope, receive, send)
    return call


class _ScopedBody:
    """A WSGI response body that keeps its request's scope open until the
    server closes it, so a streamed response's checks and model calls count
    for their request. Iteration runs in the request's context."""

    def __init__(self, body: Any, ctx: contextvars.Context, scope: Any) -> None:
        self._body, self._ctx, self._scope = body, ctx, scope
        self._it = ctx.run(iter, body)
        self._ended = self._closed = False

    def __iter__(self):
        return self

    def __next__(self):
        try:
            return self._ctx.run(next, self._it)
        except StopIteration:
            self._end()  # sent in full: the request is over, even if the server never calls close()
            raise

    def _end(self) -> None:
        if not self._ended:
            self._ended = True
            self._ctx.run(self._scope.__exit__, None, None, None)

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        try:
            c = getattr(self._body, "close", None)
            if c is not None:
                self._ctx.run(c)
        finally:
            self._end()


def _wsgi_scoped(orig: Callable) -> Callable:
    """A WSGI app (Flask's wsgi_app): one scope per request, open until the
    server closes the response body. A file sent with wsgi.file_wrapper is
    returned as it is (the server may send it with sendfile)."""
    @functools.wraps(orig)
    def wsgi_app(self, environ, start_response):
        if not _core.current_client().enabled:
            return orig(self, environ, start_response)
        ctx = contextvars.copy_context()
        scope = _core.trace()
        ctx.run(scope.__enter__)
        try:
            body = ctx.run(orig, self, environ, start_response)
        except BaseException as e:
            ctx.run(scope.__exit__, type(e), e, e.__traceback__)
            raise
        fw = environ.get("wsgi.file_wrapper") if isinstance(environ, dict) else None
        if isinstance(body, (list, tuple)) or (isinstance(fw, type) and isinstance(body, fw)):
            ctx.run(scope.__exit__, None, None, None)
            return body
        return _ScopedBody(body, ctx, scope)
    return wsgi_app


def _class_patch(cls_name: str, patches: tuple[tuple[str, Callable], ...]) -> Callable[[Any], bool]:
    def run(mod: Any) -> bool:
        cls = getattr(mod, cls_name, None)
        if cls is None:
            return False
        for name, make in patches:
            _patch(cls, name, make)
        return True
    return run


_FLASK = [("flask.app", _class_patch("Flask", (("wsgi_app", _wsgi_scoped),)))]
_STARLETTE = [("starlette.applications", _class_patch("Starlette", (("__call__", _asgi_scoped),)))]  # FastAPI apps are Starlette apps
_DJANGO = [("django.core.handlers.base", _class_patch("BaseHandler", (("get_response", _scoped), ("get_response_async", _ascoped))))]


def instrument_flask() -> bool:
    return _available("flask") and _import_and_run(_FLASK)


def instrument_starlette() -> bool:
    return _available("starlette") and _import_and_run(_STARLETTE)


def instrument_django() -> bool:
    return _available("django") and _import_and_run(_DJANGO)


# --- Patching as the app imports ------------------------------------------------
#
# init() never imports a library or a module of one: an installed library the
# app does not use (boto3 beside an OpenAI app, Django in a Flask app) or a part
# it does not use (openai's responses or embeddings) would cost megabytes. A
# module already imported is patched at once; any other is patched right after
# the app imports it, by a finder at the front of sys.meta_path that wraps the
# loader's exec_module for those few module names and answers None to all else.

_LIBS: list[tuple[str, list[tuple[str, Callable[[Any], bool]]], bool]] = [
    ("openai", _OPENAI, False), ("anthropic", _ANTHROPIC, False), ("bedrock", _BOTOCORE, False),
    ("flask", _FLASK, True), ("starlette", _STARLETTE, True), ("django", _DJANGO, True),
]
_hooks: dict[str, tuple[str, Callable[[Any], bool]]] = {}  # module name -> (library, patcher)
_patched: list[str] = []  # libraries patched in a module the app has loaded, in _LIBS order
_finding = threading.local()


def _ran(lib: str, run: Callable[[Any], bool], mod: Any) -> None:
    try:
        if run(mod) and lib not in _patched:
            _patched.append(lib)
            _patched.sort(key=[n for n, _, _ in _LIBS].index)
    except Exception:
        _core._fail("instrument " + lib)


class _PostImport:
    """Patches a hooked module right after it is executed."""

    def find_spec(self, name: str, path: Any = None, target: Any = None) -> Any:
        hook = _hooks.get(name)
        if hook is None or getattr(_finding, "busy", False):
            return None
        _finding.busy = True
        try:
            spec = None
            for finder in sys.meta_path:
                find = getattr(finder, "find_spec", None) if finder is not self else None
                if find is not None:
                    spec = find(name, path, target)
                    if spec is not None:
                        break
            loader = getattr(spec, "loader", None)
            orig = getattr(loader, "exec_module", None)
            if orig is None or "exec_module" in getattr(loader, "__dict__", {}):
                return spec

            def exec_module(module: Any) -> None:
                try:
                    orig(module)
                finally:
                    try:
                        del loader.exec_module  # the loader is as it was
                    except AttributeError:
                        pass
                if _hooks.pop(name, None) is not None:
                    _ran(hook[0], hook[1], module)

            loader.exec_module = exec_module
            return spec
        except Exception:
            return None  # never in the way of an import
        finally:
            _finding.busy = False


_finder = _PostImport()


def patched() -> list[str]:
    """The libraries patched so far, as the app loads them."""
    return list(_patched)


def install(stream_usage: bool = False, requests: bool = False) -> list[str]:
    """Patch the client libraries (and with requests the web frameworks) the
    app has imported, and arrange to patch the rest when it imports them;
    returns the names patched now."""
    global _stream_usage
    _stream_usage = bool(stream_usage)
    _hooks.clear()
    del _patched[:]
    for lib, targets, is_framework in _LIBS:
        if is_framework and not requests:
            continue
        for mod_name, run in targets:
            mod = sys.modules.get(mod_name)
            if mod is not None:
                _ran(lib, run, mod)
            else:
                _hooks[mod_name] = (lib, run)
    if _hooks and _finder not in sys.meta_path:
        sys.meta_path.insert(0, _finder)
    return patched()
