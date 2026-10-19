// node --test sdk/js/test/   (no packages)
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';
import * as esm from '../lib/index.mjs';
import { FIXTURE, Capture, exportLines } from './scenario.mjs';

const require = createRequire(import.meta.url);
const tellproof = require('../lib/index.cjs');
const core = require('../lib/core.cjs');
const otlp = require('../lib/otlp.cjs');
const { Exporter, httpTransport } = require('../lib/export.cjs');

// Run fn (sync or async) with a client that keeps spans in memory.
async function spansOf(fn) {
  const cap = new Capture();
  const old = core.currentClient();
  core.setClient(new core.Client(cap, { service: 'test' }));
  try {
    await fn();
  } finally {
    core.setClient(old);
  }
  return cap.spans;
}

const attrs = (s) => s.attrs;

test('the committed fixture is current', () => {
  assert.equal(readFileSync(FIXTURE, 'utf8'), exportLines().join('\n') + '\n', 'run node sdk/js/test/scenario.mjs');
});

test('import and require share one SDK instance', () => {
  assert.equal(esm.init, tellproof.init);
  assert.equal(esm.default, tellproof);
});

test('scopes stamp spans and nest', async () => {
  const spans = await spansOf(() =>
    tellproof.agentRun('helper', () =>
      tellproof.prompt('reply', 7, () =>
        tellproof.personalData({ redacted: true }, () =>
          tellproof.llmCall({ provider: 'openai', model: 'gpt-4.1', region: 'eu-west-1' }, (c) => {
            c.usage(10, 3);
            c.responseModel = 'gpt-4.1-2026-04-14';
          }),
        ),
      ),
    ),
  );
  const [llm, run] = spans;
  assert.equal(attrs(llm)['tellproof.prompt.version'], '7');
  assert.equal(attrs(llm)['gen_ai.agent.name'], 'helper');
  assert.equal(attrs(llm)['gen_ai.response.model'], 'gpt-4.1-2026-04-14');
  assert.equal(attrs(llm)['tellproof.redaction.applied'], true);
  assert.equal(llm.parent, run.spanId);
  assert.equal(llm.traceId, run.traceId);
  assert.equal(attrs(run)['gen_ai.operation.name'], 'invoke_agent');
});

test('spans outside scopes get their own traces', async () => {
  const spans = await spansOf(() => {
    for (let i = 0; i < 3; i++) tellproof.llmCall({ provider: 'openai', model: 'm' }, () => {});
  });
  assert.equal(new Set(spans.map((s) => s.traceId)).size, 3);
});

test('a guardrail decided after the call', async () => {
  const spans = await spansOf(() =>
    tellproof.guardrail((g) => {
      tellproof.llmCall({ provider: 'openai', model: 'm' }, () => {});
      assert.equal(core.currentClient().stats.spans, 0, 'the call waits for the decision');
      g.flag();
    }),
  );
  assert.equal(attrs(spans[0])['tellproof.guardrail.decision'], 'flag');
});

test('an inner guardrail decision wins and an outer one fills the rest', async () => {
  const spans = await spansOf(() =>
    tellproof.guardrail('allow', () => {
      tellproof.guardrail('block', () => tellproof.llmCall({ provider: 'openai', model: 'inner' }, () => {}));
      tellproof.guardrail(() => tellproof.llmCall({ provider: 'openai', model: 'undecided' }, () => {}));
    }),
  );
  const got = Object.fromEntries(spans.map((s) => [attrs(s)['gen_ai.request.model'], attrs(s)['tellproof.guardrail.decision']]));
  assert.deepEqual(got, { inner: 'block', undecided: 'allow' });
});

test('a block without a call is one blocked call', async () => {
  const spans = await spansOf(() => {
    tellproof.guardrail({ decision: 'block', provider: 'anthropic', model: 'claude-haiku-4-5' }, () => {});
    tellproof.guardrail('allow', () => {}); // allowed, nothing called: nothing to record
  });
  assert.equal(spans.length, 1);
  const a = attrs(spans[0]);
  assert.deepEqual([a['gen_ai.provider.name'], a['gen_ai.request.model'], a['tellproof.guardrail.decision']], [
    'anthropic',
    'claude-haiku-4-5',
    'block',
  ]);
});

test('an invalid decision is ignored, not thrown', async () => {
  let errors;
  const spans = await spansOf(() => {
    tellproof.guardrail('maybe', () => tellproof.llmCall({ provider: 'openai', model: 'm' }, () => {}));
    errors = core.currentClient().stats.instrumentationErrors;
  });
  assert.ok(!('tellproof.guardrail.decision' in attrs(spans[0])));
  assert.equal(errors, 1);
});

