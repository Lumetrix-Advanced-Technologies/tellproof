'use strict';
// Automatic instrumentation of the LLM clients an app already uses (D-034, D-036).
//
// init() patches, when installed: openai (chat completions, responses,
// embeddings, legacy completions; create, which parse() and stream() call), @anthropic-ai/sdk (messages and beta
// messages; the libraries' own stream helpers call create) and
// @aws-sdk/client-bedrock-runtime (Converse, ConverseStream, InvokeModel,
// InvokeModelWithResponseStream). The copy the app loads is patched: CommonJS
// when the app requires it, the ES module copy in an ES module app.
//
// Only these are read: the request's model and stream flag, whether a Bedrock
// guardrail is configured, and from the response its model, usage block, stop
// reason and Bedrock's token-count headers. Never messages, inputs, outputs,
// tool arguments, bodies or error text. The library's own objects are returned
// (APIPromise keeps withResponse(); streams keep their identity).

const fs = require('node:fs');
const path = require('node:path');
const { createRequire } = require('node:module');
const { pathToFileURL } = require('node:url');
const core = require('./core.cjs');

const { A } = core;
const MARK = Symbol.for('tellproof.wrapped');
let streamUsage = false;

function int(v) {
  if (typeof v === 'number' && Number.isInteger(v) && v >= 0) return v;
  if (typeof v === 'string' && /^\d+$/.test(v)) return Number(v);
  return null;
}

// Only ever called with the names of usage and metadata fields.
function get(obj, name) {
  if (obj === null || obj === undefined) return undefined;
  try {
    return obj[name];
  } catch {
    return undefined;
  }
}

class Call {
  constructor(provider, model, operation, region = null) {
    const c = core.currentClient();
    this.frame = core.currentFrame();
    this.start = c.clock();
    this.spanId = c.ids(8);
    this.traceId = this.frame ? null : c.ids(16);
    this.provider = core.clean(provider) || 'unknown';
    this.model = core.clean(model) || 'unknown';
    this.operation = operation;
    this.region = core.clean(region);
    this.responseModel = null;
    this.inputTokens = null;
    this.outputTokens = null;
    this.guardrail = null;
    this.prompt = null; // a stored prompt's [id, version] (OpenAI Responses)
    this.error = false;
    this.done = false;
  }

  finish(error = false) {
    if (this.done) return;
    this.done = true;
    try {
      const a = { [A.OPERATION]: this.operation, [A.PROVIDER]: this.provider, [A.REQUEST_MODEL]: this.model };
      const rm = core.clean(this.responseModel);
      if (rm) a[A.RESPONSE_MODEL] = rm;
      if (this.region) a[A.REGION] = this.region;
      if (this.inputTokens !== null) a[A.INPUT_TOKENS] = this.inputTokens;
      if (this.outputTokens !== null) a[A.OUTPUT_TOKENS] = this.outputTokens;
      if (this.guardrail) a[A.GUARDRAIL] = this.guardrail;
      if (this.prompt) {
        a[A.PROMPT_NAME] = this.prompt[0];
        if (this.prompt[1]) a[A.PROMPT_VERSION] = this.prompt[1];
      }
      core.record(this.operation, core.KIND_CLIENT, this.start, core.currentClient().clock(), a, error || this.error, {
        llm: true,
        spanId: this.spanId,
        traceId: this.traceId,
        frame: this.frame,
      });
    } catch (e) {
      core.fail('finish', e);
    }
  }
}

function safe(fn, ...args) {
  try {
    fn(...args);
  } catch (e) {
    core.fail('observe', e);
  }
}

// A stream dropped unfinished is recorded once it is collected.
const dropped = typeof FinalizationRegistry === 'function' ? new FinalizationRegistry((call) => core.later.push(() => call.finish())) : null;

