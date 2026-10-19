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
const SEVERITY = { allow: 0, flag: 1, block: 2 };
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
  PROMPT_NAME: 'tellproof.prompt.name',
  PROMPT_VERSION: 'tellproof.prompt.version',
  DATA_CLASS: 'tellproof.data_class',
  REDACTED: 'tellproof.redaction.applied',
  GUARDRAIL: 'tellproof.guardrail.decision',
  HIGH_IMPACT: 'tellproof.tool.high_impact',
  APPROVED: 'tellproof.tool.approved',
  ESCALATION: 'tellproof.escalation',
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
    this.stats = { spans: 0, instrumentationErrors: 0, unscoped: 0 }; // unscoped: decisions and redactions outside any request
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
  if (debug) console.debug(`tellproof: internal error in ${where}:`, err && err.name);
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

// What init() sets for every model call (D-069): agent, prompt and whether
// calls handle personal data. A scope or a span's own attribute wins.
let defaults = {};
function configure({ agent = null, prompt = null, promptVersion = null, personalData = false } = {}) {
  const d = {};
  for (const [key, v] of [[A.AGENT_NAME, agent], [A.PROMPT_NAME, prompt], [A.PROMPT_VERSION, promptVersion]]) {
    const c = clean(v);
    if (c) d[key] = c;
  }
  if (personalData) d[A.DATA_CLASS] = 'personal';
  defaults = d;
}

// Guardrail checks registered with guardrailCheck(): { fn, canaries }. More
// than MAX_CHECKS means a check is wrapped per call (a leak): not registered.
const MAX_CHECKS = 32;
const checks = [];
// Set by the canary module while automatic canaries run: ensureCanaries(check,
// target) starts them for a check from its first call, with the callable they use.
const hooks = { ensureCanaries: null };

// What happened in one request (or trace): shared by every async context the
// request runs in. Never content: flags, a decision and a count.
class Request {
  constructor() {
    this.personal = false;
    this.redacted = false;
    this.decision = null; // the strictest guardrail decision so far, for later calls
    this.calls = 0;
  }
}

// Immutable context: the trace, the enclosing span, inherited attributes,
// the guardrail scope, the request and the tool call in progress.
class Frame {
  constructor(traceId, spanId, attrs, guard, req = null, tool = null) {
    this.traceId = traceId;
    this.spanId = spanId;
    this.attrs = attrs;
    this.guard = guard;
    this.req = req;
    this.tool = tool;
    Object.freeze(this);
  }
}

function currentFrame() {
  return als.getStore() || null;
}

function child(attrs = {}, spanId = null, guard = null, tool = null) {
  const parent = currentFrame();
  if (parent === null) return new Frame(client.ids(16), spanId, { ...attrs }, guard, null, tool);
  return new Frame(parent.traceId, spanId || parent.spanId, { ...parent.attrs, ...attrs }, guard || parent.guard, parent.req, tool || parent.tool);
}

// init()'s defaults and the request's decision and redaction, for a model call.
function modelCallFacts(a, parent) {
  for (const [k, v] of Object.entries(defaults)) if (k !== A.DATA_CLASS && !(k in a)) a[k] = v;
  const req = parent ? parent.req : null;
  if (req) {
    req.calls++;
    if (req.decision && !(A.GUARDRAIL in a)) a[A.GUARDRAIL] = req.decision;
  }
  if (A.DATA_CLASS in a) return; // a personalData() scope decided
  if ((req && req.personal) || A.DATA_CLASS in defaults) {
    a[A.DATA_CLASS] = 'personal';
    a[A.REDACTED] = !!(req && req.redacted);
  }
}

