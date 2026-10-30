// The real client libraries, when the app directory (cwd) has them installed,
// against a local fake API. CI's sdk-js-libs job installs them into a temp app
// and runs this file from there; they are test-only, never SDK dependencies.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { createRequire } from 'node:module';
import { join } from 'node:path';
import { rmSync, writeFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
import { Capture } from './scenario.mjs';

const require = createRequire(import.meta.url);
const core = require('../lib/core.cjs');
const ins = require('../lib/instrument.cjs');
const appRequire = createRequire(join(process.cwd(), 'app.js'));
const MARK = 'REAL-LIB-MARKER';

// Node itself says which file the app's `import` loads (never the SDK's own
// finder, which is what is under test): a module in the app directory resolves it.
const resolver = join(process.cwd(), `.tellproof-resolve-${process.pid}.mjs`);
writeFileSync(resolver, 'export const resolve = (s) => import.meta.resolve(s);\n');
const { resolve: nodeResolve } = await import(pathToFileURL(resolver).href);
rmSync(resolver);

function load(spec) {
  let cjs;
  try {
    cjs = appRequire(spec);
  } catch {
    return null;
  }
  return { cjs, esmUrl: nodeResolve(spec), cjsUrl: pathToFileURL(appRequire.resolve(spec)).href };
}
const libs = { openai: load('openai'), anthropic: load('@anthropic-ai/sdk'), bedrock: load('@aws-sdk/client-bedrock-runtime') };

test('the finder names the file Node imports, or null when import shares the CommonJS copy', () => {
  for (const [spec, lib] of [['openai', libs.openai], ['@anthropic-ai/sdk', libs.anthropic], ['@aws-sdk/client-bedrock-runtime', libs.bedrock]]) {
    if (!lib) continue;
    assert.equal(ins.esmEntry(spec, appRequire) || lib.cjsUrl, lib.esmUrl, spec);
  }
});

const sse = (events) => events.map(([name, data]) => (name ? `event: ${name}\n` : '') + `data: ${typeof data === 'string' ? data : JSON.stringify(data)}\n\n`).join('');
const CHAT = { id: 'c', object: 'chat.completion', created: 1, model: 'gpt-4.1-mini-2025-04-14',
  choices: [{ index: 0, message: { role: 'assistant', content: MARK }, finish_reason: 'stop' }], usage: { prompt_tokens: 12, completion_tokens: 3, total_tokens: 15 } };
const chunk = (content, usage = null, finish = null, role = undefined) => ({ id: 'c', object: 'chat.completion.chunk', created: 1, model: 'gpt-4.1-mini-2025-04-14',
  choices: content === null ? [] : [{ index: 0, delta: role ? { role, content } : { content }, finish_reason: finish }], usage });
const MSG_USAGE = { input_tokens: 10, output_tokens: 5, cache_creation_input_tokens: 2, cache_read_input_tokens: 3 };
const MESSAGE = { id: 'm', type: 'message', role: 'assistant', model: 'claude-test-model-20250929', content: [{ type: 'text', text: MARK }],
  stop_reason: 'end_turn', stop_sequence: null, usage: MSG_USAGE };
const MSG_EVENTS = [
  ['message_start', { type: 'message_start', message: { ...MESSAGE, content: [], stop_reason: null, usage: { ...MSG_USAGE, output_tokens: 1 } } }],
  ['content_block_start', { type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } }],
  ['content_block_delta', { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: MARK } }],
  ['content_block_stop', { type: 'content_block_stop', index: 0 }],
  ['message_delta', { type: 'message_delta', delta: { stop_reason: 'end_turn', stop_sequence: null }, usage: { output_tokens: 5 } }],
  ['message_stop', { type: 'message_stop' }],
];

let srv, url, cap, old;
before(async () => {
  srv = createServer((req, res) => {
    let raw = '';
    req.on('data', (d) => (raw += d));
    req.on('end', () => {
      let body = {};
      try {
        body = JSON.parse(raw || '{}');
      } catch {}
      const send = (data, type = 'application/json', headers = {}) => {
        res.writeHead(200, { 'Content-Type': type, ...headers });
        res.end(data);
      };
      if (req.url.endsWith('/chat/completions')) {
        if (!body.stream) return send(JSON.stringify(CHAT));
        // As the API streams: the role on the first delta, finish_reason on the last.
        const ev = [[null, chunk(MARK, null, null, 'assistant')], [null, chunk(MARK)], [null, chunk('', null, 'stop')]];
        if (body.stream_options && body.stream_options.include_usage) ev.push([null, chunk(null, { prompt_tokens: 12, completion_tokens: 3, total_tokens: 15 })]);
        return send(sse([...ev, [null, '[DONE]']]), 'text/event-stream');
      }
      if (req.url.endsWith('/messages')) return body.stream ? send(sse(MSG_EVENTS), 'text/event-stream') : send(JSON.stringify(MESSAGE));
      if (req.url.endsWith('/converse'))
        return send(JSON.stringify({ output: { message: { role: 'assistant', content: [{ text: MARK }] } }, stopReason: 'end_turn',
          usage: { inputTokens: 7, outputTokens: 2, totalTokens: 9 }, metrics: { latencyMs: 1 } }));
      if (req.url.endsWith('/invoke'))
        return send(JSON.stringify({ content: [{ type: 'text', text: MARK }] }), 'application/json', {
          'x-amzn-bedrock-input-token-count': '21', 'x-amzn-bedrock-output-token-count': '4' });
      res.writeHead(404);
      res.end();
    });
  });
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  url = `http://127.0.0.1:${srv.address().port}`;
  ins.install({ streamUsage: true });
  await ins.ready();
  old = core.currentClient();
});
after(() => {
  srv.close();
  core.setClient(old);
});

