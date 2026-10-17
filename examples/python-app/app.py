"""A sample AI app with Tellproof evidence in one line (used by the end-to-end test).

Run an agent first (`tellproof-agent connect <token>`), then:
    pip install tellproof          # or PYTHONPATH=sdk/python/src from this repository
    python examples/python-app/app.py

With the openai, anthropic or boto3 libraries installed, tellproof.init() records
their calls by itself; this sample has no provider keys, so it reports each call
with llm_call, the helper for calls the SDK does not instrument.
"""
import os
import tellproof

tellproof.init(service="claims-api")  # the one line; the agent URL comes from TELLPROOF_AGENT_URL

CALLS = int(os.environ.get("SAMPLE_CALLS", "20"))


def triage(n: int) -> None:
    """One claim: a redacted triage prompt behind an input guardrail."""
    with tellproof.trace(), tellproof.prompt("claims-triage", "14"), tellproof.personal_data(redacted=True):
        with tellproof.guardrail("allow"):
            with tellproof.llm_call("openai", "gpt-4.1-mini", region="us-east-1") as call:
                call.usage(input_tokens=1200 + n, output_tokens=80)


@tellproof.agent_run("refund-agent")
def refund(n: int) -> None:
    """An agent that looks up a policy and, with approval, issues a refund."""
    with tellproof.guardrail("allow"), tellproof.llm_call("openai", "gpt-4.1-mini", region="us-east-1") as call:
        call.usage(input_tokens=900, output_tokens=60)
    with tellproof.tool("lookup_policy"):
        pass
    with tellproof.tool("issue_refund", high_impact=True) as t:
        t.approved = True


for i in range(CALLS):
    triage(i)
    if i % 5 == 0:
        refund(i)

assert tellproof.flush(10), "the agent did not take the spans"
print("python sample sent", tellproof.stats())
tellproof.shutdown()