// One blocked call: a guardrail blocked a request before any model call.
function blockedSpan(frame, start, provider = null, model = null) {
  const end = client.clock();
  const attrs = { ...(frame ? frame.attrs : {}), [A.OPERATION]: 'chat', [A.PROVIDER]: provider || 'unknown', [A.REQUEST_MODEL]: model || 'unknown' };
  modelCallFacts(attrs, frame);
  attrs[A.GUARDRAIL] = 'block';
  return {
    traceId: frame ? frame.traceId : client.ids(16),
    spanId: client.ids(8),
    parent: frame ? frame.spanId : null,
    name: 'chat',
    kind: KIND_CLIENT,
    start,
    end: end > start ? end : start,
    attrs,
    error: false,
  };
}

function blockedCall() {
  client.emit(blockedSpan(currentFrame(), client.clock()));
}

// A guardrail decision for the current request's model calls, from a
// guardrailCheck() or a detected guardrail (moderation, ApplyGuardrail).
// Several decisions in one request keep the strictest. Outside a request
// nothing is attributed, except a block, which is one blocked call.
function decide(d) {
  if (!(d in SEVERITY) || !client.enabled) return;
  const f = currentFrame();
  const g = f ? f.guard : null;
  if (g && !g._closed) g._escalate(d);
  else if (f && f.req) {
    if (f.req.decision === null || SEVERITY[d] > SEVERITY[f.req.decision]) f.req.decision = d;
  } else if (d === 'block') blockedCall();
  else client.stats.unscoped++;
}

