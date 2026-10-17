// Fake openai, @anthropic-ai/sdk and @aws-sdk/client-bedrock-runtime packages
// with the real libraries' shapes (checked against their sources on 15 Oct
// 2026): a CommonJS and an ES module copy each, as the real packages ship;
// Stainless's lazy APIPromise with _thenUnwrap and withResponse; streams with
// #private fields; anthropic's messages.stream() built on create(); a smithy
// client whose send() runs the command's middleware stack. Every piece of
// content carries the marker so tests can prove none reaches an export.

import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const shared = (marker) => `
const MARKER = ${JSON.stringify(marker)};
class APIError extends Error {}

// Stainless APIPromise: parses only when consumed; _thenUnwrap keeps the response.
class APIPromise extends Promise {
  #client;
  constructor(client, responsePromise, parseResponse = (c, p) => p.data) {
    super((resolve) => resolve(null));
    this.#client = client;
    this.responsePromise = responsePromise;
    this.parseResponse = parseResponse;
    this.parsedPromise = undefined;
  }
  static get [Symbol.species]() { return Promise; }
  _thenUnwrap(transform) {
    return new APIPromise(this.#client, this.responsePromise, async (client, props) => transform(await this.parseResponse(client, props), props));
  }
  asResponse() { return this.responsePromise.then((p) => p.response); }
  async withResponse() {
    const [data, response] = await Promise.all([this.parse(), this.asResponse()]);
    return { data, response, request_id: 'req_1' };
  }
  parse() {
    if (!this.parsedPromise) this.parsedPromise = this.responsePromise.then((p) => this.parseResponse(this.#client, p));
    return this.parsedPromise;
  }
  then(a, b) { return this.parse().then(a, b); }
  catch(b) { return this.parse().catch(b); }
  finally(f) { return this.parse().finally(f); }
}

class Stream {
  #consumed = false;
  constructor(items) { this.items = items; this.controller = new AbortController(); }
  async *[Symbol.asyncIterator]() {
    if (this.#consumed) throw new Error('Cannot iterate over a consumed stream');
    this.#consumed = true;
    for (const it of this.items) {
      if (it instanceof Error) throw it;
      yield it;
    }
  }
  tee() { return [new Stream(this.items), new Stream(this.items)]; }
}

function request(client, makeData) {
  const responsePromise = (async () => {
    await null;
    return { response: { status: 200, headers: new Map([['x-request-id', 'req_1']]) }, data: makeData() };
  })();
  return new APIPromise(client, responsePromise);
}
`;

const openaiSource = (marker) => `${shared(marker)}
const usage = () => ({ prompt_tokens: 120, completion_tokens: 30, total_tokens: 150, prompt_tokens_details: { cached_tokens: 100 } });
const completion = (model) => ({ id: 'c', object: 'chat.completion', model: model + '-2025-04-14',
  choices: [{ index: 0, message: { role: 'assistant', content: MARKER }, finish_reason: 'stop' }], usage: usage() });
const chunks = (model, withUsage) => {
  const c = [MARKER, MARKER].map((t) => ({ object: 'chat.completion.chunk', model: model + '-2025-04-14', usage: null,
    choices: [{ index: 0, delta: { content: t }, finish_reason: null }] }));
  if (withUsage) c.push({ object: 'chat.completion.chunk', model: model + '-2025-04-14', choices: [], usage: usage() });
  return c;
};
const response = (model, status = 'completed') => ({ id: 'r', object: 'response', model: model + '-2025-04-14', status,
  output: [{ type: 'message', content: [{ type: 'output_text', text: MARKER }] }],
  usage: { input_tokens: 80, output_tokens: 12, total_tokens: 92 } });
class APIResource { constructor(client) { this._client = client; } }
class Completions extends APIResource {
  create(body, options) {
    if (body.model === 'boom') return new APIPromise(this._client, Promise.reject(new APIError('400 ' + MARKER)));
    if (body.stream) return request(this._client, () => new Stream(chunks(body.model, body.stream_options && body.stream_options.include_usage)));
    return request(this._client, () => completion(body.model));
  }
  parse(body, options) { return this.create({ ...body, stream: false }, options)._thenUnwrap((c) => ({ ...c, parsed: true })); }
  // like openai's ChatCompletionStream: create(stream: true), then iterate
  stream(body) {
    const self = this;
    return { async *[Symbol.asyncIterator]() { const s = await self.create({ ...body, stream: true }); yield* s; } };
  }
}
class Chat extends APIResource { completions = new Completions(this._client); }
Chat.Completions = Completions;
class Responses extends APIResource {
  create(body) {
    if (body.stream) return request(this._client, () => new Stream([
      { type: 'response.created', response: { model: body.model, usage: null } },
      { type: 'response.output_text.delta', delta: MARKER },
      { type: body._final || 'response.completed', response: response(body.model, body._final === 'response.failed' ? 'failed' : 'completed') }]));
    return request(this._client, () => response(body.model));
  }
}
class Embeddings extends APIResource {
  create(body) { return request(this._client, () => ({ object: 'list', model: body.model, data: [{ embedding: [0.1] }], usage: { prompt_tokens: 9, total_tokens: 9 } })); }
}
class LegacyCompletions extends APIResource {
  create(body) { return request(this._client, () => ({ model: body.model, choices: [{ text: MARKER }], usage: { prompt_tokens: 4, completion_tokens: 2 } })); }
}
class OpenAI {
  constructor({ baseURL = 'https://api.openai.com/v1' } = {}) {
    this.baseURL = baseURL;
    this.chat = new Chat(this);
    this.responses = new Responses(this);
    this.embeddings = new Embeddings(this);
    this.completions = new LegacyCompletions(this);
  }
}
OpenAI.Chat = Chat;
OpenAI.Responses = Responses;
OpenAI.Embeddings = Embeddings;
OpenAI.Completions = LegacyCompletions;
class AzureOpenAI extends OpenAI { constructor() { super({ baseURL: 'https://contoso.openai.azure.com/openai' }); } }
OpenAI.OpenAI = OpenAI;
OpenAI.AzureOpenAI = AzureOpenAI;
OpenAI.APIError = APIError;
OpenAI.Stream = Stream;
`;

