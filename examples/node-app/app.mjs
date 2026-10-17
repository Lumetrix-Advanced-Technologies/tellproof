// A sample AI app with Tellproof evidence in one line (used by the end-to-end test).
//
// Run an agent first (`tellproof-agent connect <token>`), then:
//   npm install tellproof     # here it is imported from this repository
//   node examples/node-app/app.mjs
//
// With openai, @anthropic-ai/sdk or @aws-sdk/client-bedrock-runtime installed,
// tellproof.init() records their calls by itself; this sample has no provider
// keys, so it reports each call with llmCall.
import * as tellproof from '../../sdk/js/lib/index.mjs';

tellproof.init({ service: 'referral-api' }); // the one line; the agent URL comes from TELLPROOF_AGENT_URL

const calls = Number(process.env.SAMPLE_CALLS ?? 20);

async function summarise(n) {
  return tellproof.trace(() =>
    tellproof.prompt('referral-summary', '7', () =>
      tellproof.personalData({ redacted: true }, () =>
        tellproof.guardrail(async (g) => {
          await tellproof.llmCall({ provider: 'anthropic', model: 'claude-sonnet-4-5', region: 'us-east-1' }, async (c) => {
            c.usage(2000 + n, 150);
          });
          g.allow();
        }),
      ),
    ),
  );
}

for (let i = 0; i < calls; i++) await summarise(i);
if (!(await tellproof.flush(10000))) throw new Error('the agent did not take the spans');
console.log('node sample sent', JSON.stringify(tellproof.stats()));
await tellproof.shutdown();
