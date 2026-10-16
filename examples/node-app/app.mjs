// A sample AI app with Plumbline evidence in one line (used by the end-to-end test).
//
// Run an agent first (`plumbline-agent connect <token>`), then:
//   npm install plumbline-sdk     # here it is imported from this repository
//   node examples/node-app/app.mjs
//
// With openai, @anthropic-ai/sdk or @aws-sdk/client-bedrock-runtime installed,
// plumbline.init() records their calls by itself; this sample has no provider
// keys, so it reports each call with llmCall.
import * as plumbline from '../../sdk/js/lib/index.mjs';

plumbline.init({ service: 'referral-api' }); // the one line; the agent URL comes from PLUMBLINE_AGENT_URL

const calls = Number(process.env.SAMPLE_CALLS ?? 20);

async function summarise(n) {
  return plumbline.trace(() =>
    plumbline.prompt('referral-summary', '7', () =>
      plumbline.personalData({ redacted: true }, () =>
        plumbline.guardrail(async (g) => {
          await plumbline.llmCall({ provider: 'anthropic', model: 'claude-sonnet-4-5', region: 'us-east-1' }, async (c) => {
            c.usage(2000 + n, 150);
          });
          g.allow();
        }),
      ),
    ),
  );
}

for (let i = 0; i < calls; i++) await summarise(i);
if (!(await plumbline.flush(10000))) throw new Error('the agent did not take the spans');
console.log('node sample sent', JSON.stringify(plumbline.stats()));
await plumbline.shutdown();
