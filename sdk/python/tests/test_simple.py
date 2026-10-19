"""The simpler usage (D-069): init() defaults, request scopes for web
frameworks, @guardrail_check, @redactor, bare @tool and @agent_run,
approve(), automatic canaries, and guardrails detected without code
(OpenAI moderation, Bedrock ApplyGuardrail, Azure OpenAI content filters,
OpenAI stored prompts). None of it may read or export content.
"""

import asyncio
import json
import os
import sys
import threading
import time
import unittest
from types import SimpleNamespace as NS

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "src"))
sys.path.insert(0, HERE)

import tellproof  # noqa: E402
from tellproof import _canary, _core, _instrument  # noqa: E402
import fakes  # noqa: E402
import scenario  # noqa: E402

MARK = fakes.MARKER
GUARD, DATA, REDACTED = _core.GUARDRAIL, _core.DATA_CLASS, _core.REDACTED


class Base(unittest.TestCase):
    """A capturing client, fake client libraries instrumented, and no
    registered checks or defaults left over between tests."""

    def setUp(self):
        self.mods = fakes.installed(fakes.make_openai(), fakes.make_anthropic(), fakes.make_botocore(), self.frameworks())
        self.m = self.mods.__enter__()
        self.addCleanup(self.mods.__exit__)
        _instrument.install(requests=True)
        self.cap = scenario.Capture()
        old = _core.current_client()
        _core.set_client(_core.Client(self.cap, service="test"))
        self.addCleanup(_core.set_client, old)
        saved = list(_core._checks)
        self.addCleanup(lambda: _core._checks.__setitem__(slice(None), saved))
        _core._checks.clear()
        self.addCleanup(_core.configure)
        self.addCleanup(_canary.stop_auto)

    def frameworks(self):
        return {}

    def llm(self):
        return [s for s in self.cap.spans if s["kind"] == 3]

    def chat(self, model="gpt-4.1-mini", client=None):
        c = client or self.m["openai"].OpenAI()
        return c.chat.completions.create(model=model, messages=[{"role": "user", "content": MARK}])

    def assertNoContent(self):
        dump = json.dumps(self.cap.spans)
        self.assertNotIn(MARK, dump)
        self.assertNotIn("FLAG", dump)


class Defaults(Base):
    def test_init_names_every_call_unless_a_scope_does(self):
        _core.configure(agent="triage", prompt="claims", prompt_version="14")
        self.chat()
        with tellproof.prompt("claims", "15"):
            self.chat()
        a, b = self.llm()
        self.assertEqual((a["attrs"][_core.AGENT_NAME], a["attrs"][_core.PROMPT_NAME], a["attrs"][_core.PROMPT_VERSION]),
                         ("triage", "claims", "14"))
        self.assertEqual(b["attrs"][_core.PROMPT_VERSION], "15")

    def test_personal_data_is_unredacted_until_a_redactor_runs_in_the_request(self):
        _core.configure(personal_data=True)

        @tellproof.redactor
        def redact(messages):
            return [m.replace(MARK, "[x]") for m in messages]

        with tellproof.trace():
            self.chat()
        with tellproof.trace():
            redact([MARK])
            self.chat()
        a, b = self.llm()
        self.assertEqual((a["attrs"][DATA], a["attrs"][REDACTED]), ("personal", False))
        self.assertEqual((b["attrs"][DATA], b["attrs"][REDACTED]), ("personal", True))
        self.assertNoContent()

    def test_a_redactor_alone_marks_its_request_personal_and_redacted(self):
        @tellproof.redactor
        async def redact(text):
            return "[x]"

        async def request():
            async with tellproof.trace():
                await redact(MARK)
                self.chat()
        asyncio.run(request())
        self.chat()  # another request: nothing carried over
        a, b = self.llm()
        self.assertEqual((a["attrs"][DATA], a["attrs"][REDACTED]), ("personal", True))
        self.assertNotIn(DATA, b["attrs"])

    def test_a_redaction_in_a_worker_thread_counts_for_its_request(self):
        @tellproof.redactor
        def redact(text):
            return "[x]"

        with tellproof.trace():
            ctx = __import__("contextvars").copy_context()
            t = threading.Thread(target=ctx.run, args=(redact, MARK))
            t.start()
            t.join()
            self.chat()
        self.assertTrue(self.llm()[0]["attrs"][REDACTED])

    def test_outside_a_request_nothing_is_attributed(self):
        @tellproof.redactor
        def redact(text):
            return "[x]"

        @tellproof.guardrail_check(canaries=False)
        def check(text):
            return False

        redact(MARK)
        check(MARK)
        self.chat()
        (s,) = self.llm()
        self.assertNotIn(DATA, s["attrs"])
        self.assertNotIn(GUARD, s["attrs"])
        self.assertEqual(_core.current_client().stats.unscoped, 2)


