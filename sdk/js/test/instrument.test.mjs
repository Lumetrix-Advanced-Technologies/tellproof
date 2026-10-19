// node --test sdk/js/test/instrument.test.mjs   (no packages; fake libraries in a temp app)
import { test, before, after, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';
import { join } from 'node:path';
import { rmSync } from 'node:fs';
import { fakeApp } from './fakes.mjs';
import { Capture } from './scenario.mjs';

const require = createRequire(import.meta.url);
const core = require('../lib/core.cjs');
const otlp = require('../lib/otlp.cjs');
const ins = require('../lib/instrument.cjs');
const tellproof = require('../lib/index.cjs');
const { Exporter } = require('../lib/export.cjs');

const MARKER = 'CONTENT-MARKER-\u00e9"\\</';
let app, cwd, cjs, esm, cap;

before(async () => {
  app = fakeApp(MARKER);
  cwd = process.cwd();
  process.chdir(app); // the finder resolves the app's libraries from its directory
  const appRequire = createRequire(join(app, 'app.js'));
  const imp = (name) => import(pathToFileURL(join(app, 'node_modules', ...name.split('/'), 'index.mjs')).href);
  cjs = { openai: appRequire('openai'), anthropic: appRequire('@anthropic-ai/sdk'), bedrock: appRequire('@aws-sdk/client-bedrock-runtime') };
  esm = { openai: await imp('openai'), anthropic: await imp('@anthropic-ai/sdk'), bedrock: await imp('@aws-sdk/client-bedrock-runtime') };
  assert.deepEqual(ins.install().sort(), ['anthropic', 'bedrock', 'openai']);
  assert.deepEqual((await ins.ready()).sort(), ['anthropic', 'bedrock', 'openai']);
});

after(() => {
  process.chdir(cwd);
  rmSync(app, { recursive: true, force: true });
});

let old;
beforeEach(() => {
  ins.setStreamUsage(false);
  cap = new Capture();
  if (old === undefined) old = core.currentClient();
  core.setClient(new core.Client(cap, { service: 'test' }));
});
after(() => core.setClient(old));

const a = (s) => s.attrs;
const tokens = () => cap.spans.map((s) => [a(s)['gen_ai.usage.input_tokens'], a(s)['gen_ai.usage.output_tokens']]);
function facts(s) {
  const m = { provider: 'gen_ai.provider.name', op: 'gen_ai.operation.name', model: 'gen_ai.request.model', response: 'gen_ai.response.model',
    region: 'cloud.region', input: 'gen_ai.usage.input_tokens', output: 'gen_ai.usage.output_tokens', guardrail: 'tellproof.guardrail.decision' };
  return Object.fromEntries(Object.entries(m).filter(([, k]) => k in s.attrs).map(([n, k]) => [n, s.attrs[k]]));
}

for (const [kind, libs] of [['CommonJS', () => cjs], ['ES module', () => esm]]) {
  test(`openai chat completion (${kind} copy)`, async () => {
    const OpenAI = libs().openai.OpenAI;
    const r = await new OpenAI().chat.completions.create({ model: 'gpt-4.1-mini', messages: [{ role: 'user', content: MARKER }] });
    assert.equal(r.usage.prompt_tokens, 120); // the app gets the real result
    assert.equal(cap.spans.length, 1);
    assert.deepEqual(facts(cap.spans[0]), { provider: 'openai', op: 'chat', model: 'gpt-4.1-mini', response: 'gpt-4.1-mini-2025-04-14', input: 120, output: 30 });
    assert.equal(cap.spans[0].kind, 3);
  });

  test(`anthropic message counts cached input (${kind} copy)`, async () => {
    await new (libs().anthropic.Anthropic)().messages.create({ model: 'claude-sonnet-4-5', max_tokens: 10, messages: [] });
    assert.deepEqual(facts(cap.spans[0]), { provider: 'anthropic', op: 'chat', model: 'claude-sonnet-4-5', response: 'claude-sonnet-4-5-20250929', input: 115, output: 20 });
  });

  test(`bedrock converse (${kind} copy)`, async () => {
    const { BedrockRuntimeClient, ConverseCommand } = libs().bedrock;
    const out = await new BedrockRuntimeClient().send(new ConverseCommand({ modelId: 'apac.anthropic.claude-sonnet-4-5', messages: [] }));
    assert.equal(out.output.message.content[0].text, MARKER);
    assert.deepEqual(facts(cap.spans[0]), { provider: 'aws.bedrock', op: 'chat', model: 'apac.anthropic.claude-sonnet-4-5', region: 'ap-southeast-2', input: 50, output: 9 });
  });
}

test('APIPromise keeps withResponse() and stays lazy', async () => {
  const c = new cjs.openai.OpenAI();
  const p = c.chat.completions.create({ model: 'm', messages: [] });
  assert.equal(typeof p.withResponse, 'function');
  await new Promise((r) => setTimeout(r, 5));
  assert.equal(cap.spans.length, 0, 'nothing is parsed until the app consumes it');
  const { data, response, request_id } = await p.withResponse();
  assert.equal(data.usage.completion_tokens, 30);
  assert.equal(response.status, 200);
  assert.equal(request_id, 'req_1');
  assert.equal(cap.spans.length, 1);
  const parsed = await c.chat.completions.parse({ model: 'm', messages: [] });
  assert.equal(parsed.parsed, true);
  assert.equal(cap.spans.length, 2);
});

test('openai responses, embeddings and legacy completions', async () => {
  const c = new cjs.openai.OpenAI();
  await c.responses.create({ model: 'gpt-5', input: MARKER });
  await c.embeddings.create({ model: 'text-embedding-3-small', input: [MARKER] });
  await c.completions.create({ model: 'gpt-3.5-turbo-instruct', prompt: MARKER });
  assert.deepEqual(cap.spans.map((s) => [a(s)['gen_ai.operation.name'], a(s)['gen_ai.usage.input_tokens'], a(s)['gen_ai.usage.output_tokens']]), [
    ['chat', 80, 12],
    ['embeddings', 9, undefined],
    ['text_completion', 4, 2],
  ]);
});

test('a streamed chat counts usage once and keeps the stream object', async () => {
  ins.setStreamUsage(true);
  const body = { model: 'gpt-4.1-mini', messages: [], stream: true };
  const s = await new cjs.openai.OpenAI().chat.completions.create(body);
  assert.ok(s instanceof cjs.openai.Stream);
  assert.equal(body.stream_options, undefined, "the app's request object is never changed");
  assert.equal(cap.spans.length, 0, 'the span waits for the stream to end');
  let n = 0;
  for await (const _ of s) n++;
  assert.equal(n, 3); // two content chunks and the usage chunk
  assert.deepEqual(tokens(), [[120, 30]]);
});

test('stream usage is opt-in and never overrides the app', async () => {
  const c = new cjs.openai.OpenAI();
  let n = 0;
  for await (const _ of await c.chat.completions.create({ model: 'm', messages: [], stream: true })) n++;
  ins.setStreamUsage(true);
  for await (const _ of await c.chat.completions.create({ model: 'm', messages: [], stream: true, stream_options: { include_usage: false } })) n++;
  assert.equal(n, 4);
  assert.deepEqual(tokens(), [[undefined, undefined], [undefined, undefined]]);
});

test("the library's own stream helpers are covered", async () => {
  ins.setStreamUsage(true);
  for await (const _ of new cjs.openai.OpenAI().chat.completions.stream({ model: 'm', messages: [] }));
  const text = await new cjs.anthropic.Anthropic().messages.stream({ model: 'claude', max_tokens: 1, messages: [] }).finalText();
  assert.ok(text.includes('CONTENT-MARKER'));
  assert.deepEqual(tokens(), [[120, 30], [115, 20]]);
});

test('a stream left early records one call', async () => {
  const s = await new cjs.openai.OpenAI().chat.completions.create({ model: 'm', messages: [], stream: true });
  for await (const _ of s) break;
  assert.equal(cap.spans.length, 1);
});

test('responses stream and failure', async () => {
  const c = new cjs.openai.OpenAI();
  for await (const _ of await c.responses.create({ model: 'gpt-5', input: 'x', stream: true }));
  for await (const _ of await c.responses.create({ model: 'gpt-5', input: 'x', stream: true, _final: 'response.failed' }));
  assert.deepEqual(cap.spans.map((s) => [a(s)['gen_ai.usage.input_tokens'], s.error]), [[80, false], [80, true]]);
});

test('anthropic raw stream counts usage once; beta and Bedrock clients', async () => {
  const A = cjs.anthropic;
  for await (const _ of await new A.Anthropic().messages.create({ model: 'claude', max_tokens: 1, messages: [], stream: true }));
  await new A.Anthropic().beta.messages.create({ model: 'claude', max_tokens: 1, messages: [] });
  await new A.AnthropicBedrock().messages.create({ model: 'anthropic.claude-sonnet-4-5', max_tokens: 1, messages: [] });
  assert.deepEqual(tokens(), [[115, 20], [115, 20], [115, 20]]);
  assert.deepEqual([a(cap.spans[2])['gen_ai.provider.name'], a(cap.spans[2])['cloud.region']], ['aws.bedrock', 'ap-southeast-2']);
});

test('errors are recorded and rejected unchanged', async () => {
  await assert.rejects(new cjs.openai.OpenAI().chat.completions.create({ model: 'boom', messages: [] }), (e) => e instanceof cjs.openai.APIError && e.message.includes('CONTENT-MARKER'));
  await assert.rejects(new cjs.anthropic.Anthropic().messages.create({ model: 'boom', max_tokens: 1, messages: [] }));
  const { BedrockRuntimeClient, ConverseCommand, ValidationException } = cjs.bedrock;
  await assert.rejects(new BedrockRuntimeClient().send(new ConverseCommand({ modelId: 'boom' })), ValidationException);
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(cap.spans.map((s) => s.error), [true, true, true]);
  assert.equal(a(cap.spans[2])['cloud.region'], 'ap-southeast-2');
  assert.ok(!JSON.stringify(cap.spans, (k, v) => (typeof v === 'bigint' ? String(v) : v)).includes('CONTENT-MARKER'));
});

test('provider names', async () => {
  await new cjs.openai.AzureOpenAI().chat.completions.create({ model: 'gpt-4o', messages: [] });
  await new cjs.openai.OpenAI({ baseURL: 'https://api.groq.com/openai/v1' }).chat.completions.create({ model: 'llama', messages: [] });
  assert.deepEqual(cap.spans.map((s) => a(s)['gen_ai.provider.name']), ['azure.ai.openai', 'api.groq.com']);
});

test('bedrock: guardrails, streams, invoke headers, other commands, callbacks', async () => {
  const B = cjs.bedrock;
  const c = new B.BedrockRuntimeClient();
  await c.send(new B.ConverseCommand({ modelId: 'm', messages: [] })); // no guardrail configured
  await c.send(new B.ConverseCommand({ modelId: 'm', messages: [], guardrailConfig: { guardrailIdentifier: 'g', guardrailVersion: '1' } }));
  await c.send(new B.ConverseCommand({ modelId: 'm', messages: [], guardrailConfig: {}, _intervene: true }));
  const s = await c.send(new B.ConverseStreamCommand({ modelId: 'm', messages: [], guardrailConfig: {}, _intervene: true }));
  let events = 0;
  for await (const _ of s.stream) events++;
  assert.equal(events, 4);
  const inv = await c.send(new B.InvokeModelCommand({ modelId: 'anthropic.claude', body: '{}' }));
  assert.ok(new TextDecoder().decode(inv.body).includes('CONTENT-MARKER')); // the app still reads its body
  for await (const _ of (await c.send(new B.InvokeModelWithResponseStreamCommand({ modelId: 'm', body: '{}' }))).body);
  await c.send(new B.ListFoundationModelsCommand({}));
  await new B.BedrockRuntime().converse({ modelId: 'agg', messages: [] });
  await new Promise((resolve) => c.send(new B.ConverseCommand({ modelId: 'cb', messages: [] }), () => resolve())); // callback style: untouched
  assert.deepEqual(cap.spans.map((s) => a(s)['tellproof.guardrail.decision'] ?? null), [null, 'allow', 'block', 'block', null, null, null]);
  assert.deepEqual(tokens(), [[50, 9], [50, 9], [50, 9], [50, 9], [42, 7], [undefined, undefined], [50, 9]]);
});

test("a provider's block beats an enclosing guardrail scope", async () => {
  const B = cjs.bedrock;
  await tellproof.guardrail('flag', () => new B.BedrockRuntimeClient().send(new B.ConverseCommand({ modelId: 'm', guardrailConfig: {}, _intervene: true })));
  assert.equal(a(cap.spans[0])['tellproof.guardrail.decision'], 'block');
});

test('calls carry their scopes; a stream read after the scopes ended keeps them', async () => {
  let stream;
  await tellproof.agentRun('claims', () =>
    tellproof.prompt('triage', '3', () =>
      tellproof.guardrail(async (g) => {
        stream = await new cjs.openai.OpenAI().chat.completions.create({ model: 'm', messages: [], stream: true });
        g.allow();
      }),
    ),
  );
  assert.equal(cap.spans.length, 1); // only the agent run so far
  for await (const _ of stream);
  const [run, call] = cap.spans;
  assert.equal(call.traceId, run.traceId);
  assert.equal(call.parent, run.spanId);
  assert.deepEqual([a(call)['gen_ai.agent.name'], a(call)['tellproof.prompt.version'], a(call)['tellproof.guardrail.decision']], ['claims', '3', 'allow']);
});

test('installing twice wraps once; explicit instrument() works', async () => {
  ins.install();
  assert.deepEqual(tellproof.instrument({ openai: cjs.openai, anthropic: cjs.anthropic.Anthropic, bedrock: esm.bedrock }).sort(), ['anthropic', 'bedrock', 'openai']);
  await new cjs.openai.OpenAI().chat.completions.create({ model: 'm', messages: [] });
  assert.equal(cap.spans.length, 1);
});

test('a disabled SDK passes calls straight through', async () => {
  core.setClient(new core.Client());
  const p = new cjs.openai.OpenAI().chat.completions.create({ model: 'm', messages: [] });
  assert.equal(p.constructor.name, 'APIPromise');
  assert.equal((await p).usage.prompt_tokens, 120);
});

// Acceptance (S-4, as S-2): no message content from any client reaches an export.
test('content never reaches an export', async () => {
  const exports = [];
  const exp = new Exporter((b) => exports.push(b), (spans) => otlp.encode(spans, { 'service.name': 't' }, 't'), { intervalMs: 5 });
  core.setClient(new core.Client(exp, { service: 't' }));
  ins.setStreamUsage(true);
  const msgs = [{ role: 'user', content: MARKER }];
  for (const libs of [cjs, esm]) {
    const o = new libs.openai.OpenAI();
    const an = new libs.anthropic.Anthropic();
    const B = libs.bedrock;
    const b = new B.BedrockRuntimeClient();
    await o.chat.completions.create({ model: 'm', messages: msgs });
    for await (const _ of await o.chat.completions.create({ model: 'm', messages: msgs, stream: true }));
    for await (const _ of await o.responses.create({ model: 'm', input: MARKER, stream: true }));
    await o.embeddings.create({ model: 'm', input: [MARKER] });
    await an.messages.create({ model: 'm', max_tokens: 1, messages: msgs });
    await an.messages.stream({ model: 'm', max_tokens: 1, messages: msgs }).finalText();
    await b.send(new B.ConverseCommand({ modelId: 'm', messages: msgs }));
    for await (const _ of (await b.send(new B.ConverseStreamCommand({ modelId: 'm', messages: msgs }))).stream);
    await b.send(new B.InvokeModelCommand({ modelId: 'm', body: MARKER }));
    await o.chat.completions.create({ model: 'boom', messages: msgs }).catch(() => {});
    await an.messages.create({ model: 'boom', max_tokens: 1, messages: msgs }).catch(() => {});
    await b.send(new B.ConverseCommand({ modelId: 'boom', messages: msgs })).catch(() => {});
  }
  await new Promise((r) => setImmediate(r));
  assert.equal(await exp.flush(5000), true);
  const blob = exports.join('');
  assert.equal(blob.split('"spanId"').length - 1, 24);
  assert.ok(!blob.includes('CONTENT-MARKER'));
});

test('the ES module finder follows exports, never the bundler-only module field', async () => {
  const { mkdirSync, writeFileSync } = await import('node:fs');
  const dir = join(app, 'node_modules', 'only-module');
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, 'package.json'), JSON.stringify({ name: 'only-module', main: './cjs.js', module: './es/index.js' }));
  const req = createRequire(join(app, 'app.js'));
  assert.equal(ins.esmEntry('only-module', req), null); // Node serves import from main: one copy
  assert.ok(ins.esmEntry('openai', req).endsWith('/node_modules/openai/index.mjs'));
});