// The current request's personal data was redacted (a redactor() ran).
function markRedacted() {
  if (!client.enabled) return;
  const f = currentFrame();
  if (f && f.req) {
    f.req.personal = true;
    f.req.redacted = true;
  } else client.stats.unscoped++;
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
  if (llm) modelCallFacts(span.attrs, parent);
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

// One request, or one unit of work: a trace of its own, the request's
// redaction and decision, and, when guardrail checks are registered, a
// guardrail scope that holds its calls for a decision made after them.
function requestScope() {
  const parent = currentFrame();
  const req = new Request();
  let g = null;
  if (checks.length) {
    g = new Guardrail();
    g._start = client.clock();
  }
  const frame = new Frame(client.ids(16), null, parent ? { ...parent.attrs } : {}, g, req);
  if (g) g._frame = frame;
  let left = false;
  return {
    frame,
    leave() {
      if (left) return;
      left = true;
      try {
        if (g) g._leave();
        else if (req.decision === 'block' && req.calls === 0 && client.enabled) als.run(frame, blockedCall);
      } catch (e) {
        fail('request', e);
      }
    },
  };
}

function runIn(frame, fn) {
  return als.run(frame, fn);
}

// trace(fn): group everything inside into one trace and request (a job, a
// queue message). Web requests to Node.js HTTP servers get one automatically.
function trace(fn) {
  let scope = null;
  return runScope(
    () => {
      scope = requestScope();
      return scope.frame;
    },
    () => scope && scope.leave(),
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

  _escalate(d) {
    if (this._decision === null || SEVERITY[d] > SEVERITY[this._decision]) this._decision = d;
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
    if (this._calls === 0 && this._decision === 'block') this._pending.push(blockedSpan(frame, this._start, this._provider, this._model));
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
function spanScope(handle, name, kind, llm, attrsOf, inherit, fn, isTool = false) {
  let start = 0n;
  let spanId = null;
  let traceId = null;
  return runScope(
    () => {
      start = client.clock();
      spanId = client.ids(8);
      const frame = child(inherit(), spanId, null, isTool ? handle : null);
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

// A function that runs before(thisArg) and fn, sync or async, and then
// after(result); the app's result, promise object and errors pass through
// unchanged (a promise's result is observed on a side branch).
function decorate(fn, after, before = null) {
  if (typeof fn !== 'function') {
    fail('decorate', new TypeError('not a function'));
    return fn;
  }
  const note = (v) => {
    try {
      after(v);
    } catch (e) {
      fail('decorated', e);
    }
  };
  const wrapped = function (...args) {
    if (before) {
      try {
        before(this);
      } catch (e) {
        fail('decorated', e);
      }
    }
    const r = fn.apply(this, args);
    if (r !== null && typeof r === 'object' && typeof r.then === 'function') {
      try {
        r.then(note, () => {});
      } catch (e) {
        fail('decorated', e);
      }
      return r;
    }
    note(r);
    return r;
  };
  Object.defineProperty(wrapped, 'name', { value: fn.name, configurable: true });
  return wrapped;
}

// A check's result as a decision: true or "block" blocks, "flag" flags,
// anything else allows. Only compared, never kept.
function decisionOf(r) {
  if (r === true) return 'block';
  if (typeof r === 'string' && r.length <= 5) {
    const l = r.toLowerCase();
    if (l === 'block' || l === 'flag') return l;
  }
  return 'allow';
}

// guardrailCheck(fn, { canaries }): the app's own guardrail check, a function
// of the text that returns true (or "block" or "flag") when it would stop it.
// Each call's decision counts for the current request's model calls, and,
// unless canaries is false (an output-only check), the SDK sends it the canary
// probes from its first call, then hourly, with the same `this`. Its argument
// and result are never recorded. Wrap it once, where it is defined: at most
// 32 checks are registered.
function guardrailCheck(fn, { canaries = true } = {}) {
  if (typeof fn !== 'function') {
    fail('guardrailCheck', new TypeError('not a function'));
    return fn;
  }
  let check = null;
  try {
    check = checks.find((c) => c.fn === fn) || null;
    if (!check && checks.length < MAX_CHECKS) {
      check = { fn, canaries: !!canaries };
      checks.push(check);
    } else if (!check) fail('guardrailCheck', new RangeError('more than 32 registered; wrap a check once, where it is defined'));
  } catch (e) {
    fail('guardrailCheck', e);
  }
  const before = (thisArg) => {
    const ensure = hooks.ensureCanaries;
    if (check && check.canaries && ensure) ensure(check, thisArg === undefined || thisArg === globalThis ? fn : fn.bind(thisArg));
  };
  return decorate(fn, (r) => decide(decisionOf(r)), before);
}

// redactor(fn): the app's own redaction function; once it has returned, the
// current request's model calls count as handling personal data, redacted.
// Its arguments and result are never read.
function redactor(fn) {
  return decorate(fn, () => markRedacted());
}

// approve(): the tool call in progress was approved by a person or a policy.
function approve() {
  const f = currentFrame();
  if (f && f.tool) f.tool.approved = true;
}

// A function that runs fn inside scope(fn's name, opts, ...) on every call.
function wrapIn(scope, fn, opts) {
  const wrapped = function (...args) {
    return scope(fn.name || null, opts, () => fn.apply(this, args));
  };
  Object.defineProperty(wrapped, 'name', { value: fn.name, configurable: true });
  return wrapped;
}

// tool(name, {highImpact, approved}, fn): a tool call; fn gets {approved, highImpact, error}.
// tool(fn, {highImpact}): a function that is a tool call each time it runs, named after it.
function tool(name, opts, fn) {
  if (typeof name === 'function') return wrapIn(tool, name, opts || {});
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
    true,
  );
}

// agentRun(name, fn): one run of an agent; calls inside count toward this agent's model history.
// agentRun(fn): a function that is an agent run each time it runs, named after it.
function agentRun(name, fn) {
  if (typeof name === 'function' && fn === undefined) return wrapIn((n, _o, f) => agentRun(n, f), name, {});
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
    record('tellproof.escalation', KIND_INTERNAL, now, now, { [A.ESCALATION]: true }, false);
  } catch (e) {
    fail('escalation', e);
  }
}

module.exports = {
  A,
  SEVERITY,
  configure,
  checks,
  hooks,
  MAX_CHECKS,
  decide,
  markRedacted,
  requestScope,
  runIn,
  guardrailCheck,
  redactor,
  approve,
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