class Checks(Base):
    def setUp(self):
        super().setUp()

        @tellproof.guardrail_check(canaries=False)
        def input_check(text):
            return "FLAG" in text

        @tellproof.guardrail_check(canaries=False)
        def output_check(text):
            return "flag" if "FLAG" in text else "allow"

        self.input_check, self.output_check = input_check, output_check

    def test_the_strictest_decision_in_a_request_applies_to_its_calls(self):
        with tellproof.trace():
            self.input_check(MARK)
            self.chat()
            self.output_check("FLAG " + MARK)
            self.chat()
        with tellproof.trace():
            self.input_check(MARK)
            self.chat()
        with tellproof.trace():
            self.chat()  # no check ran: no decision
        decisions = [s["attrs"].get(GUARD) for s in self.llm()]
        self.assertEqual(decisions, ["flag", "flag", "allow", None])
        self.assertNoContent()

    def test_a_block_with_no_model_call_is_one_blocked_call(self):
        with tellproof.trace():
            if self.input_check("FLAG " + MARK):
                pass  # refused: the model is never called
        (s,) = self.llm()
        self.assertEqual(s["attrs"][GUARD], "block")
        self.input_check("FLAG")  # outside a request too
        self.assertEqual([x["attrs"][GUARD] for x in self.llm()], ["block", "block"])

    def test_the_check_returns_and_raises_as_before(self):
        @tellproof.guardrail_check(canaries=False)
        def raising(text):
            raise ValueError(MARK)

        with tellproof.trace():
            self.assertTrue(self.input_check("FLAG"))
            with self.assertRaises(ValueError):
                raising("x")
            self.chat()
        self.assertEqual(self.llm()[0]["attrs"][GUARD], "block")

    def test_an_explicit_guardrail_scope_still_works_inside(self):
        with tellproof.trace():
            with tellproof.guardrail() as g:
                self.chat()
                g.allow()
            self.input_check("FLAG")
        (a,) = self.llm()  # the call keeps its scope's decision; the request had a call, so no blocked call
        self.assertEqual(a["attrs"][GUARD], "allow")


def canary_kinds(cap):
    return sorted(s["attrs"][_canary.CANARY_KIND] for s in cap.spans if _canary.CANARY_KIND in s["attrs"])


def wait_canaries(cap, n=len(_canary.PROBES)):
    deadline = time.time() + 5
    while len(canary_kinds(cap)) < n and time.time() < deadline:
        time.sleep(0.01)


