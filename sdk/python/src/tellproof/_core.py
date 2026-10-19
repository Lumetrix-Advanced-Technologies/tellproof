"""Spans, scopes and the helpers.

A span records only provider, model, region, token counts, operation, timing,
error status and the helper attributes (D-020, D-025). Nothing here ever
reads a message, a function argument, a return value or an exception text.
Every SDK internal is wrapped so that it cannot raise into the app.
"""

from __future__ import annotations

import asyncio
import collections
import contextvars
import functools
import inspect
import logging
import os
import time
from typing import Any, Callable

log = logging.getLogger("tellproof")

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
PROMPT_NAME = "tellproof.prompt.name"
PROMPT_VERSION = "tellproof.prompt.version"
DATA_CLASS = "tellproof.data_class"
REDACTED = "tellproof.redaction.applied"
GUARDRAIL = "tellproof.guardrail.decision"
HIGH_IMPACT = "tellproof.tool.high_impact"
APPROVED = "tellproof.tool.approved"
ESCALATION = "tellproof.escalation"


SEVERITY = {"allow": 0, "flag": 1, "block": 2}


class Stats:
    def __init__(self) -> None:
        self.spans = 0
        self.instrumentation_errors = 0
        self.unscoped = 0  # decisions and redactions outside any request, so not recorded


# What init() sets for every model call (D-069): agent, prompt and whether
# calls handle personal data. A scope or a span's own attribute wins.
_defaults: dict = {}


def configure(agent: Any = None, prompt: Any = None, prompt_version: Any = None, personal_data: bool = False) -> None:
    d = {}
    for key, v in ((AGENT_NAME, agent), (PROMPT_NAME, prompt), (PROMPT_VERSION, prompt_version)):
        c = clean(v)
        if c:
            d[key] = c
    if personal_data:
        d[DATA_CLASS] = "personal"
    global _defaults
    _defaults = d


MAX_CHECKS = 32  # registered guardrail checks; more means a check is decorated per call (a leak)


class _Check:
    """A function registered with @guardrail_check."""
    __slots__ = ("fn", "canaries", "is_async", "method", "__weakref__")

    def __init__(self, fn: Callable, canaries: bool) -> None:
        self.fn, self.canaries, self.is_async = fn, canaries, inspect.iscoroutinefunction(fn)
        try:
            params = list(inspect.signature(fn).parameters)
        except (TypeError, ValueError):
            params = []
        self.method = bool(params) and params[0] in ("self", "cls")  # canaries need the bound instance


_checks: list = []  # _Check
# Set by the canary module while automatic canaries run: ensure(check, target)
# starts them for a check from its first call, with the callable they use.
_ensure_canaries: Callable | None = None


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
_frame: contextvars.ContextVar[_Frame | None] = contextvars.ContextVar("tellproof_frame", default=None)


def current_client() -> Client:
    return _client


def set_client(c: Client) -> None:
    global _client
    _client = c


def _fail(where: str) -> None:
    _client.stats.instrumentation_errors += 1
    log.debug("tellproof: internal error in %s", where, exc_info=True)


def clean(v: Any) -> str | None:
    """A helper's name or version: a short string, or None (ignored)."""
    if isinstance(v, bool) or not isinstance(v, (str, int)):
        return None
    s = str(v)
    return s[:MAX_VALUE] if s else None


class Request:
    """What happened in one request (or trace): shared by every context the
    request runs in (threads, tasks), so a redaction in one counts for the
    calls in another. Never content: flags, a decision and a count."""
    __slots__ = ("personal", "redacted", "decision", "calls")

    def __init__(self) -> None:
        self.personal = False
        self.redacted = False
        self.decision: str | None = None  # the strictest guardrail decision so far, for later calls
        self.calls = 0


class _Frame:
    """Immutable context: the trace, the enclosing span, inherited attributes,
    the guardrail scope, the request and the tool call in progress."""
    __slots__ = ("trace_id", "span_id", "attrs", "guard", "req", "tool")

    def __init__(self, trace_id: str, span_id: str | None, attrs: dict, guard: Guardrail | None,
                 req: Request | None = None, tool: Any = None) -> None:
        self.trace_id, self.span_id, self.attrs, self.guard = trace_id, span_id, attrs, guard
        self.req, self.tool = req, tool


def _child(attrs: dict | None = None, span_id: str | None = None, guard: Guardrail | None = None,
           tool: Any = None) -> _Frame:
    parent = _frame.get()
    if parent is None:
        return _Frame(_client.ids(16), span_id, dict(attrs or {}), guard, None, tool)
    merged = dict(parent.attrs)
    merged.update(attrs or {})
    return _Frame(parent.trace_id, span_id or parent.span_id, merged, guard or parent.guard, parent.req,
                  tool or parent.tool)


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
    if llm:
        _model_call_facts(a, parent)
    span = {"trace_id": parent.trace_id if parent else (trace_id or c.ids(16)), "span_id": span_id or c.ids(8),
            "parent": parent.span_id if parent else None, "name": name, "kind": kind,
            "start": start, "end": max(end, start), "attrs": a, "error": bool(error)}
    if llm and parent is not None and parent.guard is not None and parent.guard._defer(span):
        return
    c.emit(span)


