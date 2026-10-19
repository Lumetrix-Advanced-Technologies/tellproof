// node --test sdk/js/test/simple.test.mjs   (no packages; fake libraries in a temp app)
// The simpler usage (D-069): init() defaults, a request scope per Node.js HTTP
// request, guardrailCheck(), redactor(), tool(fn), agentRun(fn), approve(),
// automatic canaries, and guardrails detected without code (OpenAI
// moderation, Bedrock ApplyGuardrail, Azure OpenAI content filters, OpenAI
// stored prompts). None of it may read or export content.
import { test, before, after, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { join } from 'node:path';
import { rmSync } from 'node:fs';
import http from 'node:http';
import { fakeApp } from './fakes.mjs';
import { Capture } from './scenario.mjs';

const require = createRequire(import.meta.url);
const core = require('../lib/core.cjs');
const ins = require('../lib/instrument.cjs');
const canaries = require('../lib/canary.cjs');
const tellproof = require('../lib/index.cjs');

const MARKER = 'CONTENT-MARKER-simple';
const G = core.A.GUARDRAIL;
let app, cwd, lib, cap;

before(() => {
  app = fakeApp(MARKER);
  cwd = process.cwd();
  process.chdir(app);
  const appRequire = createRequire(join(app, 'app.js'));
  lib = { openai: appRequire('openai'), bedrock: appRequire('@aws-sdk/client-bedrock-runtime') };
  ins.install({ requests: true });
});
after(() => {
  process.chdir(cwd);
  rmSync(app, { recursive: true, force: true });
});

let old;
beforeEach(() => {
  cap = new Capture();
  if (old === undefined) old = core.currentClient();
  core.setClient(new core.Client(cap, { service: 'test' }));
  core.checks.length = 0;
  core.configure();
  canaries.stopAuto();
});
after(() => {
  core.setClient(old);
  core.checks.length = 0;
  core.configure();
  canaries.stopAuto();
});

const llm = () => cap.spans.filter((s) => s.kind === core.KIND_CLIENT);
const decisions = () => llm().map((s) => s.attrs[G] ?? null);
const noContent = () => {
  const dump = JSON.stringify(cap.spans, (k, v) => (typeof v === 'bigint' ? String(v) : v));
  assert.ok(!dump.includes(MARKER), 'content in a span');
  assert.ok(!dump.includes('FLAG'), 'check input in a span');
};
const openai = (o = {}) => new lib.openai.OpenAI(o);
const chat = (client = openai(), model = 'gpt-4.1-mini') => client.chat.completions.create({ model, messages: [{ role: 'user', content: MARKER }] });

test('init names every call unless a scope does', async () => {
  core.configure({ agent: 'triage', prompt: 'claims', promptVersion: '14' });
  await chat();
  await tellproof.prompt('claims', '15', () => chat());
  const [a, b] = llm();
  assert.deepEqual([a.attrs[core.A.AGENT_NAME], a.attrs[core.A.PROMPT_NAME], a.attrs[core.A.PROMPT_VERSION]], ['triage', 'claims', '14']);
  assert.equal(b.attrs[core.A.PROMPT_VERSION], '15');
});

test('personal data is unredacted until a redactor runs in the request', async () => {
  core.configure({ personalData: true });
  const redact = tellproof.redactor(async (text) => text.replace(MARKER, '[x]'));
  await tellproof.trace(() => chat());
  await tellproof.trace(async () => {
    await redact(MARKER);
    await chat();
  });
  await chat(); // outside any request: personal, not redacted
  assert.deepEqual(llm().map((s) => [s.attrs[core.A.DATA_CLASS], s.attrs[core.A.REDACTED]]), [
    ['personal', false],
    ['personal', true],
    ['personal', false],
  ]);
  noContent();
});

test('the strictest decision in a request applies to its calls; outside one nothing is attributed but a block', async () => {
  const input = tellproof.guardrailCheck((t) => t.includes('FLAG'), { canaries: false });
  const output = tellproof.guardrailCheck(async (t) => (t.includes('FLAG') ? 'flag' : 'allow'), { canaries: false });
  await tellproof.trace(async () => {
    input(MARKER);
    await chat();
    await output('FLAG ' + MARKER);
    await chat();
  });
  await tellproof.trace(async () => {
    input(MARKER);
    await chat();
  });
  await tellproof.trace(() => chat());
  await tellproof.trace(() => {
    if (input('FLAG')) return; // refused: the model is never called
  });
  input(MARKER); // outside a request: not attributed
  await chat();
  assert.deepEqual(decisions(), ['flag', 'flag', 'allow', null, 'block', null]);
  assert.equal(core.currentClient().stats.unscoped, 1);
  noContent();
});

test('a check returns, rejects and throws as before', async () => {
  const check = tellproof.guardrailCheck(() => true, { canaries: false });
  const bad = tellproof.guardrailCheck(async () => {
    throw new Error(MARKER);
  }, { canaries: false });
  assert.equal(check('x'), true);
  await assert.rejects(bad('x'), /CONTENT-MARKER/);
  assert.equal(check.name, '');
  function named(t) {
    return false;
  }
  assert.equal(tellproof.guardrailCheck(named, { canaries: false }).name, 'named');
});

test('tool(fn) and agentRun(fn) are named after the function; approve()', async () => {
  const lookup = tellproof.tool(function lookup(c) {
    return c;
  });
  const issueRefund = tellproof.tool(
    async function issueRefund(c) {
      tellproof.approve();
      return c;
    },
    { highImpact: true },
  );
  const triage = tellproof.agentRun(async function triage(c) {
    lookup(c);
    await issueRefund(c);
    await chat();
  });
  await triage(MARKER);
  const by = Object.fromEntries(cap.spans.map((s) => [s.attrs[core.A.TOOL_NAME] || s.name, s.attrs]));
  assert.equal(by.lookup[core.A.HIGH_IMPACT], false);
  assert.deepEqual([by.issueRefund[core.A.HIGH_IMPACT], by.issueRefund[core.A.APPROVED]], [true, true]);
  assert.equal(by.invoke_agent[core.A.AGENT_NAME], 'triage');
  assert.equal(llm()[0].attrs[core.A.AGENT_NAME], 'triage');
  noContent();
});

const canarySpans = () => cap.spans.filter((s) => canaries.CANARY_KIND in s.attrs);
async function waitCanaries(n = Object.keys(canaries.PROBES).length) {
  for (let i = 0; i < 200 && canarySpans().length < n; i++) await new Promise((r) => setTimeout(r, 10));
}

test('a guardrailCheck gets canaries from its first call, and not when off', async () => {
  const seen = [];
  const check = tellproof.guardrailCheck(async (t) => {
    seen.push(t);
    return 'block';
  });
  const outputOnly = tellproof.guardrailCheck(() => false, { canaries: false });
  canaries.startAuto(3_600_000);
  await new Promise((r) => setTimeout(r, 30));
  assert.deepEqual(canarySpans(), [], 'canaries before the check ever ran');
  await tellproof.trace(async () => {
    await check(MARKER);
    outputOnly(MARKER);
  });
  await waitCanaries();
  assert.deepEqual(canarySpans().map((s) => s.attrs[canaries.CANARY_KIND]).sort(), Object.keys(canaries.PROBES).sort());
  assert.ok(canarySpans().every((s) => s.attrs[canaries.CANARY_CAUGHT] === true));
  assert.deepEqual(decisions(), ['block'], 'a canary became a decision');
  assert.deepEqual(seen.filter((t) => t !== MARKER).sort(), Object.values(canaries.PROBES).sort());
  canaries.stopAuto();
  cap.spans.length = 0;
  await check(MARKER); // stopped (shutdown, or init({ canaries: false })): no canaries
  await new Promise((r) => setTimeout(r, 30));
  assert.deepEqual(canarySpans(), []);
  canaries.startAuto(3_600_000); // init() again: they start again
  await check(MARKER);
  await waitCanaries();
  assert.equal(canarySpans().length, Object.keys(canaries.PROBES).length);
  canaries.stopAuto();
});

test('a method gets its canaries with its own this', async () => {
  class Guard {
    constructor() {
      this.words = ['Ignore', 'game', 'card', 'AWS'];
    }
    check(text) {
      return this.words.some((w) => text.includes(w));
    }
  }
  Guard.prototype.check = tellproof.guardrailCheck(Guard.prototype.check);
  canaries.startAuto(3_600_000);
  new Guard().check(MARKER);
  await waitCanaries();
  assert.ok(canarySpans().length === 4 && canarySpans().every((s) => s.attrs[canaries.CANARY_CAUGHT] === true), 'probes went to a method without its this');
  canaries.stopAuto();
});

test('the check\'s own promise object comes back, and checks register once and at most 32', async () => {
  class Special extends Promise {
    extra() {
      return 'kept';
    }
  }
  const check = tellproof.guardrailCheck(() => Special.resolve(true), { canaries: false });
  await tellproof.trace(async () => {
    const r = check(MARKER);
    assert.equal(r.extra(), 'kept');
    assert.equal(await r, true);
    await new Promise((res) => setImmediate(res));
    await chat();
  });
  assert.deepEqual(decisions(), ['block']);
  const fn = () => false;
  tellproof.guardrailCheck(fn);
  tellproof.guardrailCheck(fn);
  assert.equal(core.checks.filter((c) => c.fn === fn).length, 1);
  for (let i = 0; i < 40; i++) tellproof.guardrailCheck(() => false); // a check wrapped per call: the leak this guards against
  assert.equal(core.checks.length, core.MAX_CHECKS);
});

test('OpenAI moderation, Bedrock ApplyGuardrail and Azure content filters are guardrails', async () => {
  const c = openai();
  await tellproof.trace(async () => {
    await c.moderations.create({ input: MARKER });
    await chat(c);
  });
  await tellproof.trace(async () => {
    await c.moderations.create({ input: 'FLAG ' + MARKER });
    await chat(c);
  });
  const br = new lib.bedrock.BedrockRuntimeClient();
  await tellproof.trace(async () => {
    await br.send(new lib.bedrock.ApplyGuardrailCommand({ guardrailIdentifier: 'g', guardrailVersion: '1', source: 'INPUT', content: [{ text: { text: MARKER } }] }));
    await br.send(new lib.bedrock.ConverseCommand({ modelId: 'anthropic.claude-sonnet-4-6', messages: [] }));
  });
  await tellproof.trace(() => br.send(new lib.bedrock.ApplyGuardrailCommand({ guardrailIdentifier: 'g', guardrailVersion: '1', source: 'INPUT', content: [], _intervene: true })));
  const az = new lib.openai.AzureOpenAI();
  await chat(az);
  await chat(az, 'filtered');
  await assert.rejects(chat(az, 'refused'));
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(decisions(), ['allow', 'flag', 'allow', 'block', 'allow', 'block', 'block']);
  noContent();
});

test('an OpenAI stored prompt is named by reference', async () => {
  const c = openai();
  await c.responses.create({ model: 'gpt-4.1', input: MARKER, prompt: { id: 'pmpt_123', version: '4', variables: { x: MARKER } } });
  await c.responses.create({ model: 'gpt-4.1', input: MARKER });
  const [a, b] = llm();
  assert.deepEqual([a.attrs[core.A.PROMPT_NAME], a.attrs[core.A.PROMPT_VERSION]], ['pmpt_123', '4']);
  assert.equal(b.attrs[core.A.PROMPT_NAME], undefined);
  noContent();
});

test('every request to a Node.js HTTP server is its own scope', async () => {
  const check = tellproof.guardrailCheck((t) => (t.includes('FLAG') ? 'flag' : false), { canaries: false });
  const redact = tellproof.redactor((t) => t);
  core.configure({ personalData: true });
  let n = 0;
  const srv = http.createServer(async (req, res) => {
    n++;
    if (n % 2) check(n === 3 ? 'FLAG' : MARKER); // odd requests run the check
    if (n === 1) redact(MARKER);
    await new Promise((r) => setTimeout(r, 5)); // async work keeps the request's context
    await chat();
    res.end('ok');
  });
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  try {
    for (let i = 0; i < 4; i++) {
      const r = await fetch(`http://127.0.0.1:${srv.address().port}/`);
      await r.text();
    }
  } finally {
    srv.close();
  }
  for (let i = 0; i < 50 && llm().length < 4; i++) await new Promise((r) => setTimeout(r, 10));
  assert.deepEqual(decisions(), ['allow', null, 'flag', null]);
  assert.deepEqual(llm().map((s) => s.attrs[core.A.REDACTED]), [true, false, false, false]);
  assert.equal(new Set(llm().map((s) => s.traceId)).size, 4, 'requests shared a trace');
  noContent();
});

test('init options and env', async () => {
  const bodies = [];
  const saved = { ...process.env };
  Object.assign(process.env, { TELLPROOF_PROMPT: 'claims', TELLPROOF_PROMPT_VERSION: '7', TELLPROOF_PERSONAL_DATA: '1' });
  try {
    await tellproof.shutdown();
    assert.equal(tellproof.init({ service: 't', agent: 'triage', transport: (b) => bodies.push(b), canaries: false, instrument: false }), true);
    await tellproof.trace(() => tellproof.llmCall({ provider: 'acme', model: 'm1' }, (c) => c.usage(1, 1)));
    assert.equal(await tellproof.flush(5000), true);
    assert.equal(tellproof.stats().unscoped, 0);
  } finally {
    await tellproof.shutdown();
    process.env = saved;
    core.configure();
  }
  const span = JSON.parse(bodies[0]).resourceSpans[0].scopeSpans[0].spans[0];
  const attrs = Object.fromEntries(span.attributes.map((a) => [a.key, Object.values(a.value)[0]]));
  assert.deepEqual(
    [attrs[core.A.AGENT_NAME], attrs[core.A.PROMPT_NAME], attrs[core.A.PROMPT_VERSION], attrs[core.A.DATA_CLASS], attrs[core.A.REDACTED]],
    ['triage', 'claims', '7', 'personal', false],
  );
});