class Canaries(Base):
    def test_a_check_gets_canaries_from_its_first_call(self):
        seen = []

        @tellproof.guardrail_check
        def check(text):
            seen.append(text)
            return True

        @tellproof.guardrail_check(canaries=False)
        def output_only(text):
            return False

        _canary.start_auto(3600)
        time.sleep(0.05)
        self.assertEqual(canary_kinds(self.cap), [], "canaries before the check ever ran")
        with tellproof.trace():
            check(MARK)
            output_only(MARK)
        wait_canaries(self.cap)
        self.assertEqual(canary_kinds(self.cap), sorted(_canary.PROBES))
        self.assertTrue(all(s["attrs"][_canary.CANARY_CAUGHT] for s in self.cap.spans if _canary.CANARY_KIND in s["attrs"]))
        self.assertEqual(sorted(t for t in seen if t != MARK), sorted(_canary.PROBES.values()))
        self.assertEqual([s["attrs"][GUARD] for s in self.llm()], ["block"], "a canary became a decision")

    def test_a_method_check_gets_its_instance(self):
        class Guard:
            def __init__(self):
                self.blocked = ("Ignore", "game", "card", "AWS")

            @tellproof.guardrail_check
            def check(self, text):
                return any(w in text for w in self.blocked)

        _canary.start_auto(3600)
        Guard().check(MARK)
        wait_canaries(self.cap)
        caught = {s["attrs"][_canary.CANARY_KIND]: s["attrs"][_canary.CANARY_CAUGHT] for s in self.cap.spans
                  if _canary.CANARY_KIND in s["attrs"]}
        self.assertEqual(caught, {k: True for k in _canary.PROBES}, "probes went to an unbound method and were missed")

    def test_an_async_check_runs_its_canaries_on_the_apps_loop_and_again_after_a_restart(self):
        @tellproof.guardrail_check
        async def check(text):
            await asyncio.sleep(0)
            return "block"

        async def app():
            async with tellproof.trace():
                await check(MARK)
            for _ in range(100):
                if len(canary_kinds(self.cap)) >= len(_canary.PROBES):
                    break
                await asyncio.sleep(0.01)

        _canary.start_auto(3600)
        asyncio.run(app())
        self.assertEqual(canary_kinds(self.cap), sorted(_canary.PROBES))
        _canary.stop_auto()  # shutdown(), then init() again
        _canary.start_auto(3600)
        self.cap.spans.clear()
        asyncio.run(app())  # a new loop: the old one closed
        self.assertEqual(canary_kinds(self.cap), sorted(_canary.PROBES))

    def test_canaries_off(self):
        @tellproof.guardrail_check
        def check(text):
            return True
        with tellproof.trace():
            check(MARK)
        time.sleep(0.1)  # start_auto never ran: init(canaries=False)
        self.assertEqual(canary_kinds(self.cap), [])

    def test_a_check_is_registered_once_and_at_most_32(self):
        def check(text):
            return False
        a, b = tellproof.guardrail_check(check), tellproof.guardrail_check(check)
        self.assertEqual(len(_core._checks), 1)
        for _ in range(40):
            tellproof.guardrail_check(lambda t: False)  # a check wrapped per call: the leak this guards against
        self.assertEqual(len(_core._checks), _core.MAX_CHECKS)
        with tellproof.trace():
            tellproof.guardrail_check(lambda t: True)(MARK)  # unregistered, its decision still counts
        self.assertEqual([s["attrs"][GUARD] for s in self.llm()], ["block"])


class Awaitables(Base):
    def test_a_sync_check_that_returns_an_awaitable_keeps_it_and_decides_when_it_resolves(self):
        async def scan(text):
            await asyncio.sleep(0)
            return "FLAG" in text

        @tellproof.guardrail_check(canaries=False)
        def check(text):
            return scan(text)  # a coroutine from a plain def

        @tellproof.guardrail_check(canaries=False)
        def check_task(text):
            return asyncio.ensure_future(scan(text))

        async def app():
            async with tellproof.trace():
                self.assertFalse(await check(MARK))
                self.chat()
            async with tellproof.trace():
                t = check_task("FLAG")
                self.assertIsInstance(t, asyncio.Task)  # the app's own object comes back
                self.assertTrue(await t)
                await asyncio.sleep(0)
                self.chat()
        asyncio.run(app())
        self.assertEqual([s["attrs"].get(GUARD) for s in self.llm()], ["allow", "block"])


class Tools(Base):
    def test_bare_decorators_name_after_the_function_and_approve(self):
        @tellproof.tool
        def lookup(claim):
            return claim

        @tellproof.tool(high_impact=True)
        def issue_refund(claim):
            tellproof.approve()
            return claim

        @tellproof.agent_run
        def triage(claim):
            lookup(claim)
            issue_refund(claim)
            self.chat()

        triage(MARK)
        by = {s["attrs"].get(_core.TOOL_NAME) or s["name"]: s["attrs"] for s in self.cap.spans}
        self.assertEqual(by["lookup"][_core.HIGH_IMPACT], False)
        self.assertEqual((by["issue_refund"][_core.HIGH_IMPACT], by["issue_refund"][_core.APPROVED]), (True, True))
        self.assertEqual(by["invoke_agent"][_core.AGENT_NAME], "triage")
        self.assertEqual(self.llm()[0]["attrs"][_core.AGENT_NAME], "triage")
        self.assertNoContent()


