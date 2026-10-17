"""The shared SDK scenario (sdk/testdata/README.md): the same app behaviour,
written once per SDK, must aggregate in the Go agent to
sdk/testdata/expected-windows.json.

Run directly to rewrite sdk/testdata/python-export.jsonl.
"""

from __future__ import annotations

import os
import sys
from datetime import datetime, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "src"))

import tellproof  # noqa: E402
from tellproof import _core, _otlp  # noqa: E402

FIXTURE = os.path.join(HERE, "..", "..", "testdata", "python-export.jsonl")
RESOURCE = {"service.name": "claims-api", "telemetry.sdk.name": "tellproof", "telemetry.sdk.language": "python",
            "telemetry.sdk.version": "scenario"}


class Clock:
    def __init__(self, iso: str) -> None:
        self.ns = int(datetime.fromisoformat(iso).replace(tzinfo=timezone.utc).timestamp()) * 10**9

    def __call__(self) -> int:
        self.ns += 250_000_000  # every reading moves 250 ms
        return self.ns

    def at(self, iso: str) -> None:
        self.ns = int(datetime.fromisoformat(iso).replace(tzinfo=timezone.utc).timestamp()) * 10**9


class Ids:
    def __init__(self) -> None:
        self.n = 0

    def __call__(self, nbytes: int) -> str:
        self.n += 1
        return f"{self.n:0{nbytes * 2}x}"


class Capture:
    """An exporter that keeps spans in order instead of sending them."""

    def __init__(self) -> None:
        self.spans: list[dict] = []

    def offer(self, span: dict) -> None:
        self.spans.append(span)


def run(clock: Clock) -> None:
    """The app. Hour 10:00: four requests; hour 11:00: one."""
    # 1. A triage call on personal data, redacted, allowed by the guardrail.
    with tellproof.trace(), tellproof.prompt("triage", "14"), tellproof.personal_data(redacted=True):
        with tellproof.guardrail("allow"):
            with tellproof.llm_call("openai", "gpt-4.1-mini", region="us-east-1") as c:
                c.usage(input_tokens=1200, output_tokens=80)

    # 2. An agent run: a model call the output guardrail then blocks, three tool
    #    calls (one high-impact approved, one high-impact unapproved that fails)
    #    and an escalation to a person.
    with tellproof.agent_run("claims-agent"):
        with tellproof.guardrail() as g:
            with tellproof.llm_call("anthropic", "claude-sonnet-4-5", region="ap-southeast-2") as c:
                c.usage(input_tokens=3000, output_tokens=400)
            g.block()
        with tellproof.tool("lookup_policy"):
            pass
        with tellproof.tool("issue_refund", high_impact=True) as t:
            t.approved = True
        try:
            with tellproof.tool("close_account", high_impact=True):
                raise PermissionError("not approved")
        except PermissionError:
            pass
        tellproof.escalation()

    # 3. The input guardrail blocks before any model call.
    with tellproof.trace(), tellproof.personal_data(redacted=False):
        with tellproof.guardrail("block", provider="openai", model="gpt-4.1-mini"):
            pass

    # 4. A failed embeddings call outside any scope.
    try:
        with tellproof.llm_call("openai", "text-embedding-3-small", operation="embeddings", region="us-east-1"):
            raise TimeoutError()
    except TimeoutError:
        pass

    # 5. The next hour: the triage prompt moves to version 15 on another model.
    clock.at("2026-09-01T11:20:00")
    with tellproof.prompt("triage", "15"), tellproof.guardrail("flag"):
        with tellproof.llm_call("anthropic", "claude-sonnet-4-5", region="ap-southeast-2") as c:
            c.usage(input_tokens=900, output_tokens=60)

    # 6. Guardrail canaries (D-057): the guardrail catches the injection probe
    #    and one of two personal-data probes.
    tellproof.canary("prompt_injection", "block")
    tellproof.canary("pii", True)
    tellproof.canary("pii", "allow")


def export_lines() -> list[str]:
    clock = Clock("2026-09-01T10:15:00")
    cap = Capture()
    old = _core.current_client()
    _core.set_client(_core.Client(cap, service="claims-api", clock=clock, ids=Ids()))
    try:
        run(clock)
    finally:
        _core.set_client(old)
    # One export request per hour, as the exporter would batch them.
    lines, hours = [], {}
    for s in cap.spans:
        hours.setdefault(s["start"] // (3600 * 10**9), []).append(s)
    for h in sorted(hours):
        lines.append(_otlp.encode(hours[h], RESOURCE, "scenario").decode())
    return lines


if __name__ == "__main__":
    with open(FIXTURE, "w", encoding="utf-8") as f:
        f.write("\n".join(export_lines()) + "\n")
    print(f"wrote {os.path.relpath(FIXTURE)}")
