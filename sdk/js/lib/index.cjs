'use strict';
// Plumbline SDK: one line to send evidence telemetry to the local Plumbline agent.
//
//   const plumbline = require('plumbline-sdk');   // or: import * as plumbline from 'plumbline-sdk'
//   plumbline.init();
//
// Spans go as OTLP JSON to the agent (http://127.0.0.1:4318, or
// PLUMBLINE_AGENT_URL). They carry provider, model, region, token counts,
// operation, timing, error status and the helper attributes; never prompts,
// completions, function arguments or error messages. No dependencies; never
// blocks a request (a full buffer drops spans and counts them) and never throws
// into the app. PLUMBLINE_DISABLED=1 turns it off.

const path = require('node:path');
const core = require('./core.cjs');
const otlp = require('./otlp.cjs');
const { Exporter, httpTransport } = require('./export.cjs');

const VERSION = '0.1.0';
const DEFAULT_AGENT_URL = 'http://127.0.0.1:4318';
let exitHook = false;
let instrumented = [];

function resource(service, environment) {
  let name = service || process.env.PLUMBLINE_SERVICE || process.env.OTEL_SERVICE_NAME;
  if (!name) name = `unknown_service:${path.basename(process.argv[1] || process.argv0 || 'node')}`;
  const r = {
    'service.name': String(name).slice(0, core.MAX_VALUE),
    'telemetry.sdk.name': 'plumbline',
    'telemetry.sdk.language': 'nodejs',
    'telemetry.sdk.version': VERSION,
  };
  const env = environment || process.env.PLUMBLINE_ENVIRONMENT;
  if (env) r['deployment.environment.name'] = String(env).slice(0, core.MAX_VALUE);
  return r;
}

function disabledByEnv() {
  return ['1', 'true', 'yes'].includes(String(process.env.PLUMBLINE_DISABLED || '').trim().toLowerCase());
}

// Start the SDK. Safe to call more than once; returns whether it is on.
function init(opts = {}) {
  if (core.currentClient().enabled) return true;
  if (disabledByEnv()) return false;
  try {
    const o = opts || {};
    const url = String(o.agentUrl || process.env.PLUMBLINE_AGENT_URL || DEFAULT_AGENT_URL).replace(/\/+$/, '');
    const res = resource(o.service, o.environment);
    const exporter = new Exporter(o.transport || httpTransport(url + '/v1/traces'), (spans) => otlp.encode(spans, res, VERSION));
    core.setClient(new core.Client(exporter, { service: res['service.name'] }));
    core.setDebug(o.debug);
    instrumented = [];
    if (o.instrument !== false) {
      try {
        const ins = require('./instrument.cjs');
        instrumented = ins.install({ streamUsage: !!o.streamUsage });
        ins.ready().then((list) => {
          instrumented = [...new Set([...instrumented, ...list])];
        });
      } catch (e) {
        core.fail('instrument', e);
      }
    }
    if (!exitHook) {
      // Flush once when the event loop drains. Short-lived scripts and serverless
      // handlers should await plumbline.flush() themselves.
      exitHook = true;
      process.once('beforeExit', () => {
        flush().catch(() => {});
      });
    }
    return true;
  } catch (e) {
    return false;
  }
}

// Send buffered spans now; resolves true when everything was handed to the agent.
async function flush(timeoutMs = 2000) {
  const c = core.currentClient();
  if (!c.exporter) return true;
  try {
    core.runLater();
    return await c.exporter.flush(timeoutMs);
  } catch {
    return false;
  }
}

// Flush and stop; later spans are not recorded.
async function shutdown(timeoutMs = 2000) {
  const c = core.currentClient();
  core.setClient(new core.Client());
  if (!c.exporter) return true;
  try {
    return await c.exporter.shutdown(timeoutMs);
  } catch {
    return false;
  }
}

// Resolves once the ES module copies of the client libraries are instrumented
// too (CommonJS copies are instrumented inside init()). Await it in an ES
// module that calls a model right after init().
async function ready() {
  try {
    await require('./instrument.cjs').ready();
  } catch {}
  return stats().instrumented;
}

// Instrument client libraries the SDK cannot find on its own (bundled apps,
// strict package managers): pass what you import, for example
// plumbline.instrument({ openai: OpenAI, anthropic: Anthropic, bedrock: await import('@aws-sdk/client-bedrock-runtime') }).
function instrument(libs) {
  try {
    const list = require('./instrument.cjs').instrument(libs || {});
    instrumented = [...new Set([...instrumented, ...list])];
    return list;
  } catch (e) {
    core.fail('instrument', e);
    return [];
  }
}

function isEnabled() {
  return core.currentClient().enabled;
}

// Counters for health checks.
function stats() {
  const c = core.currentClient();
  const e = c.exporter;
  return {
    enabled: c.enabled,
    spans: c.stats.spans,
    instrumentationErrors: c.stats.instrumentationErrors,
    exported: e ? e.exported : 0,
    dropped: e ? e.dropped : 0,
    exportErrors: e ? e.exportErrors : 0,
    instrumented: c.enabled ? [...instrumented] : [],
  };
}

module.exports = {
  VERSION,
  init,
  flush,
  shutdown,
  isEnabled,
  stats,
  ready,
  instrument,
  trace: core.trace,
  prompt: core.prompt,
  personalData: core.personalData,
  guardrail: core.guardrail,
  tool: core.tool,
  agentRun: core.agentRun,
  llmCall: core.llmCall,
  escalation: core.escalation,
};
