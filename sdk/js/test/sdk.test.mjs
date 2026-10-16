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
const plumbline = require('../lib/index.cjs');
const core = require('../lib/core.cjs');
const otlp = require('../lib/otlp.cjs');
const { Exporter } = require('../lib/export.cjs');

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
  assert.equal(esm.init, plumbline.init);
  assert.equal(esm.default, plumbline);
});

test('scopes stamp spans and nest', async () => {
  const spans = await spansOf(() =>
    plumbline.agentRun('helper', () =>
      plumbline.prompt('reply', 7, () =>
        plumbline.personalData({ redacted: true }, () =>
          plumbline.llmCall({ provider: 'openai', model: 'gpt-4.1', region: 'eu-west-1' }, (c) => {
            c.usage(10, 3);
            c.responseModel = 'gpt-4.1-2026-04-14';
          }),
        ),
      ),
    ),
  );
  const [llm, run] = spans;
  assert.equal(attrs(llm)['plumbline.prompt.version'], '7');
  assert.equal(attrs(llm)['gen_ai.agent.name'], 'helper');
  assert.equal(attrs(llm)['gen_ai.response.model'], 'gpt-4.1-2026-04-14');
  assert.equal(attrs(llm)['plumbline.redaction.applied'], true);
  assert.equal(llm.parent, run.spanId);
  assert.equal(llm.traceId, run.traceId);
  assert.equal(attrs(run)['gen_ai.operation.name'], 'invoke_agent');
});

test('spans outside scopes get their own traces', async () => {
  const spans = await spansOf(() => {
    for (let i = 0; i < 3; i++) plumbline.llmCall({ provider: 'openai', model: 'm' }, () => {});
  });
  assert.equal(new Set(spans.map((s) => s.traceId)).size, 3);
});

test('a guardrail decided after the call', async () => {
  const spans = await spansOf(() =>
    plumbline.guardrail((g) => {
      plumbline.llmCall({ provider: 'openai', model: 'm' }, () => {});
      assert.equal(core.currentClient().stats.spans, 0, 'the call waits for the decision');
      g.flag();
    }),
  );
  assert.equal(attrs(spans[0])['plumbline.guardrail.decision'], 'flag');
});

test('an inner guardrail decision wins and an outer one fills the rest', async () => {
  const spans = await spansOf(() =>
    plumbline.guardrail('allow', () => {
      plumbline.guardrail('block', () => plumbline.llmCall({ provider: 'openai', model: 'inner' }, () => {}));
      plumbline.guardrail(() => plumbline.llmCall({ provider: 'openai', model: 'undecided' }, () => {}));
    }),
  );
  const got = Object.fromEntries(spans.map((s) => [attrs(s)['gen_ai.request.model'], attrs(s)['plumbline.guardrail.decision']]));
  assert.deepEqual(got, { inner: 'block', undecided: 'allow' });
});

test('a block without a call is one blocked call', async () => {
  const spans = await spansOf(() => {
    plumbline.guardrail({ decision: 'block', provider: 'anthropic', model: 'claude-haiku-4-5' }, () => {});
    plumbline.guardrail('allow', () => {}); // allowed, nothing called: nothing to record
  });
  assert.equal(spans.length, 1);
  const a = attrs(spans[0]);
  assert.deepEqual([a['gen_ai.provider.name'], a['gen_ai.request.model'], a['plumbline.guardrail.decision']], [
    'anthropic',
    'claude-haiku-4-5',
    'block',
  ]);
});

test('an invalid decision is ignored, not thrown', async () => {
  let errors;
  const spans = await spansOf(() => {
    plumbline.guardrail('maybe', () => plumbline.llmCall({ provider: 'openai', model: 'm' }, () => {}));
    errors = core.currentClient().stats.instrumentationErrors;
  });
  assert.ok(!('plumbline.guardrail.decision' in attrs(spans[0])));
  assert.equal(errors, 1);
});

test('tool errors and approval; the app error passes through', async () => {
  const boom = new Error('x');
  const spans = await spansOf(() => {
    assert.throws(
      () =>
        plumbline.tool('refund', { highImpact: true }, (t) => {
          t.approved = true;
          throw boom;
        }),
      (e) => e === boom,
    );
  });
  assert.equal(spans[0].error, true);
  assert.deepEqual([attrs(spans[0])['plumbline.tool.high_impact'], attrs(spans[0])['plumbline.tool.approved']], [true, true]);
});