class Detected(Base):
    def test_openai_moderation_is_a_guardrail(self):
        c = self.m["openai"].OpenAI()
        with tellproof.trace():
            c.moderations.create(input=MARK)
            self.chat()
        with tellproof.trace():
            c.moderations.create(input="FLAG " + MARK)
            self.chat()

        async def a():
            ac = self.m["openai"].AsyncOpenAI()
            async with tellproof.trace():
                await ac.moderations.create(input=MARK)
                await ac.chat.completions.create(model="gpt-4.1-mini", messages=[])
        asyncio.run(a())
        self.assertEqual([s["attrs"].get(GUARD) for s in self.llm()], ["allow", "flag", "allow"])
        self.assertNoContent()

    def test_bedrock_apply_guardrail_is_a_guardrail(self):
        br = self.m["botocore.client"].BaseClient("bedrock-runtime", "ap-southeast-2")
        with tellproof.trace():
            br.apply_guardrail(guardrailIdentifier="gr-1", guardrailVersion="1", source="INPUT", content=[{"text": {"text": MARK}}])
            br.converse(modelId="anthropic.claude-sonnet-4-6", messages=[])
        with tellproof.trace():
            br.apply_guardrail(guardrailIdentifier="gr-1", guardrailVersion="1", source="INPUT", content=[], _intervene=True)
        self.assertEqual([s["attrs"].get(GUARD) for s in self.llm()], ["allow", "block"])
        self.assertEqual(len(self.llm()), 2, "ApplyGuardrail itself is not a model call")
        self.assertNoContent()

    def test_azure_openai_content_filter(self):
        az = self.m["openai"].AzureOpenAI()
        self.chat(client=az)
        self.chat(model="filtered", client=az)
        with self.assertRaises(fakes.APIError):
            self.chat(model="refused", client=az)
        self.chat()  # api.openai.com: no filter results, no decision
        self.assertEqual([s["attrs"].get(GUARD) for s in self.llm()], ["allow", "block", "block", None])
        self.assertNoContent()

    def test_openai_stored_prompt_is_named_by_reference(self):
        c = self.m["openai"].OpenAI()
        c.responses.create(model="gpt-4.1", input=MARK, prompt={"id": "pmpt_123", "version": "4", "variables": {"x": MARK}})
        c.responses.create(model="gpt-4.1", input=MARK)
        a, b = self.llm()
        self.assertEqual((a["attrs"][_core.PROMPT_NAME], a["attrs"][_core.PROMPT_VERSION]), ("pmpt_123", "4"))
        self.assertNotIn(_core.PROMPT_NAME, b["attrs"])
        self.assertNoContent()


def fake_frameworks(calls):
    """Flask, Starlette (FastAPI) and Django with their real entry points'
    shapes; each handler runs calls(request_number)."""
    m = fakes._module

    class Flask:
        n = 0

        def wsgi_app(self, environ, start_response):
            Flask.n += 1
            calls(Flask.n)
            return [b"ok"]

    class Starlette:
        n = 0

        async def __call__(self, scope, receive, send):
            if scope["type"] == "http":
                Starlette.n += 1
                calls(Starlette.n)

    class BaseHandler:
        n = 0

        def get_response(self, request):
            BaseHandler.n += 1
            calls(BaseHandler.n)
            return "response"

        async def get_response_async(self, request):
            BaseHandler.n += 1
            calls(BaseHandler.n)
            return "response"

    return {"flask": m("flask", Flask=Flask), "flask.app": m("flask.app", Flask=Flask),
            "starlette": m("starlette"), "starlette.applications": m("starlette.applications", Starlette=Starlette),
            "django": m("django"), "django.core": m("django.core"), "django.core.handlers": m("django.core.handlers"),
            "django.core.handlers.base": m("django.core.handlers.base", BaseHandler=BaseHandler)}