function capture() {
  cap = new Capture();
  core.setClient(new core.Client(cap, { service: 't' }));
}
const tokens = () => cap.spans.map((s) => [s.attrs['gen_ai.provider.name'], s.attrs['gen_ai.usage.input_tokens'], s.attrs['gen_ai.usage.output_tokens']]);
const noContent = () => assert.ok(!JSON.stringify(cap.spans, (k, v) => (typeof v === 'bigint' ? String(v) : v)).includes(MARK));

for (const copy of ['cjs', 'esm']) {
  test(`openai (${copy})`, { skip: !libs.openai && 'openai not installed' }, async () => {
    capture();
    const mod = copy === 'cjs' ? libs.openai.cjs : await import(libs.openai.esmUrl);
    const OpenAI = mod.OpenAI || mod.default;
    const c = new OpenAI({ apiKey: 'test', baseURL: url + '/v1', maxRetries: 0 });
    const r = await c.chat.completions.create({ model: 'gpt-4.1-mini', messages: [{ role: 'user', content: MARK }] });
    assert.equal(r.choices[0].message.content, MARK);
    const { data } = await c.chat.completions.create({ model: 'gpt-4.1-mini', messages: [] }).withResponse();
    assert.equal(data.usage.prompt_tokens, 12);
    let text = '';
    for await (const ch of await c.chat.completions.create({ model: 'gpt-4.1-mini', messages: [], stream: true })) text += (ch.choices[0] && ch.choices[0].delta.content) || '';
    assert.equal(text, MARK + MARK);
    const s = c.chat.completions.stream({ model: 'gpt-4.1-mini', messages: [] });
    for await (const _ of s);
    assert.deepEqual(tokens(), Array(4).fill(['127.0.0.1', 12, 3])); // a local base URL is named by its host
    noContent();
  });

  test(`anthropic (${copy})`, { skip: !libs.anthropic && '@anthropic-ai/sdk not installed' }, async () => {
    capture();
    const mod = copy === 'cjs' ? libs.anthropic.cjs : await import(libs.anthropic.esmUrl);
    const Anthropic = mod.Anthropic || mod.default;
    const c = new Anthropic({ apiKey: 'test', baseURL: url, maxRetries: 0 });
    await c.messages.create({ model: 'claude-test-model', max_tokens: 10, messages: [{ role: 'user', content: MARK }] });
    for await (const _ of await c.messages.create({ model: 'claude-test-model', max_tokens: 10, messages: [], stream: true }));
    const text = await c.messages.stream({ model: 'claude-test-model', max_tokens: 10, messages: [] }).finalText();
    assert.equal(text, MARK);
    assert.deepEqual(tokens(), Array(3).fill(['anthropic', 15, 5]));
    noContent();
  });

  test(`bedrock (${copy})`, { skip: !libs.bedrock && '@aws-sdk/client-bedrock-runtime not installed' }, async () => {
    capture();
    const B = copy === 'cjs' ? libs.bedrock.cjs : await import(libs.bedrock.esmUrl);
    // The client defaults to HTTP/2; the local fake API speaks HTTP/1.1.
    const { NodeHttpHandler } = appRequire('@smithy/node-http-handler');
    const c = new B.BedrockRuntimeClient({ region: 'ap-southeast-2', endpoint: url, credentials: { accessKeyId: 'test', secretAccessKey: 'test' },
      requestHandler: new NodeHttpHandler() });
    const r = await c.send(new B.ConverseCommand({ modelId: 'anthropic.claude-test-model', messages: [{ role: 'user', content: [{ text: MARK }] }] }));
    assert.equal(r.output.message.content[0].text, MARK);
    const inv = await c.send(new B.InvokeModelCommand({ modelId: 'anthropic.claude-test-model', body: JSON.stringify({ messages: [] }), contentType: 'application/json' }));
    assert.ok(new TextDecoder().decode(inv.body).includes(MARK));
    assert.deepEqual(tokens(), [['aws.bedrock', 7, 2], ['aws.bedrock', 21, 4]]);
    assert.equal(cap.spans[0].attrs['cloud.region'], 'ap-southeast-2');
    noContent();
  });
}