function wrapIterator(it, call, observe) {
  return {
    async next(...a) {
      let r;
      try {
        r = await it.next(...a);
      } catch (e) {
        call.finish(true);
        throw e;
      }
      if (r.done) call.finish();
      else safe(observe, call, r.value);
      return r;
    },
    async return(v) {
      try {
        return typeof it.return === 'function' ? await it.return(v) : { done: true, value: v };
      } finally {
        call.finish();
      }
    },
    async throw(e) {
      try {
        if (typeof it.throw === 'function') return await it.throw(e);
        throw e;
      } finally {
        call.finish();
      }
    },
    [Symbol.asyncIterator]() {
      return this;
    },
  };
}

// Observe a stream in place: only its async iterator is wrapped, so the
// object, its class and its private fields are untouched.
function observeStream(stream, call, observe) {
  const orig = stream[Symbol.asyncIterator];
  if (typeof orig !== 'function') {
    call.finish();
    return stream;
  }
  Object.defineProperty(stream, Symbol.asyncIterator, {
    configurable: true,
    writable: true,
    value() {
      return wrapIterator(orig.call(stream), call, observe);
    },
  });
  if (dropped) dropped.register(stream, call);
  return stream;
}

function isStream(v) {
  return v !== null && typeof v === 'object' && typeof v[Symbol.asyncIterator] === 'function' && get(v, 'usage') === undefined;
}

// Watch a client call's result without changing what the app gets back.
// Stainless clients (openai, anthropic) return an APIPromise: _thenUnwrap
// gives a new APIPromise over the same response that still has withResponse()
// and parses lazily, as the library's own helpers use it.
function observeResult(p, call, done) {
  const finishWith = (data) => {
    try {
      return done(data);
    } catch (e) {
      core.fail('done', e);
      call.finish();
      return data;
    }
  };
  if (p !== null && typeof p === 'object' && typeof p._thenUnwrap === 'function') {
    const rp = get(p, 'responsePromise');
    if (rp && typeof rp.then === 'function')
      rp.then(undefined, (e) => {
        safe(refused, call, e);
        call.finish(true);
      });
    return p._thenUnwrap((data) => finishWith(data));
  }
  if (p !== null && typeof p === 'object' && typeof p.then === 'function') {
    return p.then(finishWith, (e) => {
      safe(refused, call, e);
      call.finish(true);
      throw e;
    });
  }
  return finishWith(p);
}

// Azure OpenAI refuses a prompt its content filter stops with the error code
// content_filter: a guardrail block. Only the code is read.
function refused(call, err) {
  if (call.provider === 'azure.ai.openai' && err && err.code === 'content_filter') call.guardrail = 'block';
}

// Observe a result that is not a model call, keeping a Stainless APIPromise one.
function observeOnly(p, after) {
  const note = (v) => {
    if (core.currentClient().enabled) safe(after, v);
    return v;
  };
  if (p !== null && typeof p === 'object' && typeof p._thenUnwrap === 'function') return p._thenUnwrap(note);
  if (p !== null && typeof p === 'object' && typeof p.then === 'function') return p.then(note);
  return note(p);
}

function patch(owner, name, make) {
  if (!owner) return false;
  const orig = owner[name];
  if (typeof orig !== 'function' || orig[MARK]) return false;
  const wrapped = make(orig);
  Object.defineProperty(wrapped, MARK, { value: orig });
  Object.defineProperty(owner, name, { value: wrapped, writable: true, configurable: true });
  return true;
}

function begin(start, self, args) {
  if (!core.currentClient().enabled) return null;
  try {
    return start(self, args);
  } catch (e) {
    core.fail('start', e);
    return null;
  }
}

function wrapCall(orig, start, done, rewrite = null) {
  return function tellproofWrapped(...args) {
    const call = begin(start, this, args);
    if (call === null) return orig.apply(this, args);
    if (rewrite) {
      try {
        args = rewrite(args);
      } catch (e) {
        core.fail('rewrite', e);
      }
    }
    let p;
    try {
      p = orig.apply(this, args);
    } catch (e) {
      call.finish(true);
      throw e;
    }
    try {
      return observeResult(p, call, (data) => done(call, data));
    } catch (e) {
      core.fail('observe', e);
      call.finish();
      return p;
    }
  };
}