test('tool errors and approval; the app error passes through', async () => {
  const boom = new Error('x');
  const spans = await spansOf(() => {
    assert.throws(
      () =>
        tellproof.tool('refund', { highImpact: true }, (t) => {
          t.approved = true;
          throw boom;
        }),
      (e) => e === boom,
    );
  });
  assert.equal(spans[0].error, true);
  assert.deepEqual([attrs(spans[0])['tellproof.tool.high_impact'], attrs(spans[0])['tellproof.tool.approved']], [true, true]);
});

test('async scopes end when their promise settles and keep the value', async () => {
  const spans = await spansOf(async () => {
    const v = await tellproof.agentRun('async-agent', async () => {
      await new Promise((r) => setTimeout(r, 5));
      await tellproof.llmCall({ provider: 'openai', model: 'm' }, async () => {
        await new Promise((r) => setImmediate(r));
      });
      return 42;
    });
    assert.equal(v, 42);
    await assert.rejects(tellproof.tool('t', async () => Promise.reject(new RangeError('r'))), RangeError);
  });
  assert.deepEqual(spans.map((s) => s.name), ['chat', 'invoke_agent', 'execute_tool']);
  assert.equal(spans[0].traceId, spans[1].traceId);
  assert.equal(spans[2].error, true);
});

test('concurrent requests keep their own context', async () => {
  const spans = await spansOf(() =>
    Promise.all(
      [0, 1, 2].map((n) =>
        tellproof.prompt('p', n, async () => {
          await new Promise((r) => setTimeout(r, 10 * (3 - n)));
          tellproof.llmCall({ provider: 'openai', model: 'm' }, () => {});
        }),
      ),
    ),
  );
  assert.deepEqual(spans.map((s) => attrs(s)['tellproof.prompt.version']).sort(), ['0', '1', '2']);
  assert.equal(new Set(spans.map((s) => s.traceId)).size, 3);
});

test('values are short names only', async () => {
  const spans = await spansOf(() =>
    tellproof.prompt('x'.repeat(1000), { not: 'a version' }, () => tellproof.llmCall({ provider: 'openai', model: 'm' }, () => {})),
  );
  assert.equal(attrs(spans[0])['tellproof.prompt.name'].length, core.MAX_VALUE);
  assert.ok(!('tellproof.prompt.version' in attrs(spans[0])));
});

test('usage ignores bad counts', async () => {
  const spans = await spansOf(() =>
    tellproof.llmCall({ provider: 'openai', model: 'm' }, (c) => {
      c.usage(-1, '12');
      c.usage(1.5, true);
    }),
  );
  assert.ok(!('gen_ai.usage.input_tokens' in attrs(spans[0])));
  assert.ok(!('gen_ai.usage.output_tokens' in attrs(spans[0])));
});

test('disabled is a no-op that still runs the app', () => {
  const old = core.currentClient();
  core.setClient(new core.Client());
  try {
    const r = tellproof.agentRun('a', () =>
      tellproof.guardrail('block', (g) =>
        tellproof.tool('t', (t) => {
          t.approved = true;
          g.allow();
          tellproof.escalation();
          return 'ran';
        }),
      ),
    );
    assert.equal(r, 'ran');
    assert.equal(tellproof.isEnabled(), false);
    assert.equal(tellproof.trace('not a function'), undefined); // misuse never throws
  } finally {
    core.setClient(old);
  }
});

test('init respects TELLPROOF_DISABLED', () => {
  process.env.TELLPROOF_DISABLED = '1';
  try {
    assert.equal(tellproof.init({ transport: () => {} }), false);
    assert.equal(tellproof.isEnabled(), false);
  } finally {
    delete process.env.TELLPROOF_DISABLED;
  }
});

// Acceptance (S-3, as S-1): no argument text reaches an exported span.
test('random text never reaches an export', async () => {
  let seed = 42;
  const rnd = () => ((seed = (seed * 1103515245 + 12345) % 2 ** 31) / 2 ** 31);
  const chars = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 "\'\\\n{}';
  const exports = [];
  const exp = new Exporter((b) => exports.push(b), (spans) => otlp.encode(spans, { 'service.name': 'fuzz' }, 't'), { intervalMs: 5 });
  const old = core.currentClient();
  core.setClient(new core.Client(exp, { service: 'fuzz' }));
  const markers = [];
  try {
    for (let i = 0; i < 300; i++) {
      let marker = 'SECRET';
      for (let j = 0; j < 24; j++) marker += chars[Math.floor(rnd() * chars.length)];
      markers.push(marker);
      const act = (message, note) =>
        tellproof.tool('t', { highImpact: rnd() < 0.5 }, () => {
          if (rnd() < 0.3) throw new Error(message);
          return message + note;
        });
      try {
        await tellproof.agentRun('fuzz', () =>
          tellproof.guardrail(async (g) => {
            await tellproof.llmCall({ provider: 'openai', model: 'm' }, async (c) => {
              c.usage(marker.length, 1);
              act(marker, marker);
            });
            g.decision = ['allow', 'block', 'flag', marker][Math.floor(rnd() * 4)];
            tellproof.escalation();
          }),
        );
      } catch {}
    }
    assert.equal(await exp.flush(5000), true);
  } finally {
    core.setClient(old);
    await exp.shutdown(1000);
  }
  const blob = exports.join('');
  assert.ok(blob.split('"spanId"').length - 1 > 900);
  for (const m of markers) assert.ok(!blob.includes(m.slice(0, 12)), `argument text ${JSON.stringify(m.slice(0, 12))} reached an export`);
});