class Frameworks(Base):
    def frameworks(self):
        def calls(n):
            if n % 2:  # odd requests run the check; even ones do not
                self.check("FLAG" if n == 3 else MARK)
            self.chat()
        return fake_frameworks(calls)

    def setUp(self):
        super().setUp()

        @tellproof.guardrail_check(canaries=False)
        def check(text):
            return "flag" if "FLAG" in text else False
        self.check = check

    def test_every_request_is_its_own_scope(self):
        self.assertEqual(_instrument.install(requests=True)[-3:], ["flask", "starlette", "django"])
        flask = self.m["flask.app"].Flask()
        for _ in range(4):
            flask.wsgi_app({}, None)  # one thread, four requests: nothing leaks between them
        app = self.m["starlette.applications"].Starlette()

        async def serve():
            await app({"type": "lifespan"}, None, None)
            for _ in range(2):
                await app({"type": "http"}, None, None)
        asyncio.run(serve())
        h = self.m["django.core.handlers.base"].BaseHandler()
        h.get_response(NS())
        asyncio.run(h.get_response_async(NS()))
        spans = self.llm()
        self.assertEqual([s["attrs"].get(GUARD) for s in spans],
                         ["allow", None, "flag", None, "allow", None, "allow", None])
        self.assertEqual(len({s["trace_id"] for s in spans}), len(spans), "requests shared a trace")
        self.assertNoContent()

    def test_requests_off_leaves_frameworks_alone(self):
        mods = fakes.installed(fake_frameworks(lambda n: None))
        m = mods.__enter__()
        try:
            self.assertEqual(_instrument.install(requests=False), ["openai", "anthropic", "bedrock"])
            self.assertFalse(hasattr(m["flask.app"].Flask.wsgi_app, _instrument._MARK))
        finally:
            mods.__exit__()


class Streaming(Base):
    def frameworks(self):
        test = self

        class Flask:
            def wsgi_app(self, environ, start_response):
                def body():  # a streamed response: the check and the call happen while it is sent
                    test.check("FLAG")
                    test.chat()
                    yield b"chunk"
                return body()
        return {"flask": fakes._module("flask", Flask=Flask), "flask.app": fakes._module("flask.app", Flask=Flask)}

    def setUp(self):
        super().setUp()
        self.check = tellproof.guardrail_check(lambda t: "flag" if "FLAG" in t else False, canaries=False)

    def test_a_streamed_flask_response_stays_in_its_request(self):
        body = self.m["flask.app"].Flask().wsgi_app({}, None)
        self.assertEqual(next(body), b"chunk")
        self.assertEqual(self.llm(), [], "the call left before the response was sent")
        self.assertEqual(list(body), [])  # sent in full: the request ends here, close() or not
        self.assertEqual([s["attrs"].get(GUARD) for s in self.llm()], ["flag"])
        body.close()
        self.assertEqual(len(self.llm()), 1)
        self.assertEqual(_core.current_client().stats.unscoped, 0)


class Compatibility(Base):
    def test_the_helpers_are_still_classes(self):
        class AuditedTool(tellproof.tool):
            pass

        with AuditedTool("audited") as t:
            self.assertIsInstance(t, tellproof.tool)
        self.assertIsInstance(tellproof.trace(), tellproof.trace)
        self.assertIsInstance(tellproof.agent_run("a"), tellproof.agent_run)

        @tellproof.trace
        def job():
            return 7
        self.assertEqual(job(), 7)


class Init(unittest.TestCase):
    def test_init_options_and_env(self):
        bodies = []
        env = {"TELLPROOF_PROMPT": "claims", "TELLPROOF_PROMPT_VERSION": "7", "TELLPROOF_PERSONAL_DATA": "1"}
        old = {k: os.environ.get(k) for k in env}
        os.environ.update(env)
        try:
            self.assertTrue(tellproof.init(service="t", agent="triage", transport=bodies.append, canaries=False,
                                           instrument=False))
            with tellproof.trace():
                with tellproof.llm_call("acme", "m1") as c:
                    c.usage(1, 1)
            self.assertTrue(tellproof.flush(5))
            self.assertEqual(tellproof.stats()["unscoped"], 0)
        finally:
            tellproof.shutdown()
            _core.configure()
            for k, v in old.items():
                if v is None:
                    os.environ.pop(k, None)
                else:
                    os.environ[k] = v
        span = json.loads(bodies[0])["resourceSpans"][0]["scopeSpans"][0]["spans"][0]
        attrs = {a["key"]: list(a["value"].values())[0] for a in span["attributes"]}
        self.assertEqual((attrs[_core.AGENT_NAME], attrs[_core.PROMPT_NAME], attrs[_core.PROMPT_VERSION], attrs[DATA],
                          attrs[REDACTED]), ("triage", "claims", "7", "personal", False))


if __name__ == "__main__":
    unittest.main()
