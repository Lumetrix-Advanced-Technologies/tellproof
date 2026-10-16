"""A sample AI app with Plumbline evidence in one line (used by the end-to-end test).

Run an agent first (`plumbline-agent connect <token>`), then:
    pip install plumbline-sdk          # or PYTHONPATH=sdk/python/src from this repository
    python examples/python-app/app.py

With the openai, anthropic or boto3 libraries installed, plumbline.init() records
their calls by itself; this sample has no provider keys, so it reports each call
with llm_call, the helper for calls the SDK does not instrument.
"""
import os
import plumbline

plumbline.init(service="claims-api")  # the one line; the agent URL comes from PLUMBLINE_AGENT_URL

CALLS = int(os.environ.get("SAMPLE_CALLS", "20"))


def triage(n: int) -> None:
    """One claim: a redacted triage prompt behind an input guardrail."""
    with plumbline.trace(), plumbline.prompt("claims-triage", "14"), plumbline.personal_data(redacted=True):
        with plumbline.guardrail("allow"):
            with plumbline.llm_call("openai", "gpt-4.1-mini", region="us-east-1") as call:
                call.usage(input_tokens=1200 + n, output_tokens=80)


@plumbline.agent_run("refund-agent")
def refund(n: int) -> None:
    """An agent that looks up a policy and, with approval, issues a refund."""
    with plumbline.guardrail("allow"), plumbline.llm_call("openai", "gpt-4.1-mini", region="us-east-1") as call:
        call.usage(input_tokens=900, output_tokens=60)
    with plumbline.tool("lookup_policy"):
        pass
    with plumbline.tool("issue_refund", high_impact=True) as t:
        t.approved = True


for i in range(CALLS):
    triage(i)
    if i % 5 == 0:
        refund(i)

assert plumbline.flush(10), "the agent did not take the spans"
print("python sample sent", plumbline.stats())
plumbline.shutdown()