// --- OpenAI ---------------------------------------------------------------

function openaiProvider(resource) {
  const client = get(resource, '_client');
  const name = (client && client.constructor && client.constructor.name) || '';
  if (name.includes('Azure')) return 'azure.ai.openai';
  let host = '';
  try {
    host = new URL(String(get(client, 'baseURL') || '')).hostname;
  } catch {}
  if (host === '' || host === 'api.openai.com' || host.endsWith('.openai.com')) return 'openai';
  if (host.endsWith('.openai.azure.com')) return 'azure.ai.openai';
  return host; // an OpenAI-compatible endpoint: name it by its host
}

function openaiUsage(call, usage) {
  if (!usage) return;
  // Chat, completions, embeddings: prompt/completion; responses: input/output. Input includes cached tokens (D-028).
  const i = int(get(usage, 'prompt_tokens')) ?? int(get(usage, 'input_tokens'));
  const o = int(get(usage, 'completion_tokens')) ?? int(get(usage, 'output_tokens'));
  if (i !== null) call.inputTokens = i;
  if (o !== null) call.outputTokens = o;
}

function openaiObserve(call, item) {
  const type = get(item, 'type');
  if (typeof type === 'string' && type.startsWith('response.')) {
    if (type === 'response.completed' || type === 'response.incomplete' || type === 'response.failed') {
      const r = get(item, 'response');
      const m = get(r, 'model');
      if (typeof m === 'string') call.responseModel = m;
      openaiUsage(call, get(r, 'usage'));
      if (type === 'response.failed') call.error = true;
    }
    return;
  }
  const m = get(item, 'model');
  if (typeof m === 'string') call.responseModel = m;
  openaiUsage(call, get(item, 'usage'));
  azureFilter(call, item);
}

// Azure OpenAI's content filter ran when a response carries its results: the
// call was guarded, and blocked when a choice ended on content_filter. Only
// the presence of the results and the finish reasons are read.
function azureFilter(call, data) {
  if (call.provider !== 'azure.ai.openai') return;
  const choices = get(data, 'choices');
  if (!Array.isArray(choices)) return;
  const ran = get(data, 'prompt_filter_results') != null || choices.some((c) => get(c, 'content_filter_results') != null);
  if (choices.some((c) => get(c, 'finish_reason') === 'content_filter')) call.guardrail = 'block';
  else if (ran && call.guardrail === null) call.guardrail = 'allow';
}

// An OpenAI moderation check is a guardrail: flagged or not, for the request's
// model calls. Only the flagged booleans are read.
function moderationDone(data) {
  const results = get(data, 'results');
  if (Array.isArray(results) && results.length) core.decide(results.some((r) => get(r, 'flagged') === true) ? 'flag' : 'allow');
}

function openaiDone(call, data) {
  if (isStream(data)) return observeStream(data, call, openaiObserve);
  const m = get(data, 'model');
  if (typeof m === 'string') call.responseModel = m;
  openaiUsage(call, get(data, 'usage'));
  azureFilter(call, data);
  if (get(data, 'status') === 'failed') call.error = true;
  call.finish();
  return data;
}

function openaiStart(operation, responses = false) {
  return (self, args) => {
    const body = args[0] || {};
    const call = new Call(openaiProvider(self), get(body, 'model'), operation);
    const ref = responses ? get(body, 'prompt') : null;
    if (ref && typeof ref === 'object') {
      // A stored prompt is named by reference: its ID and version, never its text or variables.
      const id = core.clean(get(ref, 'id'));
      if (id) call.prompt = [id, core.clean(get(ref, 'version'))];
    }
    return call;
  };
}