test('async scopes end when their promise settles and keep the value', async () => {
  const spans = await spansOf(async () => {
    const v = await plumbline.agentRun('async-agent', async () => {
      await new Promise((r) => setTimeout(r, 5));
      await plumbline.llmCall({ provider: 'openai', model: 'm' }, async () => {
        await new Promise((r) => setImmediate(r));
      });
      return 42;
    });
    assert.equal(v, 42);
    await assert.rejects(plumbline.tool('t', async () => Promise.reject(new RangeError('r'))), RangeError);
  });
  assert.deepEqual(spans.map((s) => s.name), ['chat', 'invoke_agent', 'execute_tool']);
  assert.equal(spans[0].traceId, spans[1].traceId);
  assert.equal(spans[2].error, true);
});

test('concurrent requests keep their own context', async () => {
  const spans = await spansOf(() =>
    Promise.all(
      [0, 1, 2].map((n) =>
        plumbline.prompt('p', n, async () => {
          await new Promise((r) => setTimeout(r, 10 * (3 - n)));
          plumbline.llmCall({ provider: 'openai', model: 'm' }, () => {});
        }),
      ),
    ),
  );
  assert.deepEqual(spans.map((s) => attrs(s)['plumbline.prompt.version']).sort(), ['0', '1', '2']);
  assert.equal(new Set(spans.map((s) => s.traceId)).size, 3);
});

test('values are short names only', async () => {
  const spans = await spansOf(() =>
    plumbline.prompt('x'.repeat(1000), { not: 'a version' }, () => plumbline.llmCall({ provider: 'openai', model: 'm' }, () => {})),
  );
  assert.equal(attrs(spans[0])['plumbline.prompt.name'].length, core.MAX_VALUE);
  assert.ok(!('plumbline.prompt.version' in attrs(spans[0])));
});

test('usage ignores bad counts', async () => {
  const spans = await spansOf(() =>
    plumbline.llmCall({ provider: 'openai', model: 'm' }, (c) => {
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
    const r = plumbline.agentRun('a', () =>
      plumbline.guardrail('block', (g) =>
        plumbline.tool('t', (t) => {
          t.approved = true;
          g.allow();
          plumbline.escalation();
          return 'ran';
        }),
      ),
    );
    assert.equal(r, 'ran');
    assert.equal(plumbline.isEnabled(), false);
    assert.equal(plumbline.trace('not a function'), undefined); // misuse never throws
  } finally {
    core.setClient(old);
  }
});

test('init respects PLUMBLINE_DISABLED', () => {
  process.env.PLUMBLINE_DISABLED = '1';
  try {
    assert.equal(plumbline.init({ transport: () => {} }), false);
    assert.equal(plumbline.isEnabled(), false);
  } finally {
    delete process.env.PLUMBLINE_DISABLED;
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
        plumbline.tool('t', { highImpact: rnd() < 0.5 }, () => {
          if (rnd() < 0.3) throw new Error(message);
          return message + note;
        });
      try {
        await plumbline.agentRun('fuzz', () =>
          plumbline.guardrail(async (g) => {
            await plumbline.llmCall({ provider: 'openai', model: 'm' }, async (c) => {
              c.usage(marker.length, 1);
              act(marker, marker);
            });
            g.decision = ['allow', 'block', 'flag', marker][Math.floor(rnd() * 4)];
            plumbline.escalation();
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
    assert.equal(plumbline.init({ agentUrl: `http://127.0.0.1:${srv.address().port}/`, service: 'e2e', instrument: false }), true);
    assert.equal(plumbline.init(), true); // idempotent
    plumbline.llmCall({ provider: 'openai', model: 'gpt-4.1-mini' }, (c) => c.usage(5, 1));
    assert.equal(await plumbline.flush(5000), true);
    const st = plumbline.stats();
    assert.deepEqual([st.exported, st.dropped], [1, 0]);
  } finally {
    await plumbline.shutdown();
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
  assert.equal(plumbline.isEnabled(), false);
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
