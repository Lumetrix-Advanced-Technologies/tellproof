'use strict';
// Spans, scopes and the helpers (D-025, D-033).
//
// A span records only provider, model, region, token counts, operation,
// timing, error status and the helper attributes. Nothing here reads a
// message, a function argument, a return value or an error's text. Every SDK
// internal is wrapped so that it cannot throw into the app.

const { AsyncLocalStorage } = require('node:async_hooks');
const crypto = require('node:crypto');

const MAX_VALUE = 128; // helper strings are names and versions, never text
const DECISIONS = ['allow', 'block', 'flag'];
const MAX_DEFERRED = 1000; // LLM spans held by one guardrail scope

// Attribute names (spec/semconv-map.json v2).
const A = {
  PROVIDER: 'gen_ai.provider.name',
  OPERATION: 'gen_ai.operation.name',
  REQUEST_MODEL: 'gen_ai.request.model',
  RESPONSE_MODEL: 'gen_ai.response.model',
  INPUT_TOKENS: 'gen_ai.usage.input_tokens',
  OUTPUT_TOKENS: 'gen_ai.usage.output_tokens',
  REGION: 'cloud.region',
  AGENT_NAME: 'gen_ai.agent.name',
  TOOL_NAME: 'gen_ai.tool.name',
  PROMPT_NAME: 'plumbline.prompt.name',
  PROMPT_VERSION: 'plumbline.prompt.version',
  DATA_CLASS: 'plumbline.data_class',
  REDACTED: 'plumbline.redaction.applied',
  GUARDRAIL: 'plumbline.guardrail.decision',
  HIGH_IMPACT: 'plumbline.tool.high_impact',
  APPROVED: 'plumbline.tool.approved',
  ESCALATION: 'plumbline.escalation',
};

const KIND_INTERNAL = 1;
const KIND_CLIENT = 3;

function wallClockNs() {
  const { timeOrigin, now } = globalThis.performance;
  return BigInt(Math.round((timeOrigin + now.call(globalThis.performance)) * 1e3)) * 1000n;
}

class Client {
  constructor(exporter = null, { service = null, clock = null, ids = null } = {}) {
    this.exporter = exporter; // null: disabled
    this.service = service;
    this.clock = clock || wallClockNs;
    this.ids = ids || ((n) => crypto.randomBytes(n).toString('hex'));
    this.stats = { spans: 0, instrumentationErrors: 0 };
  }

  get enabled() {
    return this.exporter !== null;
  }

  emit(span) {
    if (this.exporter === null) return;
    this.stats.spans++;
    this.exporter.offer(span);
  }
}

let client = new Client(); // disabled until init()
const als = new AsyncLocalStorage();
let debug = false;

function currentClient() {
  return client;
}
function setClient(c) {
  client = c;
}
function setDebug(on) {
  debug = !!on;
}

function fail(where, err) {
  client.stats.instrumentationErrors++;
  if (debug) console.debug(`plumbline: internal error in ${where}:`, err && err.name);
}

// A helper's name or version: a short string, or null (ignored).
function clean(v) {
  if (typeof v === 'number' && Number.isInteger(v)) v = String(v);
  else if (typeof v === 'bigint') v = String(v);
  if (typeof v !== 'string' || v === '') return null;
  return v.slice(0, MAX_VALUE);
}

function count(v) {
  if (typeof v === 'number' && Number.isInteger(v) && v >= 0) return v;
  if (typeof v === 'bigint' && v >= 0n && v <= BigInt(Number.MAX_SAFE_INTEGER)) return Number(v);
  return null;
}

// Immutable context: the trace, the enclosing span and inherited attributes.
class Frame {
  constructor(traceId, spanId, attrs, guard) {
    this.traceId = traceId;
    this.spanId = spanId;
    this.attrs = attrs;
    this.guard = guard;
    Object.freeze(this);
  }
}

function currentFrame() {
  return als.getStore() || null;
}

function child(attrs = {}, spanId = null, guard = null) {
  const parent = currentFrame();
  if (parent === null) return new Frame(client.ids(16), spanId, { ...attrs }, guard);
  return new Frame(parent.traceId, spanId || parent.spanId, { ...parent.attrs, ...attrs }, guard || parent.guard);
}

// Work queued where emitting is unsafe (finalizers); run on the next record or flush.
const later = [];
function runLater() {
  while (later.length) {
    const fn = later.shift();
    try {
      fn();
    } catch (e) {
      fail('deferred', e);
    }
  }
}

const CURRENT = Symbol('current');