// streamUsage: ask for usage on a streamed chat completion the app has not
// configured (a copy of the body; the app's object is never changed).
function chatRewrite(args) {
  const body = args[0];
  if (!streamUsage || !body || typeof body !== 'object' || body.stream !== true || body.stream_options !== undefined) return args;
  return [{ ...body, stream_options: { include_usage: true } }, ...args.slice(1)];
}

function instrumentOpenAI(mod) {
  const OpenAI = mod && (mod.OpenAI || (mod.default && mod.default.OpenAI) || mod.default || mod);
  if (!OpenAI || typeof OpenAI !== 'function') return false;
  const targets = [
    // parse() and the stream() helpers call create(), so create() is the one place to patch.
    [OpenAI.Chat && OpenAI.Chat.Completions, ['create'], 'chat', chatRewrite, false],
    [OpenAI.Responses, ['create'], 'chat', null, true],
    [OpenAI.Embeddings, ['create'], 'embeddings', null, false],
    [OpenAI.Completions, ['create'], 'text_completion', null, false],
  ];
  let any = false;
  for (const [cls, methods, op, rewrite, responses] of targets) {
    if (!cls || !cls.prototype) continue;
    for (const m of methods) {
      patch(cls.prototype, m, (orig) => wrapCall(orig, openaiStart(op, responses), openaiDone, rewrite));
      any = true;
    }
  }
  if (OpenAI.Moderations && OpenAI.Moderations.prototype) {
    patch(OpenAI.Moderations.prototype, 'create', (orig) =>
      function create(...args) {
        return observeOnly(orig.apply(this, args), moderationDone);
      },
    );
  }
  return any;
}

// --- Anthropic --------------------------------------------------------------

const ANTHROPIC_INPUT = ['input_tokens', 'cache_creation_input_tokens', 'cache_read_input_tokens'];

// Billed input = uncached + cache writes + cache reads (D-028). Stream deltas
// are cumulative and may carry only some fields: each keeps its latest value.
function anthropicUsage(call, usage) {
  if (!usage) return;
  call.anthropicInput = call.anthropicInput || {};
  for (const k of ANTHROPIC_INPUT) {
    const v = int(get(usage, k));
    if (v !== null) call.anthropicInput[k] = v;
  }
  const parts = Object.values(call.anthropicInput);
  if (parts.length) call.inputTokens = parts.reduce((x, y) => x + y, 0);
  const o = int(get(usage, 'output_tokens'));
  if (o !== null) call.outputTokens = o;
}

function anthropicMessage(call, msg) {
  const m = get(msg, 'model');
  if (typeof m === 'string') call.responseModel = m;
  anthropicUsage(call, get(msg, 'usage'));
}

function anthropicObserve(call, ev) {
  const type = get(ev, 'type');
  if (type === 'message_start') anthropicMessage(call, get(ev, 'message'));
  else if (type === 'message_delta') anthropicUsage(call, get(ev, 'usage'));
  else if (type === 'error') call.error = true;
}

function anthropicProvider(resource) {
  const client = get(resource, '_client');
  const name = (client && client.constructor && client.constructor.name) || '';
  if (name.includes('Bedrock')) return ['aws.bedrock', get(client, 'awsRegion')];
  if (name.includes('Vertex')) return ['gcp.vertex_ai', get(client, 'region')];
  return ['anthropic', null];
}

function anthropicDone(call, data) {
  if (isStream(data)) return observeStream(data, call, anthropicObserve);
  anthropicMessage(call, data);
  call.finish();
  return data;
}

function anthropicStart(self, args) {
  const [provider, region] = anthropicProvider(self);
  return new Call(provider, get(args[0] || {}, 'model'), 'chat', region);
}

function instrumentAnthropic(mod) {
  const Anthropic = mod && (mod.Anthropic || (mod.default && mod.default.Anthropic) || mod.default || mod);
  if (!Anthropic || typeof Anthropic !== 'function') return false;
  let any = false;
  for (const cls of [Anthropic.Messages, Anthropic.Beta && Anthropic.Beta.Messages]) {
    if (!cls || !cls.prototype) continue;
    patch(cls.prototype, 'create', (orig) => wrapCall(orig, anthropicStart, anthropicDone));
    any = true;
  }
  return any;
}