const anthropicSource = (marker) => `${shared(marker)}
const usage = (final) => ({ input_tokens: 10, cache_creation_input_tokens: 5, cache_read_input_tokens: 100, output_tokens: final ? 20 : 1 });
const message = (model) => ({ id: 'm', type: 'message', role: 'assistant', model: model + '-20250929',
  content: [{ type: 'text', text: MARKER }], stop_reason: 'end_turn', usage: usage(true) });
const events = (model) => [
  { type: 'message_start', message: { ...message(model), content: [], usage: usage(false) } },
  { type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } },
  { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: MARKER } },
  { type: 'content_block_stop', index: 0 },
  // cumulative; newer API versions repeat only some input fields here
  { type: 'message_delta', delta: { stop_reason: 'end_turn' }, usage: { output_tokens: 20, input_tokens: 10 } },
  { type: 'message_stop' }];
class APIResource { constructor(client) { this._client = client; } }
// like the SDK's MessageStream: create(stream: true).withResponse(), then iterate
class MessageStream {
  #messages; #params;
  constructor(messages, params) { this.#messages = messages; this.#params = params; }
  async *[Symbol.asyncIterator]() {
    const { data } = await this.#messages.create({ ...this.#params, stream: true }).withResponse();
    for await (const ev of data) yield ev;
  }
  async finalText() { let t = ''; for await (const ev of this) if (ev.type === 'content_block_delta') t += ev.delta.text; return t; }
}
class Messages extends APIResource {
  create(body) {
    if (body.model === 'boom') return new APIPromise(this._client, Promise.reject(new APIError('529 overloaded ' + MARKER)));
    if (body.stream) return request(this._client, () => new Stream(events(body.model)));
    return request(this._client, () => message(body.model));
  }
  stream(body) { return new MessageStream(this, body); }
}
class BetaMessages extends Messages {}
class Beta extends APIResource { messages = new BetaMessages(this._client); }
Beta.Messages = BetaMessages;
class Anthropic {
  constructor() { this.messages = new Messages(this); this.beta = new Beta(this); }
}
Anthropic.Messages = Messages;
Anthropic.Beta = Beta;
Anthropic.Anthropic = Anthropic;
Anthropic.APIError = APIError;
class AnthropicBedrock extends Anthropic { awsRegion = 'ap-southeast-2'; }
Anthropic.AnthropicBedrock = AnthropicBedrock;
`;

