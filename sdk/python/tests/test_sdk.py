import asyncio
import json
import os
import random
import string
import sys
import threading
import time
import unittest
import warnings
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "src"))
sys.path.insert(0, HERE)

import tellproof  # noqa: E402
from tellproof import _core, _otlp  # noqa: E402
from tellproof._export import Exporter, http_transport  # noqa: E402
import scenario  # noqa: E402


class Capturing:
    """Install a client whose spans are kept in memory."""

    def __enter__(self):
        self.cap = scenario.Capture()
        self.old = _core.current_client()
        _core.set_client(_core.Client(self.cap, service="test"))
        return self.cap.spans

    def __exit__(self, *exc):
        _core.set_client(self.old)


def attrs(span):
    return span["attrs"]


class FixtureTest(unittest.TestCase):
    def test_committed_fixture_is_current(self):
        with open(scenario.FIXTURE, encoding="utf-8") as f:
            committed = f.read()
        self.assertEqual(committed, "\n".join(scenario.export_lines()) + "\n",
                         "sdk/testdata/python-export.jsonl is stale: run python3 sdk/python/tests/scenario.py")


class HelperTest(unittest.TestCase):
    def test_scopes_stamp_spans_and_nest(self):
        with Capturing() as spans:
            with tellproof.agent_run("helper"), tellproof.prompt("reply", 7), tellproof.personal_data(redacted=True):
                with tellproof.llm_call("openai", "gpt-4.1", region="eu-west-1") as c:
                    c.usage(input_tokens=10, output_tokens=3)
                    c.response_model = "gpt-4.1-2026-04-14"
        llm, run = spans
        self.assertEqual(attrs(llm)["tellproof.prompt.version"], "7")
        self.assertEqual(attrs(llm)["gen_ai.agent.name"], "helper")
        self.assertEqual(attrs(llm)["gen_ai.response.model"], "gpt-4.1-2026-04-14")
        self.assertIs(attrs(llm)["tellproof.redaction.applied"], True)
        self.assertEqual(llm["parent"], run["span_id"])
        self.assertEqual(llm["trace_id"], run["trace_id"])
        self.assertEqual(attrs(run)["gen_ai.operation.name"], "invoke_agent")

    def test_spans_outside_scopes_get_their_own_traces(self):
        with Capturing() as spans:
            for _ in range(3):
                with tellproof.llm_call("openai", "m"):
                    pass
        self.assertEqual(len({s["trace_id"] for s in spans}), 3)

    def test_guardrail_decided_after_the_call(self):
        with Capturing() as spans:
            with tellproof.guardrail() as g:
                with tellproof.llm_call("openai", "m"):
                    pass
                self.assertEqual(spans, [], "the call must wait for the decision")
                g.flag()
        self.assertEqual(attrs(spans[0])["tellproof.guardrail.decision"], "flag")

    def test_guardrail_inner_decision_wins_and_outer_fills_the_rest(self):
        with Capturing() as spans:
            with tellproof.guardrail("allow"):
                with tellproof.guardrail("block"):
                    with tellproof.llm_call("openai", "inner"):
                        pass
                with tellproof.guardrail():
                    with tellproof.llm_call("openai", "undecided"):
                        pass
        got = {attrs(s)["gen_ai.request.model"]: attrs(s)["tellproof.guardrail.decision"] for s in spans}
        self.assertEqual(got, {"inner": "block", "undecided": "allow"})

    def test_block_without_a_call_is_one_blocked_call(self):
        with Capturing() as spans:
            with tellproof.guardrail("block", provider="anthropic", model="claude-haiku-4-5"):
                pass
            with tellproof.guardrail("allow"):  # allowed but nothing called: nothing to record
                pass
        self.assertEqual(len(spans), 1)
        a = attrs(spans[0])
        self.assertEqual((a["gen_ai.provider.name"], a["gen_ai.request.model"], a["tellproof.guardrail.decision"]),
                         ("anthropic", "claude-haiku-4-5", "block"))

    def test_invalid_decision_is_ignored_not_raised(self):
        with Capturing() as spans:
            with tellproof.guardrail("maybe"):
                with tellproof.llm_call("openai", "m"):
                    pass
            errors = _core.current_client().stats.instrumentation_errors
        self.assertNotIn("tellproof.guardrail.decision", attrs(spans[0]))
        self.assertEqual(errors, 1)

    def test_tool_errors_and_approval(self):
        with Capturing() as spans:
            with self.assertRaises(KeyError):
                with tellproof.tool("refund", high_impact=True) as t:
                    t.approved = True
                    raise KeyError("x")
        s = spans[0]
        self.assertTrue(s["error"])
        self.assertEqual((attrs(s)["tellproof.tool.high_impact"], attrs(s)["tellproof.tool.approved"]), (True, True))

    def test_decorators_sync_and_async_get_a_fresh_scope_per_call(self):
        @tellproof.tool("lookup")
        def lookup(x):
            return x * 2

        @tellproof.agent_run("async-agent")
        async def run(x):
            with tellproof.llm_call("openai", "m"):
                await asyncio.sleep(0)
            return x

        with Capturing() as spans:
            self.assertEqual(lookup(2), 4)
            self.assertEqual(lookup(3), 6)
            self.assertEqual(asyncio.run(run(5)), 5)
        self.assertEqual([s["name"] for s in spans], ["execute_tool", "execute_tool", "chat", "invoke_agent"])
        self.assertNotEqual(spans[0]["span_id"], spans[1]["span_id"])
        self.assertEqual(spans[2]["trace_id"], spans[3]["trace_id"])

    def test_concurrent_tasks_keep_their_own_context(self):
        async def one(n):
            with tellproof.prompt("p", n):
                await asyncio.sleep(0.01 * (3 - n))
                with tellproof.llm_call("openai", "m"):
                    pass

        async def main():
            await asyncio.gather(*(one(n) for n in range(3)))

        with Capturing() as spans:
            asyncio.run(main())
        self.assertEqual(sorted(attrs(s)["tellproof.prompt.version"] for s in spans), ["0", "1", "2"])

    def test_values_are_short_names_only(self):
        with Capturing() as spans:
            with tellproof.prompt("x" * 1000, {"not": "a version"}):
                with tellproof.llm_call("openai", "m"):
                    pass
        a = attrs(spans[0])
        self.assertEqual(len(a["tellproof.prompt.name"]), _core.MAX_VALUE)
        self.assertNotIn("tellproof.prompt.version", a)

    def test_usage_ignores_bad_counts(self):
        with Capturing() as spans:
            with tellproof.llm_call("openai", "m") as c:
                c.usage(input_tokens=-1, output_tokens="12")
                c.usage(input_tokens=True)
        self.assertNotIn("gen_ai.usage.input_tokens", attrs(spans[0]))
        self.assertNotIn("gen_ai.usage.output_tokens", attrs(spans[0]))

    def test_disabled_is_a_no_op(self):
        old = _core.current_client()
        _core.set_client(_core.Client())
        try:
            with tellproof.agent_run("a"), tellproof.guardrail("block") as g, tellproof.tool("t") as t:
                t.approved = True
                g.allow()
                tellproof.escalation()
            self.assertFalse(tellproof.is_enabled())
        finally:
            _core.set_client(old)

    def test_init_respects_tellproof_disabled(self):
        os.environ["TELLPROOF_DISABLED"] = "1"
        try:
            self.assertFalse(tellproof.init(transport=lambda b: None))
            self.assertFalse(tellproof.is_enabled())
        finally:
            del os.environ["TELLPROOF_DISABLED"]