// Acceptance (P8-3, D-065): the leak corpus (spec/leak-corpus.json, shared with
// the agent and the Python SDK) in every request position the clients and the
// helpers see, and the canary probes, never reach an export. The fake
// libraries' responses and errors carry MARKER, checked the same way.
test('nothing of the leak corpus reaches an export', async () => {
  const { readFileSync } = await import('node:fs');
  const spec = (f) => JSON.parse(readFileSync(new URL(`../../../spec/${f}`, import.meta.url), 'utf8'));
  const corpus = spec('leak-corpus.json');
  const probes = spec('canaries.json').probes;
  const TEXT = corpus.markers.map((m) => m.text).join(' | ');
  const needles = [...corpus.markers.map((m) => m.needle), ...probes.map((p) => p.text.slice(0, 40)), 'CONTENT-MARKER'];
  const exports = [];
  const exp = new Exporter((b) => exports.push(b), (spans) => otlp.encode(spans, { 'service.name': 't' }, 't'), { intervalMs: 5 });
  core.setClient(new core.Client(exp, { service: 't' }));
  ins.setStreamUsage(true);
  const msgs = [
    { role: 'system', content: TEXT },
    { role: 'user', content: [{ type: 'text', text: TEXT }, { type: 'image_url', image_url: { url: 'https://example.com/?q=' + TEXT } }] },
    { role: 'assistant', content: TEXT, tool_calls: [{ id: 'c1', type: 'function', function: { name: 'lookup', arguments: TEXT } }] },
    { role: 'tool', tool_call_id: 'c1', content: TEXT },
  ];
  const tools = [{ type: 'function', function: { name: 'lookup', description: TEXT, parameters: { type: 'object', description: TEXT } } }];
  const anth = [
    { role: 'user', content: [{ type: 'text', text: TEXT }, { type: 'tool_result', tool_use_id: 't1', content: TEXT }] },
    { role: 'assistant', content: [{ type: 'tool_use', id: 't1', name: 'lookup', input: { q: TEXT } }] },
  ];
  const conv = [{ role: 'user', content: [{ text: TEXT }, { toolResult: { toolUseId: 't1', content: [{ text: TEXT }] } }] }];
  for (const libs of [cjs, esm]) {
    const o = new libs.openai.OpenAI();
    const an = new libs.anthropic.Anthropic();
    const B = libs.bedrock;
    const b = new B.BedrockRuntimeClient();
    await o.chat.completions.create({ model: 'gpt-4.1-mini', messages: msgs, tools, user: TEXT, metadata: { note: TEXT } });
    for await (const _ of await o.chat.completions.create({ model: 'gpt-4.1-mini', messages: msgs, stream: true }));
    await o.responses.create({ model: 'gpt-5', input: TEXT, instructions: TEXT, tools });
    for await (const _ of await o.responses.create({ model: 'gpt-5', input: [{ role: 'user', content: TEXT }], stream: true }));
    await o.embeddings.create({ model: 'text-embedding-3-small', input: [TEXT, TEXT] });
    await o.completions.create({ model: 'gpt-3.5-turbo-instruct', prompt: TEXT, suffix: TEXT });
    await an.messages.create({ model: 'claude-sonnet-4-5', max_tokens: 1, system: TEXT, messages: anth, tools: [{ name: 'lookup', description: TEXT, input_schema: {} }], metadata: { user_id: TEXT } });
    await an.messages.stream({ model: 'claude-sonnet-4-5', max_tokens: 1, system: [{ type: 'text', text: TEXT }], messages: anth }).finalText();
    await b.send(new B.ConverseCommand({ modelId: 'anthropic.claude-sonnet-4-5', messages: conv, system: [{ text: TEXT }], toolConfig: { tools: [{ toolSpec: { name: 'lookup', description: TEXT } }] } }));
    for await (const _ of (await b.send(new B.ConverseStreamCommand({ modelId: 'anthropic.claude-sonnet-4-5', messages: conv }))).stream);
    await b.send(new B.InvokeModelCommand({ modelId: 'anthropic.claude-sonnet-4-5', body: JSON.stringify({ prompt: TEXT }) }));
    await assert.rejects(o.chat.completions.create({ model: 'boom', messages: msgs }));
    await assert.rejects(an.messages.create({ model: 'boom', max_tokens: 1, messages: anth }));
    await assert.rejects(b.send(new B.ConverseCommand({ modelId: 'boom', messages: conv })));
  }
  // The helpers: the app's own call, a tool and an agent run failing with content, and the canaries.
  await assert.rejects(tellproof.trace(() => tellproof.prompt('triage', '14', () => tellproof.personalData({ redacted: true }, () =>
    tellproof.guardrail('allow', () => tellproof.llmCall({ provider: 'openai', model: 'gpt-4.1-mini' }, async (c) => {
      c.usage(3, 1);
      throw new Error(TEXT);
    }))))));
  await assert.rejects(tellproof.agentRun('refund-agent', () => tellproof.tool('lookup_policy', async () => {
    throw new Error(TEXT);
  })));
  const seen = [];
  const caught = await tellproof.runCanaries((probe) => { seen.push(probe); return true; });
  assert.equal(seen.length, probes.length);
  assert.ok(Object.values(caught).every(Boolean));
  // The simpler usage (D-069): a check and a redactor that take and return
  // content, wrapped tools and agent runs that throw it, detected guardrails,
  // Azure filter results and a stored prompt's variables.
  core.configure({ agent: 'triage', prompt: 'claims', promptVersion: '14', personalData: true });
  const saved = core.checks.splice(0);
  try {
    const check = tellproof.guardrailCheck(async () => TEXT, { canaries: false });
    const redact = tellproof.redactor(() => TEXT);
    const lookup = tellproof.tool(async function lookup() { tellproof.approve(); throw new Error(TEXT); }, { highImpact: true });
    const refund = tellproof.agentRun(async function refund(q) { await lookup(q); });
    const o = new cjs.openai.OpenAI();
    const B = cjs.bedrock;
    await tellproof.trace(async () => {
      redact(TEXT);
      await check(TEXT);
      await o.moderations.create({ input: TEXT });
      await new B.BedrockRuntimeClient().send(new B.ApplyGuardrailCommand({ guardrailIdentifier: 'g', guardrailVersion: '1', source: 'INPUT', content: [{ text: { text: TEXT } }] }));
      await o.responses.create({ model: 'gpt-5', input: TEXT, prompt: { id: 'pmpt_1', version: '2', variables: { x: TEXT } } });
      await new cjs.openai.AzureOpenAI().chat.completions.create({ model: 'gpt-4.1-mini', messages: [{ role: 'user', content: TEXT }] });
      await assert.rejects(refund(TEXT));
    });
    await check(TEXT); // outside a request
    redact(TEXT);
  } finally {
    core.checks.splice(0, core.checks.length, ...saved);
    core.configure();
  }
  await new Promise((r) => setImmediate(r));
  assert.equal(await exp.flush(5000), true);
  const blob = exports.join('');
  assert.ok(blob.split('"spanId"').length - 1 >= 30);
  assert.ok(blob.includes('pmpt_1') && blob.includes('"lookup"'), 'the D-069 section ran');
  const low = blob.toLowerCase();
  for (const n of needles) {
    const i = low.indexOf(n.toLowerCase());
    assert.equal(i, -1, `${n} reached an export: …${blob.slice(Math.max(0, i - 80), i + 80)}…`);
  }
});