const bedrockSource = (marker) => `
const MARKER = ${JSON.stringify(marker)};
class MiddlewareStack {
  constructor() { this.entries = []; }
  add(mw, opts) { this.entries = this.entries.filter((e) => !(opts.override && e.opts.name === opts.name)); this.entries.push({ mw, opts }); }
}
class Command { constructor(input) { this.input = input; this.middlewareStack = new MiddlewareStack(); } }
class ConverseCommand extends Command {}
class ConverseStreamCommand extends Command {}
class InvokeModelCommand extends Command {}
class InvokeModelWithResponseStreamCommand extends Command {}
class ListFoundationModelsCommand extends Command {}
class ValidationException extends Error {}

async function* events(list) { for (const e of list) yield e; }

// smithy Client: send() runs the command's middleware around the handler.
class Client {
  constructor(config) { this.config = config; }
  send(command, optionsOrCb, cb) {
    const handler = async () => this.handle(command);
    const deserialize = command.middlewareStack.entries.filter((e) => e.opts.step === 'deserialize');
    let h = handler;
    for (const { mw } of deserialize) h = mw(h, {});
    const p = h({ input: command.input }).then((r) => r.output);
    if (typeof optionsOrCb === 'function' || typeof cb === 'function') {
      const callback = typeof optionsOrCb === 'function' ? optionsOrCb : cb;
      p.then((o) => callback(null, o), (e) => callback(e));
      return;
    }
    return p;
  }
}
class BedrockRuntimeClient extends Client {
  constructor({ region = 'ap-southeast-2' } = {}) { super({ region: async () => region }); }
  async handle(command) {
    const input = command.input;
    if (input.modelId === 'boom') throw new ValidationException('ValidationException: ' + MARKER);
    const stop = input._intervene ? 'guardrail_intervened' : 'end_turn';
    const meta = { $metadata: { httpStatusCode: 200, requestId: 'r' } };
    if (command instanceof ConverseCommand)
      return { response: { headers: {} }, output: { ...meta, output: { message: { role: 'assistant', content: [{ text: MARKER }] } }, stopReason: stop,
        usage: { inputTokens: 50, outputTokens: 9, totalTokens: 159, cacheReadInputTokens: 100 }, metrics: { latencyMs: 1 } } };
    if (command instanceof ConverseStreamCommand)
      return { response: { headers: {} }, output: { ...meta, stream: events([{ messageStart: { role: 'assistant' } },
        { contentBlockDelta: { delta: { text: MARKER }, contentBlockIndex: 0 } }, { messageStop: { stopReason: stop } },
        { metadata: { usage: { inputTokens: 50, outputTokens: 9, totalTokens: 59 }, metrics: { latencyMs: 1 } } }]) } };
    if (command instanceof InvokeModelCommand)
      return { response: { headers: { 'x-amzn-bedrock-input-token-count': '42', 'x-amzn-bedrock-output-token-count': '7' } },
        output: { ...meta, body: new TextEncoder().encode(JSON.stringify({ content: [{ text: MARKER }] })), contentType: 'application/json' } };
    if (command instanceof InvokeModelWithResponseStreamCommand)
      return { response: { headers: {} }, output: { ...meta, body: events([{ chunk: { bytes: new TextEncoder().encode(MARKER) } }]) } };
    return { response: { headers: {} }, output: { ...meta, modelSummaries: [{ modelName: MARKER }] } };
  }
}
class BedrockRuntime extends BedrockRuntimeClient {
  converse(input) { return this.send(new ConverseCommand(input)); }
}
`;

const BEDROCK_EXPORTS = 'BedrockRuntimeClient, BedrockRuntime, ConverseCommand, ConverseStreamCommand, InvokeModelCommand, InvokeModelWithResponseStreamCommand, ListFoundationModelsCommand, ValidationException';

function pkg(root, name, source, cjsExports, esmExports) {
  const dir = join(root, 'node_modules', ...name.split('/'));
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, 'package.json'), JSON.stringify({ name, version: '0.0.0-fake', exports: { '.': { import: './index.mjs', require: './index.js' } } }));
  writeFileSync(join(dir, 'index.js'), `'use strict';\n${source}\n${cjsExports}\n`);
  writeFileSync(join(dir, 'index.mjs'), `${source}\n${esmExports}\n`);
  return dir;
}

// Writes the three fake packages into a new temporary app directory.
export function fakeApp(marker = 'CONTENT-MARKER') {
  const root = mkdtempSync(join(tmpdir(), 'tellproof-fakes-'));
  pkg(root, 'openai', openaiSource(marker), 'module.exports = OpenAI;', 'export { OpenAI, AzureOpenAI, APIError, Stream };\nexport default OpenAI;');
  pkg(root, '@anthropic-ai/sdk', anthropicSource(marker), 'module.exports = Anthropic;', 'export { Anthropic, AnthropicBedrock, APIError };\nexport default Anthropic;');
  pkg(root, '@aws-sdk/client-bedrock-runtime', bedrockSource(marker), `module.exports = { ${BEDROCK_EXPORTS} };`, `export { ${BEDROCK_EXPORTS} };`);
  return root;
}