def _model_call_facts(a: dict, parent: _Frame | None) -> None:
    """init()'s defaults and the request's redaction, for a model call."""
    for k, v in _defaults.items():
        if k != DATA_CLASS and k not in a:
            a[k] = v
    req = parent.req if parent else None
    if req is not None:
        req.calls += 1
        if req.decision and GUARDRAIL not in a:
            a[GUARDRAIL] = req.decision
    if DATA_CLASS in a:
        return  # a personal_data() scope decided
    if (req is not None and req.personal) or DATA_CLASS in _defaults:
        a[DATA_CLASS] = "personal"
        a[REDACTED] = bool(req is not None and req.redacted)


def _blocked_span(frame: _Frame | None, start: int, provider: str | None = None, model: str | None = None) -> dict:
    """One blocked call: a guardrail blocked a request before any model call."""
    a = dict(frame.attrs) if frame else {}
    a.update({OPERATION: "chat", PROVIDER: provider or "unknown", REQUEST_MODEL: model or "unknown"})
    _model_call_facts(a, frame)
    a[GUARDRAIL] = "block"
    return {"trace_id": frame.trace_id if frame else _client.ids(16), "span_id": _client.ids(8),
            "parent": frame.span_id if frame else None, "name": "chat", "kind": 3,
            "start": start, "end": max(_client.clock(), start), "attrs": a, "error": False}


def decide(decision: str) -> None:
    """A guardrail decision for the current request's model calls, from a
    @guardrail_check or a detected guardrail (moderation, ApplyGuardrail).
    Several decisions in one request keep the strictest. Outside a request
    nothing is attributed, except a block, which is one blocked call."""
    if decision not in SEVERITY or not _client.enabled:
        return
    f = _frame.get()
    g = f.guard if f is not None else None
    if g is not None and not g._closed:
        g._escalate(decision)
    elif f is not None and f.req is not None:
        r = f.req  # applies to the request's later calls; a block with none is one blocked call at its end
        if r.decision is None or SEVERITY[decision] > SEVERITY[r.decision]:
            r.decision = decision
    elif decision == "block":
        _client.emit(_blocked_span(f, _client.clock()))
    else:
        _client.stats.unscoped += 1


def mark_redacted() -> None:
    """The current request's personal data was redacted (a @redactor ran)."""
    if not _client.enabled:
        return
    f = _frame.get()
    if f is not None and f.req is not None:
        f.req.personal = f.req.redacted = True
    else:
        _client.stats.unscoped += 1


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
    """One request, or one unit of work: a trace of its own, the request's
    redaction, and, when guardrail checks are registered, a guardrail scope
    whose strictest decision its model calls carry. `with tellproof.trace():`,
    or `@tellproof.trace` on a worker's handler. Web requests in Flask,
    FastAPI, Starlette, Django and Node.js HTTP servers get one automatically."""

    _guard: Guardrail | None = None
    _req: Request | None = None

    def __new__(cls, fn: Callable | None = None):
        if callable(fn):  # @tellproof.trace, without parentheses
            return super().__new__(cls)(fn)
        return super().__new__(cls)

    def __init__(self, fn: Callable | None = None) -> None:
        pass

    def _clone(self) -> trace:
        return trace()

    def _enter(self) -> None:
        if _client.enabled:
            parent = _frame.get()
            g = None
            if _checks:  # a check may decide after a call: hold the request's calls until it ends
                g = Guardrail()
                g._start = _client.clock()
            self._guard, self._req = g, Request()
            self._push(_Frame(_client.ids(16), None, dict(parent.attrs) if parent else {}, g, self._req))

    def _exit(self, error: bool) -> None:
        if self._guard is not None:
            self._guard._exit(error)
        elif self._req is not None and self._req.decision == "block" and self._req.calls == 0 and _client.enabled:
            _client.emit(_blocked_span(_frame.get(), _client.clock()))


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
            log.debug("tellproof: guardrail decision must be one of %s", DECISIONS)

    def _escalate(self, d: str) -> None:
        if self._decision is None or SEVERITY[d] > SEVERITY[self._decision]:
            self._decision = d

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
            self._pending.append(_blocked_span(frame, self._start, self._provider, self._model))
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
            frame = _child(self._inherit(), span_id=self._span_id, tool=self if isinstance(self, tool) else None)
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
    """A tool call: `with tellproof.tool("refund") as t:`, or a decorator,
    `@tellproof.tool` or `@tellproof.tool(high_impact=True)`, named after the
    function. high_impact marks actions with real-world effect (payments,
    account changes); approved records a human or policy approval and may be
    set inside (t.approved = True, or tellproof.approve()). An exception marks
    it failed."""

    def __new__(cls, name: Any = None, **kwargs: Any):
        if callable(name):  # @tellproof.tool, without parentheses
            t = super().__new__(cls)
            t.__init__(None)
            return t(name)
        return super().__new__(cls)

    def __init__(self, name: Any = None, *, high_impact: bool = False, approved: bool = False) -> None:
        super().__init__()
        self.name = clean(name)
        self.high_impact = bool(high_impact)
        self.approved = bool(approved)

    def _clone(self) -> tool:
        return tool(self.name, high_impact=self.high_impact, approved=self.approved)

    def __call__(self, fn: Callable) -> Callable:
        if self.name is None:  # @tellproof.tool(high_impact=True): named after the function
            self.name = clean(getattr(fn, "__name__", None))
        return super().__call__(fn)

    _name = "execute_tool"

    def _attrs(self) -> dict:
        a = {OPERATION: "execute_tool", HIGH_IMPACT: bool(self.high_impact), APPROVED: bool(self.approved)}
        if self.name:
            a[TOOL_NAME] = self.name
        return a