// Emit one span in the current context (or the given frame, for a call that
// started there and ends later, such as a stream). LLM spans inside a
// guardrail scope wait for its decision.
function record(name, kind, start, end, attrs, error, { llm = false, spanId = null, traceId = null, frame = CURRENT } = {}) {
  const c = client;
  if (!c.enabled) return;
  if (later.length) runLater();
  const parent = frame === CURRENT ? currentFrame() : frame;
  const span = {
    traceId: parent ? parent.traceId : traceId || c.ids(16),
    spanId: spanId || c.ids(8),
    parent: parent ? parent.spanId : null,
    name,
    kind,
    start,
    end: end > start ? end : start,
    attrs: { ...(parent ? parent.attrs : {}), ...attrs },
    error: !!error,
  };
  if (llm && parent && parent.guard && parent.guard._defer(span)) return;
  c.emit(span);
}

// Run fn inside a scope. enter() returns the frame to run in (or null);
// leave(failed) runs once fn has returned or its promise has settled. The
// app's return value and errors pass through unchanged.
function runScope(enter, leave, fn, arg) {
  if (typeof fn !== 'function') {
    fail('scope', new TypeError('not a function'));
    return undefined;
  }
  let frame = null;
  try {
    frame = client.enabled ? enter() : null;
  } catch (e) {
    fail('enter', e);
  }
  let done = false;
  const finish = (failed) => {
    if (done || frame === null) return;
    done = true;
    try {
      leave(failed);
    } catch (e) {
      fail('leave', e);
    }
  };
  let result;
  try {
    result = frame ? als.run(frame, fn, arg) : fn(arg);
  } catch (e) {
    finish(true);
    throw e;
  }
  if (result !== null && typeof result === 'object' && typeof result.then === 'function') {
    return result.then(
      (v) => {
        finish(false);
        return v;
      },
      (e) => {
        finish(true);
        throw e;
      },
    );
  }
  finish(false);
  return result;
}

// trace(fn): group everything inside into one trace (for example one web request).
function trace(fn) {
  return runScope(
    () => {
      const parent = currentFrame();
      return new Frame(client.ids(16), null, parent ? { ...parent.attrs } : {}, null);
    },
    () => {},
    fn,
  );
}

function attrScope(attrs, fn) {
  return runScope(() => child(attrs), () => {}, fn);
}

// prompt(name, version, fn): calls inside use this prompt (name and version, never its text).
function prompt(name, version, fn) {
  if (typeof version === 'function' && fn === undefined) [version, fn] = [undefined, version];
  const attrs = {};
  const n = clean(name);
  const v = clean(version);
  if (n) attrs[A.PROMPT_NAME] = n;
  if (v) attrs[A.PROMPT_VERSION] = v;
  return attrScope(attrs, fn);
}

// personalData({redacted}, fn): spans inside handle personal data; redacted
// says it was removed or masked before reaching the model.
function personalData(opts, fn) {
  if (typeof opts === 'function') [opts, fn] = [{}, opts];
  return attrScope({ [A.DATA_CLASS]: 'personal', [A.REDACTED]: !!(opts && opts.redacted) }, fn);
}

// A guardrail decision for the LLM calls inside: allow, block or flag. It may
// be given up front or set inside (g.block()); calls inside wait for it. A
// block with no call inside is recorded as one blocked call.
class Guardrail {
  constructor({ decision = null, provider = null, model = null } = {}) {
    this._decision = null;
    this._provider = clean(provider);
    this._model = clean(model);
    this._pending = [];
    this._calls = 0;
    this._start = 0n;
    this._outer = null;
    this._closed = false;
    this.decision = decision;
  }

  get decision() {
    return this._decision;
  }

  set decision(d) {
    if (d === null || d === undefined) this._decision = null;
    else if (DECISIONS.includes(d)) this._decision = d;
    else fail('guardrail', new TypeError('decision must be allow, block or flag'));
  }

  allow() {
    this.decision = 'allow';
  }
  block() {
    this.decision = 'block';
  }
  flag() {
    this.decision = 'flag';
  }

  _defer(span) {
    this._calls++;
    if (this._closed) {
      // A stream finishing after the scope ended: stamp the decision now.
      if (this._decision && !(A.GUARDRAIL in span.attrs)) span.attrs[A.GUARDRAIL] = this._decision;
      return this._outer ? this._outer._defer(span) : false;
    }
    if (this._pending.length >= MAX_DEFERRED) return false;
    this._pending.push(span);
    return true;
  }

  _enter() {
    const parent = currentFrame();
    this._outer = parent ? parent.guard : null;
    this._start = client.clock();
    this._frame = child({}, null, this);
    return this._frame;
  }

  _leave() {
    this._closed = true;
    const frame = this._frame;
    if (this._calls === 0 && this._decision === 'block') {
      const end = client.clock();
      this._pending.push({
        traceId: frame.traceId,
        spanId: client.ids(8),
        parent: frame.spanId,
        name: 'chat',
        kind: KIND_CLIENT,
        start: this._start,
        end: end > this._start ? end : this._start,
        attrs: { ...frame.attrs, [A.OPERATION]: 'chat', [A.PROVIDER]: this._provider || 'unknown', [A.REQUEST_MODEL]: this._model || 'unknown' },
        error: false,
      });
    }
    for (const span of this._pending) {
      if (this._decision && !(A.GUARDRAIL in span.attrs)) span.attrs[A.GUARDRAIL] = this._decision;
      if (!this._outer || !this._outer._defer(span)) client.emit(span);
    }
    this._pending = [];
  }
}

