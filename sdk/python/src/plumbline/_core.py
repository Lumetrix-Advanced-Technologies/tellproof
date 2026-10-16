"""Spans, scopes and the helpers.

A span records only provider, model, region, token counts, operation, timing,
error status and the helper attributes (D-020, D-025). Nothing here ever
reads a message, a function argument, a return value or an exception text.
Every SDK internal is wrapped so that it cannot raise into the app.
"""

from __future__ import annotations

import collections
import contextvars
import functools
import inspect
import logging
import os
import time
from typing import Any, Callable

log = logging.getLogger("plumbline")

MAX_VALUE = 128  # helper strings are names and versions, never text
DECISIONS = ("allow", "block", "flag")
MAX_DEFERRED = 1000  # LLM spans held by one guardrail scope

# Attribute names (spec/semconv-map.json v2).
PROVIDER = "gen_ai.provider.name"
OPERATION = "gen_ai.operation.name"
REQUEST_MODEL = "gen_ai.request.model"
RESPONSE_MODEL = "gen_ai.response.model"
INPUT_TOKENS = "gen_ai.usage.input_tokens"
OUTPUT_TOKENS = "gen_ai.usage.output_tokens"
REGION = "cloud.region"
AGENT_NAME = "gen_ai.agent.name"
TOOL_NAME = "gen_ai.tool.name"
PROMPT_NAME = "plumbline.prompt.name"
PROMPT_VERSION = "plumbline.prompt.version"
DATA_CLASS = "plumbline.data_class"
REDACTED = "plumbline.redaction.applied"
GUARDRAIL = "plumbline.guardrail.decision"
HIGH_IMPACT = "plumbline.tool.high_impact"
APPROVED = "plumbline.tool.approved"
ESCALATION = "plumbline.escalation"


class Stats:
    def __init__(self) -> None:
        self.spans = 0
        self.instrumentation_errors = 0


class Client:
    """One configured SDK instance. Tests build their own; apps use init()."""

    def __init__(self, exporter=None, *, service: str | None = None,
                 clock: Callable[[], int] | None = None, ids: Callable[[int], str] | None = None) -> None:
        self.exporter = exporter  # None: disabled
        self.service = service
        self.clock = clock or time.time_ns
        self.ids = ids or (lambda n: os.urandom(n).hex())
        self.stats = Stats()

    @property
    def enabled(self) -> bool:
        return self.exporter is not None

    def emit(self, span: dict) -> None:
        if self.exporter is None:
            return
        self.stats.spans += 1
        self.exporter.offer(span)


_client: Client = Client()  # disabled until init()
_frame: contextvars.ContextVar[_Frame | None] = contextvars.ContextVar("plumbline_frame", default=None)


def current_client() -> Client:
    return _client


def set_client(c: Client) -> None:
    global _client
    _client = c


def _fail(where: str) -> None:
    _client.stats.instrumentation_errors += 1
    log.debug("plumbline: internal error in %s", where, exc_info=True)


def clean(v: Any) -> str | None:
    """A helper's name or version: a short string, or None (ignored)."""
    if isinstance(v, bool) or not isinstance(v, (str, int)):
        return None
    s = str(v)
    return s[:MAX_VALUE] if s else None


class _Frame:
    """Immutable context: the trace, the enclosing span and inherited attributes."""
    __slots__ = ("trace_id", "span_id", "attrs", "guard")

    def __init__(self, trace_id: str, span_id: str | None, attrs: dict, guard: Guardrail | None) -> None:
        self.trace_id, self.span_id, self.attrs, self.guard = trace_id, span_id, attrs, guard


def _child(attrs: dict | None = None, span_id: str | None = None, guard: Guardrail | None = None) -> _Frame:
    parent = _frame.get()
    if parent is None:
        return _Frame(_client.ids(16), span_id, dict(attrs or {}), guard)
    merged = dict(parent.attrs)
    merged.update(attrs or {})
    return _Frame(parent.trace_id, span_id or parent.span_id, merged, guard or parent.guard)


