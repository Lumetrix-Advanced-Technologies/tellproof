// Types for tellproof. The package is plain JavaScript; there is no build step.

export declare const VERSION: string;

export type Decision = 'allow' | 'block' | 'flag';

export interface InitOptions {
  /** Agent base URL. Default TELLPROOF_AGENT_URL, else http://127.0.0.1:4318. */
  agentUrl?: string;
  /** service.name. Default TELLPROOF_SERVICE or OTEL_SERVICE_NAME. */
  service?: string;
  environment?: string;
  /** Instrument the installed LLM clients (default true). */
  instrument?: boolean;
  /** Ask OpenAI for usage on streamed chat completions (adds one final chunk with no choices). Default false. */
  streamUsage?: boolean;
  /** Replace the HTTP transport (tests, custom routing). */
  transport?: (body: string) => Promise<void> | void;
  debug?: boolean;
}

export interface Stats {
  enabled: boolean;
  spans: number;
  instrumentationErrors: number;
  exported: number;
  dropped: number;
  exportErrors: number;
  instrumented: string[];
}

/** Start the SDK. Safe to call more than once; returns whether it is on. */
export declare function init(options?: InitOptions): boolean;
/** Send buffered spans now; true when everything was handed to the agent. */
export declare function flush(timeoutMs?: number): Promise<boolean>;
/** Flush and stop. */
export declare function shutdown(timeoutMs?: number): Promise<boolean>;
export declare function isEnabled(): boolean;
export declare function stats(): Stats;
/** Resolves (with the instrumented library names) once the ES module copies of the client libraries are instrumented too. */
export declare function ready(): Promise<string[]>;
/** Instrument client libraries the SDK cannot find itself (bundlers, strict package managers). Pass the module or class you import. */
export declare function instrument(libs: { openai?: unknown; anthropic?: unknown; bedrock?: unknown }): string[];

/** Group everything inside into one trace (for example one web request). */
export declare function trace<T>(fn: () => T): T;
/** Calls inside use this prompt (name and version, never its text). */
export declare function prompt<T>(name: string, version: string | number | undefined, fn: () => T): T;
export declare function prompt<T>(name: string, fn: () => T): T;
/** Spans inside handle personal data; redacted: removed or masked before the model saw it. */
export declare function personalData<T>(options: { redacted?: boolean }, fn: () => T): T;
export declare function personalData<T>(fn: () => T): T;

export interface Guardrail {
  decision: Decision | null;
  allow(): void;
  block(): void;
  flag(): void;
}
export interface GuardrailOptions {
  decision?: Decision | null;
  /** Attribute a block with no model call inside. */
  provider?: string;
  model?: string;
}
/** The guardrail decision for the LLM calls inside; a block with no call inside counts as one blocked call. */
export declare function guardrail<T>(fn: (g: Guardrail) => T): T;
export declare function guardrail<T>(decision: Decision | GuardrailOptions | null, fn: (g: Guardrail) => T): T;

export interface Tool {
  readonly name: string | null;
  highImpact: boolean;
  approved: boolean;
  error: boolean;
}
/** A tool call. highImpact: real-world effect; approved: a person or policy approved it. */
export declare function tool<T>(name: string, options: { highImpact?: boolean; approved?: boolean }, fn: (t: Tool) => T): T;
export declare function tool<T>(name: string, fn: (t: Tool) => T): T;

export interface AgentRun {
  readonly name: string | null;
  error: boolean;
}
/** One run of an agent; model changes are tracked per agent. */
export declare function agentRun<T>(name: string, fn: (run: AgentRun) => T): T;

export interface LLMCall {
  responseModel: string | null;
  error: boolean;
  usage(inputTokens?: number, outputTokens?: number): void;
}
/** A model call the SDK does not instrument. */
export declare function llmCall<T>(
  options: { provider: string; model: string; operation?: string; region?: string },
  fn: (call: LLMCall) => T,
): T;

/** The request was handed to a person. */
export declare function escalation(): void;

/** Guardrail canary kinds with a built-in probe (spec/canaries.json). */
export type CanaryKind = 'prompt_injection' | 'jailbreak' | 'pii' | 'secret';
/** The built-in synthetic probes. They never leave the process. */
export declare const PROBES: Readonly<Record<CanaryKind, string>>;
/** A guardrail's answer: true, "block" or "flag" means the probe was caught. */
export type CanaryResult = boolean | 'allow' | 'block' | 'flag' | string | null | undefined;
/** Record one canary the app ran itself: a short kind (a-z, 0-9, _ or -, at most 32) and whether it was caught. */
export declare function canary(kind: string, caught: CanaryResult): void;
/** Pass each built-in probe to the app's guardrail check and record whether it was caught. Throwing counts as missed. */
export declare function runCanaries(
  check: (probe: string) => CanaryResult | Promise<CanaryResult>,
  options?: { kinds?: Iterable<CanaryKind> },
): Promise<Partial<Record<CanaryKind, boolean>>>;
/** Run the canaries now and then every `every` ms (default an hour, at least a minute) on an unref'd timer. */
export declare function scheduleCanaries(
  check: (probe: string) => CanaryResult | Promise<CanaryResult>,
  options?: { every?: number; kinds?: Iterable<CanaryKind> },
): { every: number | null; stop(): void };

declare const sdk: {
  VERSION: typeof VERSION;
  init: typeof init;
  flush: typeof flush;
  shutdown: typeof shutdown;
  isEnabled: typeof isEnabled;
  stats: typeof stats;
  ready: typeof ready;
  instrument: typeof instrument;
  trace: typeof trace;
  prompt: typeof prompt;
  personalData: typeof personalData;
  guardrail: typeof guardrail;
  tool: typeof tool;
  agentRun: typeof agentRun;
  llmCall: typeof llmCall;
  escalation: typeof escalation;
  PROBES: typeof PROBES;
  canary: typeof canary;
  runCanaries: typeof runCanaries;
  scheduleCanaries: typeof scheduleCanaries;
};
export default sdk;