// guardrail(fn) | guardrail(decision, fn) | guardrail({decision, provider, model}, fn); fn gets the Guardrail.
function guardrail(opts, fn) {
  if (typeof opts === 'function') [opts, fn] = [{}, opts];
  if (typeof opts === 'string' || opts === null || opts === undefined) opts = { decision: opts ?? null };
  const g = new Guardrail(opts);
  return runScope(() => g._enter(), () => g._leave(), fn, g);
}

// A helper that is itself a span. The handle is passed to fn.
function spanScope(handle, name, kind, llm, attrsOf, inherit, fn) {
  let start = 0n;
  let spanId = null;
  let traceId = null;
  return runScope(
    () => {
      start = client.clock();
      spanId = client.ids(8);
      const frame = child(inherit(), spanId);
      traceId = frame.traceId; // a root span starts the trace its children join
      return frame;
    },
    (failed) => {
      // The span itself belongs to the enclosing context, which is current again here.
      record(typeof name === 'function' ? name() : name, kind, start, client.clock(), attrsOf(), handle.error || failed, {
        llm,
        spanId,
        traceId,
      });
    },
    fn,
    handle,
  );
}

// tool(name, {highImpact, approved}, fn): a tool call; fn gets {approved, highImpact, error}.
function tool(name, opts, fn) {
  if (typeof opts === 'function') [opts, fn] = [{}, opts];
  const t = { name: clean(name), highImpact: !!(opts && opts.highImpact), approved: !!(opts && opts.approved), error: false };
  return spanScope(
    t,
    'execute_tool',
    KIND_INTERNAL,
    false,
    () => {
      const a = { [A.OPERATION]: 'execute_tool', [A.HIGH_IMPACT]: !!t.highImpact, [A.APPROVED]: !!t.approved };
      if (t.name) a[A.TOOL_NAME] = t.name;
      return a;
    },
    () => ({}),
    fn,
  );
}

// agentRun(name, fn): one run of an agent; calls inside count toward this agent's model history.
function agentRun(name, fn) {
  const h = { name: clean(name), error: false };
  return spanScope(
    h,
    'invoke_agent',
    KIND_INTERNAL,
    false,
    () => (h.name ? { [A.OPERATION]: 'invoke_agent', [A.AGENT_NAME]: h.name } : { [A.OPERATION]: 'invoke_agent' }),
    () => (h.name ? { [A.AGENT_NAME]: h.name } : {}),
    fn,
  );
}

// llmCall({provider, model, operation, region}, fn): a model call the SDK does
// not instrument; fn gets a handle: call.usage(inputTokens, outputTokens).
function llmCall(opts, fn) {
  const o = opts || {};
  const c = {
    provider: clean(o.provider),
    model: clean(o.model),
    operation: clean(o.operation) || 'chat',
    region: clean(o.region),
    responseModel: null,
    inputTokens: null,
    outputTokens: null,
    error: false,
    usage(inputTokens, outputTokens) {
      const i = count(inputTokens);
      const out = count(outputTokens);
      if (i !== null) c.inputTokens = i;
      if (out !== null) c.outputTokens = out;
    },
  };
  return spanScope(
    c,
    () => c.operation,
    KIND_CLIENT,
    true,
    () => {
      const a = { [A.OPERATION]: c.operation, [A.PROVIDER]: c.provider || 'unknown', [A.REQUEST_MODEL]: c.model || 'unknown' };
      const rm = clean(c.responseModel);
      if (rm) a[A.RESPONSE_MODEL] = rm;
      if (c.region) a[A.REGION] = c.region;
      if (count(c.inputTokens) !== null) a[A.INPUT_TOKENS] = count(c.inputTokens);
      if (count(c.outputTokens) !== null) a[A.OUTPUT_TOKENS] = count(c.outputTokens);
      return a;
    },
    () => ({}),
    fn,
  );
}

// escalation(): record that this request was handed to a person.
function escalation() {
  try {
    const now = client.clock();
    record('plumbline.escalation', KIND_INTERNAL, now, now, { [A.ESCALATION]: true }, false);
  } catch (e) {
    fail('escalation', e);
  }
}

module.exports = {
  A,
  MAX_VALUE,
  KIND_CLIENT,
  KIND_INTERNAL,
  Client,
  Guardrail,
  currentClient,
  setClient,
  setDebug,
  currentFrame,
  record,
  later,
  runLater,
  clean,
  count,
  fail,
  trace,
  prompt,
  personalData,
  guardrail,
  tool,
  agentRun,
  llmCall,
  escalation,
};