_CURRENT = object()
_later: collections.deque = collections.deque()  # work queued where taking locks is unsafe (finalizers)


def later(fn: Callable[[], None]) -> None:
    """Run fn on the next record() or flush; deque.append takes no lock."""
    _later.append(fn)


def run_later() -> None:
    while True:
        try:
            fn = _later.popleft()
        except IndexError:
            return
        try:
            fn()
        except Exception:
            _fail("deferred")


def current_frame() -> _Frame | None:
    return _frame.get()


def record(name: str, kind: int, start: int, end: int, attrs: dict, error: bool, *, llm: bool = False,
           span_id: str | None = None, trace_id: str | None = None, frame: Any = _CURRENT) -> None:
    """Emit one span in the current context (or the given frame, for a call
    that started there and ends later, such as a stream). LLM spans inside a
    guardrail scope wait for its decision."""
    c = _client
    if not c.enabled:
        return
    if _later:
        run_later()
    parent = _frame.get() if frame is _CURRENT else frame
    a = dict(parent.attrs) if parent else {}
    a.update(attrs)
    span = {"trace_id": parent.trace_id if parent else (trace_id or c.ids(16)), "span_id": span_id or c.ids(8),
            "parent": parent.span_id if parent else None, "name": name, "kind": kind,
            "start": start, "end": max(end, start), "attrs": a, "error": bool(error)}
    if llm and parent is not None and parent.guard is not None and parent.guard._defer(span):
        return
    c.emit(span)


class _Scope:
    """Base for helpers: a context manager (sync and async) and a decorator.
    A decorated function gets a fresh scope per call."""

    _token = None

    def _clone(self) -> _Scope:
        raise NotImplementedError

    def _enter(self) -> None:
        pass

    def _exit(self, error: bool) -> None:
        pass

    def __enter__(self):
        try:
            self._enter()
        except Exception:
            _fail(type(self).__name__)
        return self

    def __exit__(self, exc_type, exc, tb) -> bool:
        try:
            self._exit(exc_type is not None)
        except Exception:
            _fail(type(self).__name__)
        finally:
            if self._token is not None:
                try:
                    _frame.reset(self._token)
                except ValueError:  # exited in another context; leave it
                    pass
                self._token = None
        return False  # the app's exception, if any, propagates unchanged

    async def __aenter__(self):
        return self.__enter__()

    async def __aexit__(self, exc_type, exc, tb) -> bool:
        return self.__exit__(exc_type, exc, tb)

    def _push(self, frame: _Frame) -> None:
        self._token = _frame.set(frame)

    def __call__(self, fn: Callable) -> Callable:
        if inspect.iscoroutinefunction(fn):
            @functools.wraps(fn)
            async def awrapper(*args, **kwargs):
                async with self._clone():
                    return await fn(*args, **kwargs)
            return awrapper

        @functools.wraps(fn)
        def wrapper(*args, **kwargs):
            with self._clone():
                return fn(*args, **kwargs)
        return wrapper


class trace(_Scope):
    """Group everything inside into one trace (for example one web request)."""

    def _clone(self) -> trace:
        return trace()

    def _enter(self) -> None:
        if _client.enabled:
            parent = _frame.get()
            self._push(_Frame(_client.ids(16), None, dict(parent.attrs) if parent else {}, None))


class _AttrScope(_Scope):
    def __init__(self, attrs: dict) -> None:
        self._attrs = attrs

    def _clone(self) -> _AttrScope:
        return _AttrScope(dict(self._attrs))

    def _enter(self) -> None:
        if _client.enabled:
            self._push(_child(self._attrs))


def prompt(name: Any, version: Any = None) -> _Scope:
    """Calls inside use this prompt (name and version, never its text)."""
    attrs = {}
    n, v = clean(name), clean(version)
    if n:
        attrs[PROMPT_NAME] = n
    if v:
        attrs[PROMPT_VERSION] = v
    return _AttrScope(attrs)