class NoContentTest(unittest.TestCase):
    """Acceptance (S-1): no argument text reaches an exported span."""

    def test_random_text_never_reaches_an_export(self):
        rng = random.Random(42)
        exports = []
        exp = Exporter(exports.append, lambda spans: _otlp.encode(spans, {"service.name": "fuzz"}, "t"), interval=0.01)
        old = _core.current_client()
        _core.set_client(_core.Client(exp, service="fuzz"))
        markers = []
        try:
            for _ in range(300):
                marker = "SECRET" + "".join(rng.choices(string.ascii_letters + string.digits + " \"'\\\n{}", k=24))
                markers.append(marker)

                @tellproof.tool("t", high_impact=rng.random() < 0.5)
                def act(message, *, note):
                    if rng.random() < 0.3:
                        raise ValueError(message)
                    return message + note

                try:
                    with tellproof.agent_run("fuzz"), tellproof.guardrail() as g:
                        with tellproof.llm_call("openai", "m") as c:
                            c.usage(input_tokens=len(marker), output_tokens=1)
                            act(marker, note=marker)
                        g.decision = rng.choice(["allow", "block", "flag", marker])
                        tellproof.escalation()
                except ValueError:
                    pass
            self.assertTrue(exp.flush(5))
        finally:
            _core.set_client(old)
            exp.shutdown(1)
        blob = b"".join(exports).decode()
        self.assertGreater(blob.count('"spanId"'), 900)
        for m in markers:
            self.assertFalse(m[:12] in blob, f"argument text {m[:12]!r} reached an export")


