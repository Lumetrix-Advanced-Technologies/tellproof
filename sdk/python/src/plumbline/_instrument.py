"""Automatic instrumentation of the LLM clients an app already uses.

init() patches, when the library is installed: openai (chat completions,
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

import functools
import importlib
import importlib.util
import inspect
import sys
from typing import Any, Callable
from urllib.parse import urlsplit

from . import _core

_MARK = "__plumbline_wrapped__"


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
            except BaseException:
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
        except BaseException:
            call.finish(error=True)
            raise
        return _end(done, call, result)
    return wrapper


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


def _openai_start(operation: str, chat: bool = False):
    def start(self, args, kwargs):
        provider, region = _openai_provider(self)
        if chat and _stream_usage and kwargs.get("stream") is True and not _given(kwargs, "stream_options"):
            kwargs["stream_options"] = {"include_usage": True}
        return Call(provider, kwargs.get("model"), operation, region)
    return start


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


def _openai_done(call: Call, result: Any) -> Any:
    if hasattr(result, "__aiter__") and not hasattr(result, "usage"):
        return AsyncStream(result, call, _openai_observe)
    if hasattr(result, "__iter__") and hasattr(result, "__next__") and not hasattr(result, "usage"):
        return SyncStream(result, call, _openai_observe)
    m = _get(result, "model")
    if isinstance(m, str):
        call.response_model = m
    _openai_usage(call, _get(result, "usage"))
    if _get(result, "status") == "failed":
        call.error = True
    call.finish()
    return result


def instrument_openai() -> bool:
    if not _available("openai"):
        return False
    targets = [
        ("openai.resources.chat.completions", ("Completions", "AsyncCompletions"), ("create", "parse"), "chat", True),
        ("openai.resources.responses", ("Responses", "AsyncResponses"), ("create", "parse"), "chat", False),
        ("openai.resources.embeddings", ("Embeddings", "AsyncEmbeddings"), ("create",), "embeddings", False),
        ("openai.resources.completions", ("Completions", "AsyncCompletions"), ("create",), "text_completion", False),
    ]
    done = False
    for mod_name, classes, methods, op, chat in targets:
        try:
            mod = importlib.import_module(mod_name)
        except Exception:
            continue
        for cls_name in classes:
            cls = getattr(mod, cls_name, None)
            for m in methods if cls is not None else ():
                _patch(cls, m, lambda orig, op=op, chat=chat: wrap_call(orig, _openai_start(op, chat), _openai_done))
                done = True
    return done


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


def instrument_anthropic() -> bool:
    if not _available("anthropic"):
        return False
    done = False
    for mod_name in ("anthropic.resources.messages", "anthropic.resources.beta.messages"):
        try:
            mod = importlib.import_module(mod_name)
        except Exception:
            continue
        for cls_name in ("Messages", "AsyncMessages"):
            cls = getattr(mod, cls_name, None)
            if cls is None:
                continue
            _patch(cls, "create", lambda orig: wrap_call(orig, _anthropic_start, _anthropic_done))
            _patch(cls, "stream", _wrap_stream_method)
            done = True
    return done


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


def _make_api_call(orig: Callable) -> Callable:
    @functools.wraps(orig)
    def wrapper(self, operation_name, api_params):
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


def instrument_botocore() -> bool:
    if not _available("botocore"):
        return False
    try:
        client = importlib.import_module("botocore.client")
    except Exception:
        return False
    base = getattr(client, "BaseClient", None)
    if base is None:
        return False
    _patch(base, "_make_api_call", _make_api_call)
    return True


def install(stream_usage: bool = False) -> list[str]:
    """Patch every installed client library; returns the names instrumented."""
    global _stream_usage
    _stream_usage = bool(stream_usage)
    done = []
    for name, fn in (("openai", instrument_openai), ("anthropic", instrument_anthropic), ("bedrock", instrument_botocore)):
        try:
            if fn():
                done.append(name)
        except Exception:
            _core._fail("instrument " + name)
    return done