test('overload drops instead of blocking', async () => {
  let release;
  const stalled = new Promise((r) => (release = r));
  const exp = new Exporter(() => stalled, () => 'x', { maxQueue: 100, batchSize: 10, intervalMs: 1 });
  const t0 = performance.now();
  for (let i = 0; i < 10000; i++) exp.offer({ n: i });
  await new Promise((r) => setTimeout(r, 20)); // the first batch is now in flight
  for (let i = 0; i < 10000; i++) exp.offer({ n: i });
  const elapsed = performance.now() - t0;
  release();
  assert.ok(elapsed < 1000, `offer() took ${elapsed} ms`);
  assert.ok(exp.dropped >= 20000 - 2 * 100 - 10, `dropped ${exp.dropped}`);
  await exp.shutdown(1000);
});

test('a failing agent is counted, not thrown', async () => {
  const exp = new Exporter(() => {
    throw new Error('ECONNREFUSED');
  }, () => 'x', { intervalMs: 1 });
  for (let i = 0; i < 5; i++) exp.offer({ n: i });
  await exp.flush(1000);
  assert.deepEqual([exp.exportErrors, exp.dropped, exp.exported], [1, 5, 0]);
});

test('flush sends everything in batches and shutdown stops', async () => {
  const got = [];
  const exp = new Exporter(async (b) => got.push(JSON.parse(b)), (s) => JSON.stringify(s), { batchSize: 7, intervalMs: 10000 });
  for (let i = 0; i < 50; i++) exp.offer({ n: i });
  assert.equal(await exp.flush(5000), true);
  assert.equal(got.flat().length, 50);
  assert.ok(got.every((b) => b.length <= 7));
  await exp.shutdown(1000);
  exp.offer({ n: 99 });
  assert.equal(exp.dropped, 1);
});

test('init sends OTLP JSON to the agent URL', async () => {
  const received = [];
  const srv = createServer((req, res) => {
    let body = '';
    req.on('data', (d) => (body += d));
    req.on('end', () => {
      received.push([req.url, req.headers['content-type'], JSON.parse(body)]);
      res.end('{}');
    });
  });
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  try {
    assert.equal(tellproof.init({ agentUrl: `http://127.0.0.1:${srv.address().port}/`, service: 'e2e', instrument: false }), true);
    assert.equal(tellproof.init(), true); // idempotent
    tellproof.llmCall({ provider: 'openai', model: 'gpt-4.1-mini' }, (c) => c.usage(5, 1));
    assert.equal(await tellproof.flush(5000), true);
    const st = tellproof.stats();
    assert.deepEqual([st.exported, st.dropped], [1, 0]);
  } finally {
    await tellproof.shutdown();
    srv.close();
  }
  const [path, ctype, body] = received[0];
  assert.deepEqual([path, ctype], ['/v1/traces', 'application/json']);
  const rs = body.resourceSpans[0];
  assert.deepEqual(
    rs.resource.attributes.find((a) => a.key === 'service.name'),
    { key: 'service.name', value: { stringValue: 'e2e' } },
  );
  assert.equal(rs.scopeSpans[0].spans.length, 1);
  assert.equal(tellproof.isEnabled(), false);
});

test('a script that awaits flush() is not cut short by the unref\'d timers', () => {
  // Nothing else keeps this child's event loop alive: if flush() waited only
  // on unref'd timers, the process would exit before printing.
  const exportPath = fileURLToPath(new URL('../lib/export.cjs', import.meta.url));
  const script = `
    const { Exporter } = require(${JSON.stringify(exportPath)});
    const exp = new Exporter(() => new Promise((r) => setTimeout(r, 30)), () => 'x', { intervalMs: 60000 });
    exp.offer({});
    exp.flush(5000).then((ok) => console.log('flushed', ok, exp.exported));`;
  const out = execFileSync(process.execPath, ['-e', script], { encoding: 'utf8', timeout: 10000 });
  assert.equal(out.trim(), 'flushed true 1');
});

