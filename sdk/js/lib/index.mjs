// ES module entry. It re-exports the CommonJS implementation, so `import` and
// `require` share one SDK instance (one buffer, one configuration).
import sdk from './index.cjs';

export const { VERSION, init, flush, shutdown, isEnabled, stats, ready, instrument, trace, prompt, personalData, guardrail, tool, agentRun, llmCall, escalation, PROBES, canary, runCanaries, scheduleCanaries } = sdk;
export default sdk;