def approve() -> None:
    """The tool call in progress was approved by a person or a policy."""
    f = _frame.get()
    if f is not None and f.tool is not None:
        f.tool.approved = True


class agent_run(_SpanScope):
    """One run of an agent: `with tellproof.agent_run("triage"):`, or a
    decorator, `@tellproof.agent_run` or `@tellproof.agent_run("triage")`.
    Calls inside count toward this agent's model history (model transitions
    are tracked per agent)."""

    _name = "invoke_agent"

    def __new__(cls, name: Any = None):
        if callable(name):  # @tellproof.agent_run, without parentheses
            r = super().__new__(cls)
            r.__init__(None)
            return r(name)
        return super().__new__(cls)

    def __init__(self, name: Any = None) -> None:
        super().__init__()
        self.name = clean(name)

    def _clone(self) -> agent_run:
        return agent_run(self.name)

    def __call__(self, fn: Callable) -> Callable:
        if self.name is None:  # @tellproof.agent_run(): named after the function
            self.name = clean(getattr(fn, "__name__", None))
        return super().__call__(fn)

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
        record("tellproof.escalation", 1, now, now, {ESCALATION: True}, False)
    except Exception:
        _fail("escalation")


def _decision_of(result: Any) -> str:
    """A check's result as a decision: True or "block" blocks, "flag" flags,
    anything else allows. Only compared, never kept."""
    if result is True:
        return "block"
    if isinstance(result, str) and len(result) <= 5:
        r = result.lower()
        if r in ("block", "flag"):
            return r
    return "allow"


def _decorate(fn: Callable, after: Callable[[Any], None], before: Callable[[tuple], None] | None = None) -> Callable:
    """Wrap fn so before(args) runs first and after(result) once it has its
    result; the app's arguments, result and exceptions pass through
    unchanged. A sync function that returns an awaitable keeps returning it:
    a Future or Task gets a done callback, a coroutine is awaited inside one
    that returns the same value; any other awaitable records nothing."""
    def note(result: Any) -> None:
        try:
            after(result)
        except Exception:
            _fail("decorated")

    def first(args: tuple) -> None:
        if before is not None:
            try:
                before(args)
            except Exception:
                _fail("decorated")

    if inspect.iscoroutinefunction(fn):
        @functools.wraps(fn)
        async def awrapper(*args, **kwargs):
            first(args)
            result = await fn(*args, **kwargs)
            note(result)
            return result
        return awrapper

    @functools.wraps(fn)
    def wrapper(*args, **kwargs):
        first(args)
        result = fn(*args, **kwargs)
        if not inspect.isawaitable(result):
            note(result)
            return result
        if isinstance(result, asyncio.Future):
            result.add_done_callback(lambda f: None if f.cancelled() or f.exception() is not None else note(f.result()))
            return result
        if inspect.iscoroutine(result):
            async def awaited(coro=result):
                value = await coro
                note(value)
                return value
            return awaited()
        return result
    return wrapper


def guardrail_check(fn: Callable | None = None, *, canaries: bool = True):
    """Decorate the app's own guardrail check, a function of the text that
    returns True (or "block" or "flag") when it would stop it; a method is
    fine. Each time it runs, its decision counts for the current request's
    model calls, and, unless canaries=False (an output-only check, say), the
    SDK sends it the canary probes, from its first call and then hourly. Its
    argument and result are never recorded. Decorate it once, where it is
    defined: the SDK registers at most 32 checks."""
    if fn is None:
        return lambda f: guardrail_check(f, canaries=canaries)
    check = None
    try:
        check = next((c for c in _checks if c.fn is fn), None)
        if check is None:
            if len(_checks) < MAX_CHECKS:
                check = _Check(fn, bool(canaries))
                _checks.append(check)
            else:
                _fail("guardrail_check: more than 32 registered; decorate a check once, where it is defined")
    except Exception:
        _fail("guardrail_check")

    def before(args: tuple) -> None:
        ensure = _ensure_canaries
        if check is not None and check.canaries and ensure is not None:
            target = functools.partial(fn, args[0]) if check.method and args else fn
            ensure(check, target)

    return _decorate(fn, lambda result: decide(_decision_of(result)), before)


def redactor(fn: Callable) -> Callable:
    """Decorate the app's own redaction function: once it has returned, the
    current request's model calls count as handling personal data, redacted.
    Its arguments and result are never read."""
    return _decorate(fn, lambda result: mark_redacted())