// Guardrail canaries (D-057): only the kind and the result leave the probe.
test('canary probes are spec/canaries.json', () => {
  const spec = JSON.parse(readFileSync(new URL('../../../spec/canaries.json', import.meta.url), 'utf8'));
  assert.deepEqual({ ...tellproof.PROBES }, Object.fromEntries(spec.probes.map((p) => [p.kind, p.text])));
  assert.equal(esm.runCanaries, tellproof.runCanaries);
});

test('runCanaries records the kind and the result only', async () => {
  const seen = [];
  let got;
  const spans = await spansOf(() =>
    tellproof.personalData(async () => {
      got = await tellproof.runCanaries(async (text) => {
        seen.push(text);
        if (text.includes('AKIA')) throw new Error('guardrail crashed'); // counts as missed
        return text.includes('instructions') ? 'block' : text.includes('4111') ? true : 'allow';
      });
    }),
  );
  assert.deepEqual(got, { prompt_injection: true, jailbreak: false, pii: true, secret: false });
  assert.deepEqual(seen, Object.values(tellproof.PROBES));
  assert.equal(spans.length, 4);
  for (const s of spans) {
    assert.deepEqual(Object.keys(attrs(s)).sort(), ['tellproof.canary.caught', 'tellproof.canary.kind']);
    assert.equal(s.parent, null);
  }
  assert.equal(new Set(spans.map((s) => s.traceId)).size, 4);
  const dumped = JSON.stringify(spans, (k, v) => (typeof v === 'bigint' ? String(v) : v));
  for (const text of Object.values(tellproof.PROBES)) assert.ok(!dumped.includes(text.slice(0, 20)));
});

test('canary() and kinds', async () => {
  let got;
  const spans = await spansOf(async () => {
    tellproof.canary('Prompt_Injection', true);
    tellproof.canary('custom-check_2', 'allow');
    tellproof.canary('has space', true);
    tellproof.canary(null, true);
    got = await tellproof.runCanaries(() => true, { kinds: ['pii', 'unknown'] });
  });
  assert.deepEqual(got, { pii: true });
  assert.deepEqual(
    spans.map((s) => [attrs(s)['tellproof.canary.kind'], attrs(s)['tellproof.canary.caught']]),
    [['prompt_injection', true], ['custom-check_2', false], ['pii', true]],
  );
});

test('canaries do not call the guardrail when the SDK is off; schedules unref and stop', async () => {
  const calls = [];
  const old = core.currentClient();
  core.setClient(new core.Client());
  try {
    assert.deepEqual(await tellproof.runCanaries((t) => calls.push(t)), {});
  } finally {
    core.setClient(old);
  }
  assert.deepEqual(calls, []);
  let ran = 0;
  const spans = await spansOf(async () => {
    const s = tellproof.scheduleCanaries(() => { ran++; return true; }, { every: 0, kinds: ['pii'] });
    assert.equal(s.every, 60_000);
    await new Promise((r) => setTimeout(r, 20));
    s.stop();
  });
  assert.equal(ran, 1);
  assert.equal(spans.length, 1);
});

test('the transport never follows a redirect, ignores the answer body, and takes http(s) only', async () => {
  const hits = { target: 0 };
  const target = createServer((req, res) => { hits.target++; res.end('{}'); });
  await new Promise((r) => target.listen(0, '127.0.0.1', r));
  const redirector = createServer((req, res) => {
    req.resume();
    res.writeHead(307, { Location: `http://127.0.0.1:${target.address().port}/v1/traces` }).end();
  });
  await new Promise((r) => redirector.listen(0, '127.0.0.1', r));
  const big = createServer((req, res) => { req.resume(); res.end('x'.repeat(8 << 20)); });
  await new Promise((r) => big.listen(0, '127.0.0.1', r));
  try {
    await assert.rejects(httpTransport(`http://127.0.0.1:${redirector.address().port}/v1/traces`)('{}'));
    assert.equal(hits.target, 0, 'the spans followed a redirect');
    await httpTransport(`http://127.0.0.1:${big.address().port}/v1/traces`)('{}');
  } finally {
    target.close(); redirector.close(); big.close();
  }
  for (const u of ['file:///etc/passwd', 'ftp://127.0.0.1/', 'javascript:alert(1)']) {
    assert.throws(() => httpTransport(u), /http/);
  }
  assert.equal(tellproof.init({ agentUrl: 'file:///tmp/x', instrument: false }), false);
  assert.equal(tellproof.isEnabled(), false);
});