// --- Amazon Bedrock (@aws-sdk/client-bedrock-runtime) ----------------------

function bedrockKind(command, mod) {
  if (!command || typeof command !== 'object') return null;
  if (mod.ConverseCommand && command instanceof mod.ConverseCommand) return 'converse';
  if (mod.ConverseStreamCommand && command instanceof mod.ConverseStreamCommand) return 'converseStream';
  if (mod.InvokeModelCommand && command instanceof mod.InvokeModelCommand) return 'invoke';
  if (mod.InvokeModelWithResponseStreamCommand && command instanceof mod.InvokeModelWithResponseStreamCommand) return 'invokeStream';
  return null;
}

// inputTokens excludes cache reads and writes, as CloudWatch's InputTokenCount does (D-028).
function bedrockUsage(call, usage) {
  const i = int(get(usage, 'inputTokens'));
  const o = int(get(usage, 'outputTokens'));
  if (i !== null) call.inputTokens = i;
  if (o !== null) call.outputTokens = o;
}

function bedrockStop(call, reason, guarded) {
  if (guarded) call.guardrail = reason === 'guardrail_intervened' ? 'block' : 'allow';
}

async function region(config) {
  const r = get(config, 'region');
  return typeof r === 'function' ? await r() : r;
}

// InvokeModel's body holds the completion and is never read; Bedrock reports
// the counts in response headers, which only a middleware can see.
function headerMiddleware(call) {
  return (next) => async (args) => {
    const result = await next(args);
    safe(() => {
      const h = get(get(result, 'response'), 'headers') || {};
      const i = int(h['x-amzn-bedrock-input-token-count']);
      const o = int(h['x-amzn-bedrock-output-token-count']);
      if (i !== null) call.inputTokens = i;
      if (o !== null) call.outputTokens = o;
    });
    return result;
  };
}

// Bedrock's ApplyGuardrail is a guardrail check: blocked when it intervened.
// Only the action is read.
function applyGuardrailDone(out) {
  const action = get(out, 'action');
  if (typeof action === 'string') core.decide(action === 'GUARDRAIL_INTERVENED' ? 'block' : 'allow');
}

function wrapSend(orig, mod) {
  return function send(command, ...rest) {
    if (core.currentClient().enabled && mod.ApplyGuardrailCommand && command instanceof mod.ApplyGuardrailCommand && !rest.some((x) => typeof x === 'function'))
      return observeOnly(orig.call(this, command, ...rest), applyGuardrailDone);
    const kind = core.currentClient().enabled ? bedrockKind(command, mod) : null;
    if (kind === null || rest.some((x) => typeof x === 'function')) return orig.call(this, command, ...rest); // callback style: untouched
    let call;
    let guarded = false;
    try {
      const input = get(command, 'input') || {};
      guarded = input.guardrailConfig !== undefined || input.guardrailIdentifier !== undefined;
      call = new Call('aws.bedrock', input.modelId, 'chat');
      if (kind === 'invoke' && command.middlewareStack && typeof command.middlewareStack.add === 'function') {
        command.middlewareStack.add(headerMiddleware(call), { step: 'deserialize', name: 'tellproofTokenCounts', override: true });
      }
    } catch (e) {
      core.fail('bedrock start', e);
      return orig.call(this, command, ...rest);
    }
    const regionP = region(this.config).catch(() => null);
    let p;
    try {
      p = orig.call(this, command, ...rest);
    } catch (e) {
      call.finish(true);
      throw e;
    }
    return Promise.resolve(p).then(
      async (out) => {
        try {
          call.region = core.clean(await regionP);
          if (kind === 'converse') {
            bedrockUsage(call, get(out, 'usage'));
            bedrockStop(call, get(out, 'stopReason'), guarded);
          } else if (kind === 'converseStream' && get(out, 'stream')) {
            observeStream(out.stream, call, (c, ev) => {
              if (ev && ev.metadata) bedrockUsage(c, ev.metadata.usage);
              if (ev && ev.messageStop) bedrockStop(c, ev.messageStop.stopReason, guarded);
              if (ev && Object.keys(ev).some((k) => k.endsWith('Exception'))) c.error = true;
            });
            return out;
          } else if (kind === 'invokeStream' && get(out, 'body')) {
            // Token counts arrive only inside the streamed body; the call is recorded without them.
            observeStream(out.body, call, () => {});
            return out;
          }
        } catch (e) {
          core.fail('bedrock done', e);
        }
        call.finish();
        return out;
      },
      async (err) => {
        call.region = core.clean(await regionP);
        call.finish(true);
        throw err;
      },
    );
  };
}