class ExporterTest(unittest.TestCase):
    def test_overload_drops_instead_of_blocking(self):
        release = threading.Event()

        def slow(body):
            release.wait(5)

        exp = Exporter(slow, lambda spans: b"x", max_queue=100, batch_size=10, interval=0.01)
        t0 = time.monotonic()
        for i in range(10_000):
            exp.offer({"n": i})
        elapsed = time.monotonic() - t0
        release.set()
        self.assertLess(elapsed, 1.0, "offer() must never wait on the network")
        self.assertGreaterEqual(exp.dropped, 10_000 - 100 - 10)
        exp.shutdown(2)

    def test_failing_agent_is_counted_not_raised(self):
        def down(body):
            raise ConnectionRefusedError()

        exp = Exporter(down, lambda spans: b"x", interval=0.01)
        for i in range(5):
            exp.offer({"n": i})
        exp.flush(2)
        self.assertEqual((exp.export_errors, exp.dropped, exp.exported), (1, 5, 0))
        exp.shutdown(1)

    def test_flush_sends_everything_and_shutdown_stops(self):
        got = []
        exp = Exporter(lambda b: got.append(json.loads(b)), lambda spans: json.dumps(spans).encode(),
                       batch_size=7, interval=10)
        for i in range(50):
            exp.offer({"n": i})
        self.assertTrue(exp.flush(5))
        self.assertEqual(sum(len(b) for b in got), 50)
        self.assertTrue(all(len(b) <= 7 for b in got))
        exp.shutdown(1)
        exp.offer({"n": 99})
        self.assertEqual(exp.dropped, 1)

    def test_forked_child_starts_clean(self):
        if not hasattr(os, "fork"):
            self.skipTest("no fork")
        r, w = os.pipe()
        exp = Exporter(lambda b: os.write(w, b"%d\n" % len(json.loads(b))), lambda s: json.dumps(s).encode(),
                       interval=0.01)
        exp.offer({"parent": True})
        exp.flush(2)
        with warnings.catch_warnings():
            warnings.simplefilter("ignore", DeprecationWarning)  # the point is forking with a live exporter thread
            pid = os.fork()
        if pid == 0:  # child: the parent's buffer is gone, a fresh thread sends
            try:
                exp.offer({"child": 1})
                exp.offer({"child": 2})
                os._exit(0 if exp.flush(2) else 1)
            except BaseException:
                os._exit(2)
        _, status = os.waitpid(pid, 0)
        os.close(w)
        out = os.read(r, 100).decode().split()
        os.close(r)
        self.assertEqual(os.waitstatus_to_exitcode(status), 0)
        self.assertEqual(out, ["1", "2"])
        exp.shutdown(1)


class AgentEndToEndTest(unittest.TestCase):
    def test_init_sends_otlp_json_to_the_agent_url(self):
        received = []

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                body = self.rfile.read(int(self.headers["Content-Length"]))
                received.append((self.path, self.headers["Content-Type"], json.loads(body)))
                self.send_response(200)
                self.end_headers()

            def log_message(self, *a):
                pass

        srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        os.environ["HTTP_PROXY"] = "http://127.0.0.1:9"  # must be ignored
        try:
            self.assertTrue(tellproof.init(f"http://127.0.0.1:{srv.server_port}", service="e2e"))
            self.assertTrue(tellproof.init())  # idempotent
            with tellproof.llm_call("openai", "gpt-4.1-mini") as c:
                c.usage(5, 1)
            self.assertTrue(tellproof.flush(5))
            st = tellproof.stats()
        finally:
            tellproof.shutdown()
            del os.environ["HTTP_PROXY"]
            srv.shutdown()
            srv.server_close()
        path, ctype, body = received[0]
        self.assertEqual((path, ctype), ("/v1/traces", "application/json"))
        rs = body["resourceSpans"][0]
        self.assertIn({"key": "service.name", "value": {"stringValue": "e2e"}}, rs["resource"]["attributes"])
        self.assertEqual(len(rs["scopeSpans"][0]["spans"]), 1)
        self.assertEqual((st["exported"], st["dropped"]), (1, 0))
        self.assertFalse(tellproof.is_enabled())


