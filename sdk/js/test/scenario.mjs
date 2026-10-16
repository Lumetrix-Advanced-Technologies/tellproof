// The shared SDK scenario (sdk/testdata/README.md), the twin of
// sdk/python/tests/scenario.py: the same app must aggregate in the Go agent to
// sdk/testdata/expected-windows.json.
//
// node sdk/js/test/scenario.mjs   rewrites sdk/testdata/js-export.jsonl

import { writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const core = require('../lib/core.cjs');
const otlp = require('../lib/otlp.cjs');
const plumbline = require('../lib/index.cjs');

export const FIXTURE = join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'testdata', 'js-export.jsonl');
const RESOURCE = {
  'service.name': 'claims-api',
  'telemetry.sdk.name': 'plumbline',
  'telemetry.sdk.language': 'nodejs',
  'telemetry.sdk.version': 'scenario',
};

export class Clock {
  constructor(iso) {
    this.at(iso);
  }
  at(iso) {
    this.ns = BigInt(Date.parse(iso + 'Z')) * 1000000n;
  }
  read() {
    this.ns += 250000000n; // every reading moves 250 ms
    return this.ns;
  }
}

export function ids() {
  let n = 0;
  return (bytes) => (++n).toString(16).padStart(bytes * 2, '0');
}

export class Capture {
  constructor() {
    this.spans = [];
  }
  offer(span) {
    this.spans.push(span);
  }
}

// The app. Hour 10:00: four requests; hour 11:00: one.
export function run(clock) {
  // 1. A triage call on personal data, redacted, allowed by the guardrail.
  plumbline.trace(() =>
    plumbline.prompt('triage', '14', () =>
      plumbline.personalData({ redacted: true }, () =>
        plumbline.guardrail('allow', () =>
          plumbline.llmCall({ provider: 'openai', model: 'gpt-4.1-mini', region: 'us-east-1' }, (c) => c.usage(1200, 80)),
        ),
      ),
    ),
  );

  // 2. An agent run: a model call the output guardrail then blocks, three tool
  //    calls (one high-impact approved, one high-impact unapproved that fails)
  //    and an escalation to a person.
  plumbline.agentRun('claims-agent', () => {
    plumbline.guardrail((g) => {
      plumbline.llmCall({ provider: 'anthropic', model: 'claude-sonnet-4-5', region: 'ap-southeast-2' }, (c) => c.usage(3000, 400));
      g.block();
    });
    plumbline.tool('lookup_policy', () => {});
    plumbline.tool('issue_refund', { highImpact: true }, (t) => {
      t.approved = true;
    });
    try {
      plumbline.tool('close_account', { highImpact: true }, () => {
        throw new Error('not approved');
      });
    } catch {}
    plumbline.escalation();
  });

  // 3. The input guardrail blocks before any model call.
  plumbline.trace(() =>
    plumbline.personalData({ redacted: false }, () =>
      plumbline.guardrail({ decision: 'block', provider: 'openai', model: 'gpt-4.1-mini' }, () => {}),
    ),
  );

  // 4. A failed embeddings call outside any scope.
  try {
    plumbline.llmCall({ provider: 'openai', model: 'text-embedding-3-small', operation: 'embeddings', region: 'us-east-1' }, () => {
      throw new Error('timeout');
    });
  } catch {}

  // 5. The next hour: the triage prompt moves to version 15 on another model.
  clock.at('2026-09-01T11:20:00');
  plumbline.prompt('triage', '15', () =>
    plumbline.guardrail('flag', () =>
      plumbline.llmCall({ provider: 'anthropic', model: 'claude-sonnet-4-5', region: 'ap-southeast-2' }, (c) => c.usage(900, 60)),
    ),
  );
}

export function exportLines() {
  const clock = new Clock('2026-09-01T10:15:00');
  const cap = new Capture();
  const old = core.currentClient();
  core.setClient(new core.Client(cap, { service: 'claims-api', clock: () => clock.read(), ids: ids() }));
  try {
    run(clock);
  } finally {
    core.setClient(old);
  }
  const hours = new Map(); // one export request per hour, as the exporter would batch them
  for (const s of cap.spans) {
    const h = s.start / 3600000000000n;
    if (!hours.has(h)) hours.set(h, []);
    hours.get(h).push(s);
  }
  return [...hours.keys()].sort((a, b) => (a < b ? -1 : 1)).map((h) => otlp.encode(hours.get(h), RESOURCE, 'scenario'));
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  writeFileSync(FIXTURE, exportLines().join('\n') + '\n');
  console.log('wrote sdk/testdata/js-export.jsonl');
}