def personal_data(redacted: bool = False) -> _Scope:
    """Spans inside handle personal data; redacted says it was removed or
    masked before reaching the model."""
    return _AttrScope({DATA_CLASS: "personal", REDACTED: bool(redacted)})


class Guardrail(_Scope):
    """A guardrail decision for the LLM calls inside: allow, block or flag.

    The decision may be given up front or set later (g.block()); LLM calls
    inside wait for it and carry it when the scope ends. A block with no call
    inside (the request never reached the model) is recorded as one blocked
    call, attributed to provider and model when given.
    """

    def __init__(self, decision: str | None = None, *, provider: str | None = None, model: str | None = None) -> None:
        self._decision = None
        self._provider, self._model = clean(provider), clean(model)
        self._pending: list[dict] = []
        self._calls = 0
        self._start = 0
        self._outer: Guardrail | None = None
        self._closed = False
        self.decision = decision

    @property
    def decision(self) -> str | None:
        return self._decision

    @decision.setter
    def decision(self, d: str | None) -> None:
        if d is None or d in DECISIONS:
            self._decision = d
        else:
            _client.stats.instrumentation_errors += 1
            log.debug("plumbline: guardrail decision must be one of %s", DECISIONS)

    def allow(self) -> None:
        self.decision = "allow"

    def block(self) -> None:
        self.decision = "block"

    def flag(self) -> None:
        self.decision = "flag"

    def _clone(self) -> Guardrail:
        return Guardrail(self._decision, provider=self._provider, model=self._model)

    def _defer(self, span: dict) -> bool:
        """Hold an LLM span for the decision. After the scope has ended (a
        stream finishing late) the decision is stamped at once."""
        self._calls += 1
        if self._closed:
            if self._decision and GUARDRAIL not in span["attrs"]:
                span["attrs"][GUARDRAIL] = self._decision
            return self._outer._defer(span) if self._outer is not None else False
        if len(self._pending) >= MAX_DEFERRED:
            return False
        self._pending.append(span)
        return True

    def _enter(self) -> None:
        if _client.enabled:
            parent = _frame.get()
            self._outer = parent.guard if parent else None
            self._start = _client.clock()
            self._push(_child(guard=self))

    def _exit(self, error: bool) -> None:
        if not _client.enabled:
            return
        self._closed = True
        frame = _frame.get()
        outer = self._outer
        if self._calls == 0 and self._decision == "block":
            attrs = {OPERATION: "chat", PROVIDER: self._provider or "unknown", REQUEST_MODEL: self._model or "unknown"}
            span = {"trace_id": frame.trace_id if frame else _client.ids(16), "span_id": _client.ids(8),
                    "parent": frame.span_id if frame else None, "name": "chat", "kind": 3,
                    "start": self._start, "end": max(_client.clock(), self._start),
                    "attrs": {**(frame.attrs if frame else {}), **attrs}, "error": False}
            self._pending.append(span)
        for span in self._pending:
            if self._decision and GUARDRAIL not in span["attrs"]:
                span["attrs"][GUARDRAIL] = self._decision
            if outer is None or not outer._defer(span):
                _client.emit(span)
        self._pending = []


def guardrail(decision: str | None = None, *, provider: str | None = None, model: str | None = None) -> Guardrail:
    return Guardrail(decision, provider=provider, model=model)