function instrumentBedrock(mod) {
  const m = mod && (mod.BedrockRuntimeClient ? mod : mod.default);
  if (!m || !m.BedrockRuntimeClient || !m.BedrockRuntimeClient.prototype) return false;
  // send is inherited from the smithy Client; the own property on the Bedrock
  // client's prototype affects only Bedrock clients (and BedrockRuntime).
  patch(m.BedrockRuntimeClient.prototype, 'send', (orig) => wrapSend(orig, m));
  return true;
}

// --- Finding the libraries ----------------------------------------------------

const LIBS = [
  ['openai', 'openai', instrumentOpenAI],
  ['anthropic', '@anthropic-ai/sdk', instrumentAnthropic],
  ['bedrock', '@aws-sdk/client-bedrock-runtime', instrumentBedrock],
];

function requirers() {
  const out = [];
  try {
    out.push(createRequire(path.join(process.cwd(), 'noop.js')));
  } catch {}
  out.push(require);
  return out;
}

// The file `import` loads for an installed package, resolved from its
// package.json exports the way Node resolves them: the first condition, in
// the object's own order, that Node's ES module loader matches ("node",
// "import", "module-sync" where require(esm) is on, "default"). openai 6+ and
// @anthropic-ai/sdk put the ES module build under "default" with no "import"
// key, so that one is not optional. Returns null when import loads the same
// file as require (no exports, the AWS SDK; or both conditions agree): one
// copy. Node ignores the bundler-only "module" field.
const IMPORT_CONDITIONS = new Set(['node', 'import', 'default', ...(process.features && process.features.require_module ? ['module-sync'] : [])]);

function exportTarget(e, depth = 0) {
  if (depth > 8 || e === null || e === undefined) return null;
  if (typeof e === 'string') return e;
  if (Array.isArray(e)) {
    for (const x of e) {
      const t = exportTarget(x, depth + 1);
      if (t) return t;
    }
    return null;
  }
  if (typeof e !== 'object') return null;
  for (const [k, v] of Object.entries(e)) {
    if (!IMPORT_CONDITIONS.has(k)) continue;
    const t = exportTarget(v, depth + 1);
    if (t) return t;
  }
  return null;
}

function esmEntry(spec, req) {
  const dirs = (req.resolve.paths && req.resolve.paths(spec)) || [];
  for (const d of dirs) {
    const dir = path.join(d, spec);
    let pkg;
    try {
      pkg = JSON.parse(fs.readFileSync(path.join(dir, 'package.json'), 'utf8'));
    } catch {
      continue;
    }
    let e = pkg.exports;
    if (e === undefined || e === null) return null;
    if (typeof e === 'object' && !Array.isArray(e) && Object.keys(e).some((k) => k.startsWith('.'))) e = e['.'];
    const t = exportTarget(e);
    if (!t) return null;
    const file = path.join(dir, t);
    try {
      if (req.resolve(spec) === file) return null;
    } catch {}
    return pathToFileURL(file).href;
  }
  return null;
}

// --- Node.js HTTP servers: one trace (request) per request ----------------------