class TransportTest(unittest.TestCase):
    def test_never_follows_a_redirect_and_takes_http_only(self):
        hits = []

        class Target(BaseHTTPRequestHandler):
            def do_POST(self):
                hits.append(self.path)
                self.send_response(200)
                self.end_headers()

            def log_message(self, *a):
                pass

        target = ThreadingHTTPServer(("127.0.0.1", 0), Target)

        class Redirect(BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", 0)))
                self.send_response(307)
                self.send_header("Location", f"http://127.0.0.1:{target.server_address[1]}/v1/traces")
                self.end_headers()

            def log_message(self, *a):
                pass

        redirect = ThreadingHTTPServer(("127.0.0.1", 0), Redirect)
        for s in (target, redirect):
            threading.Thread(target=s.serve_forever, daemon=True).start()
        try:
            with self.assertRaises(Exception) as ctx:
                http_transport(f"http://127.0.0.1:{redirect.server_address[1]}/v1/traces")(b"{}")
            if hasattr(ctx.exception, "close"):
                ctx.exception.close()  # an HTTPError holds the 307 response
            self.assertEqual(hits, [], "the spans followed a redirect")
        finally:
            for srv in (target, redirect):
                srv.shutdown()
                srv.server_close()
        for u in ("file:///etc/passwd", "ftp://127.0.0.1/", "gopher://x/"):
            with self.assertRaises(ValueError):
                http_transport(u)
        self.assertFalse(tellproof.init(agent_url="file:///tmp/x", instrument=False))
        self.assertFalse(tellproof.is_enabled())


if __name__ == "__main__":
    unittest.main()


class CanaryTest(unittest.TestCase):
    """Guardrail canaries (D-057): only the kind and the result leave the probe."""

    def test_probes_are_the_spec(self):
        with open(os.path.join(HERE, "..", "..", "..", "spec", "canaries.json")) as f:
            spec = json.load(f)
        self.assertEqual(tellproof.PROBES, {p["kind"]: p["text"] for p in spec["probes"]})

    def test_run_records_kind_and_result_only(self):
        seen = []

        def check(text):
            seen.append(text)
            if "AKIA" in text:
                raise RuntimeError("guardrail crashed")  # counts as missed
            return "block" if "instructions" in text else "pii" in text and "4111" in text

        with Capturing() as spans:
            with tellproof.personal_data():  # an app scope must not leak into canaries
                got = tellproof.run_canaries(check)
        self.assertEqual(got, {"prompt_injection": True, "jailbreak": False, "pii": False, "secret": False})
        self.assertEqual(seen, list(tellproof.PROBES.values()))
        self.assertEqual(len(spans), 4)
        for s in spans:
            self.assertEqual(set(attrs(s)), {"tellproof.canary.kind", "tellproof.canary.caught"})
            self.assertIsNone(s["parent"])
        self.assertEqual(len({s["trace_id"] for s in spans}), 4)
        dumped = json.dumps(spans)
        for text in tellproof.PROBES.values():
            self.assertNotIn(text[:20], dumped)

    def test_canary_and_kinds(self):
        with Capturing() as spans:
            tellproof.canary("Prompt_Injection", True)
            tellproof.canary("custom-check_2", "allow")
            tellproof.canary("has space", True)  # refused, counted as an SDK error
            tellproof.canary(None, True)
            got = tellproof.run_canaries(lambda t: True, kinds=["pii", "unknown"])
        self.assertEqual(got, {"pii": True})
        self.assertEqual([(attrs(s)["tellproof.canary.kind"], attrs(s)["tellproof.canary.caught"]) for s in spans],
                         [("prompt_injection", True), ("custom-check_2", False), ("pii", True)])

    def test_async_and_disabled(self):
        async def check(text):
            return True

        with Capturing() as spans:
            got = asyncio.run(tellproof.arun_canaries(check, kinds=["secret"]))
            missed = tellproof.run_canaries(check, kinds=["secret"])  # sync runner, async check: missed
        self.assertEqual((got, missed), ({"secret": True}, {"secret": False}))
        self.assertEqual(len(spans), 2)
        calls = []
        old = _core.current_client()
        _core.set_client(_core.Client())  # off: the guardrail is not called at all
        try:
            self.assertEqual(tellproof.run_canaries(lambda t: calls.append(t)), {})
        finally:
            _core.set_client(old)
        self.assertEqual(calls, [])

    def test_schedule(self):
        ran = threading.Event()

        def check(text):
            ran.set()
            return True

        with Capturing() as spans:
            s = tellproof.schedule_canaries(check, every=0, kinds=["pii"])
            self.assertTrue(ran.wait(2))
            s.stop()
            s._thread.join(2)
        self.assertFalse(s._thread.is_alive())
        self.assertEqual(s._every, 60.0)
        self.assertGreaterEqual(len(spans), 1)