class _SpanScope(_Scope):
    """A helper that is itself a span."""

    _name = "span"
    _kind = 1
    _llm = False

    def __init__(self) -> None:
        self.error = False
        self._start = 0
        self._span_id: str | None = None
        self._trace_id: str | None = None

    def _attrs(self) -> dict:
        return {}

    def _inherit(self) -> dict:
        return {}

    def _enter(self) -> None:
        if _client.enabled:
            self._start = _client.clock()
            self._span_id = _client.ids(8)
            # Nested spans are children of this one.
            frame = _child(self._inherit(), span_id=self._span_id)
            self._trace_id = frame.trace_id  # a root span starts the trace its children join
            self._push(frame)

    def _exit(self, error: bool) -> None:
        if not _client.enabled or self._span_id is None:
            return
        end = _client.clock()
        if self._token is not None:  # the span belongs to the enclosing context
            _frame.reset(self._token)
            self._token = None
        record(self._name, self._kind, self._start, end, self._attrs(), self.error or error,
               llm=self._llm, span_id=self._span_id, trace_id=self._trace_id)


class tool(_SpanScope):
    """A tool call. high_impact marks actions with real-world effect (payments,
    account changes); approved records a human or policy approval and may be
    set inside the block (t.approved = True)."""

    def __init__(self, name: Any, *, high_impact: bool = False, approved: bool = False) -> None:
        super().__init__()
        self.name = clean(name)
        self.high_impact = bool(high_impact)
        self.approved = bool(approved)

    def _clone(self) -> tool:
        return tool(self.name, high_impact=self.high_impact, approved=self.approved)

    _name = "execute_tool"

    def _attrs(self) -> dict:
        a = {OPERATION: "execute_tool", HIGH_IMPACT: bool(self.high_impact), APPROVED: bool(self.approved)}
        if self.name:
            a[TOOL_NAME] = self.name
        return a


class agent_run(_SpanScope):
    """One run of an agent. Calls inside count toward this agent's model
    history (model transitions are tracked per agent)."""

    _name = "invoke_agent"

    def __init__(self, name: Any) -> None:
        super().__init__()
        self.name = clean(name)

    def _clone(self) -> agent_run:
        return agent_run(self.name)

    def _inherit(self) -> dict:
        return {AGENT_NAME: self.name} if self.name else {}

    def _attrs(self) -> dict:
        a = {OPERATION: "invoke_agent"}
        if self.name:
            a[AGENT_NAME] = self.name
        return a


class llm_call(_SpanScope):
    """A model call the SDK does not instrument automatically. Report usage
    with call.usage(input_tokens, output_tokens); nothing else is recorded."""

    _kind = 3
    _llm = True

    def __init__(self, provider: Any, model: Any, *, operation: str = "chat", region: Any = None) -> None:
        super().__init__()
        self.provider, self.model, self.region = clean(provider), clean(model), clean(region)
        self.operation = clean(operation) or "chat"
        self.response_model: str | None = None
        self.input_tokens: int | None = None
        self.output_tokens: int | None = None

    def _clone(self) -> llm_call:
        return llm_call(self.provider, self.model, operation=self.operation, region=self.region)

    def usage(self, input_tokens: Any = None, output_tokens: Any = None) -> None:
        self.input_tokens = _count(input_tokens, self.input_tokens)
        self.output_tokens = _count(output_tokens, self.output_tokens)

    @property
    def _name(self) -> str:  # type: ignore[override]
        return self.operation

    def _attrs(self) -> dict:
        a = {OPERATION: self.operation, PROVIDER: self.provider or "unknown", REQUEST_MODEL: self.model or "unknown"}
        rm = clean(self.response_model)
        if rm:
            a[RESPONSE_MODEL] = rm
        if self.region:
            a[REGION] = self.region
        if self.input_tokens is not None:
            a[INPUT_TOKENS] = self.input_tokens
        if self.output_tokens is not None:
            a[OUTPUT_TOKENS] = self.output_tokens
        return a


def _count(v: Any, old: int | None) -> int | None:
    if isinstance(v, bool) or not isinstance(v, int) or v < 0:
        return old
    return v


def escalation() -> None:
    """Record that this request was handed to a person."""
    try:
        now = _client.clock()
        record("plumbline.escalation", 1, now, now, {ESCALATION: True}, False)
    except Exception:
        _fail("escalation")