// Every framework on node:http or node:https (Express, Fastify, Koa, Next.js,
// Nest...) receives requests through Server#emit('request'): run its
// listeners inside a request scope that ends when the response closes.
function instrumentServers() {
  let any = false;
  for (const name of ['node:http', 'node:https']) {
    let mod;
    try {
      mod = require(name);
    } catch {
      continue;
    }
    if (!mod.Server || !mod.Server.prototype) continue;
    patch(mod.Server.prototype, 'emit', (orig) =>
      function emit(event, req, res) {
        if (event !== 'request' || !core.currentClient().enabled || !res || typeof res.once !== 'function') return orig.apply(this, arguments);
        let scope = null;
        try {
          scope = core.requestScope();
          res.once('close', scope.leave);
        } catch (e) {
          core.fail('request', e);
          return orig.apply(this, arguments);
        }
        const args = arguments;
        return core.runIn(scope.frame, () => orig.apply(this, args));
      },
    );
    any = true;
  }
  return any;
}

let ready = Promise.resolve([]);
const done = new Set(); // what is patched in a copy the app has loaded
const BY_SPEC = new Map(LIBS.map(([name, spec, fn]) => [spec, [name, fn]]));
let hooked = false;

function patchLib(name, fn, mod) {
  try {
    if (fn(mod)) done.add(name);
  } catch (e) {
    core.fail('instrument ' + name, e);
  }
}

// CommonJS copies are patched when the app first requires them (or now, if it
// already has). The SDK never loads a library itself: in an ES module app the
// CommonJS copy is a second instance the app never calls, megabytes for nothing.
function hookRequire() {
  if (hooked) return;
  hooked = true;
  const Module = require('node:module');
  patch(Module, '_load', (orig) =>
    function _load(request) {
      const exp = orig.apply(this, arguments);
      const lib = typeof request === 'string' && BY_SPEC.get(request);
      if (lib) patchLib(lib[0], lib[1], exp);
      return exp;
    },
  );
}

// Patch every installed library the app uses: CommonJS copies as the app
// requires them; when the app's entry is an ES module, the ES module copies as
// soon as they load (the app's own static imports have loaded them already, so
// importing them again costs nothing). A CommonJS app that import()s a client
// passes it to instrument(). Returns the names patched so far; ready()
// resolves with the full list.
function install({ streamUsage: su = false, requests = false } = {}) {
  streamUsage = !!su;
  if (requests) {
    try {
      if (instrumentServers()) done.add('http');
    } catch (e) {
      core.fail('instrument http', e);
    }
  }
  try {
    hookRequire();
  } catch (e) {
    core.fail('instrument require', e);
  }
  const esmApp = !require.main;
  const esm = [];
  for (const [name, spec, fn] of LIBS) {
    for (const req of requirers()) {
      let file;
      try {
        file = req.resolve(spec);
      } catch {
        continue;
      }
      const loaded = require.cache[file];
      if (loaded) patchLib(name, fn, loaded.exports);
      const url = esmApp && esmEntry(spec, req);
      if (url) esm.push(import(url).then((ns) => patchLib(name, fn, ns), () => {}));
      break;
    }
  }
  ready = Promise.all(esm).then(() => [...done]);
  return [...done];
}

// Explicit instrumentation for setups the finder cannot see (bundlers, strict
// package managers): pass the module or class you import.
function instrument({ openai, anthropic, bedrock } = {}) {
  const done = [];
  if (openai && instrumentOpenAI(openai)) done.push('openai');
  if (anthropic && instrumentAnthropic(anthropic)) done.push('anthropic');
  if (bedrock && instrumentBedrock(bedrock)) done.push('bedrock');
  return done;
}

module.exports = {
  install,
  instrument,
  ready: () => ready,
  patched: () => [...done],
  setStreamUsage: (v) => {
    streamUsage = !!v;
  },
  Call,
  esmEntry,
  instrumentServers,
  instrumentOpenAI,
  instrumentAnthropic,
  instrumentBedrock,
};
