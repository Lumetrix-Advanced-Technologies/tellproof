import asyncio
import gc
import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "src"))
sys.path.insert(0, HERE)

import plumbline  # noqa: E402
from plumbline import _core, _instrument, _otlp  # noqa: E402
from plumbline._export import Exporter  # noqa: E402
import fakes  # noqa: E402
import scenario  # noqa: E402


class Instrumented(unittest.TestCase):
    """Each test gets fresh fake libraries, instrumented, and a capturing client."""

    def setUp(self):
        self.mods = fakes.installed(fakes.make_openai(), fakes.make_anthropic(), fakes.make_botocore())
        self.m = self.mods.__enter__()
        self.addCleanup(self.mods.__exit__)
        self.assertEqual(_instrument.install(), ["openai", "anthropic", "bedrock"])
        self.cap = scenario.Capture()
        old = _core.current_client()
        _core.set_client(_core.Client(self.cap, service="test"))
        self.addCleanup(_core.set_client, old)
        self.addCleanup(setattr, _instrument, "_stream_usage", False)

    @property
    def spans(self):
        return self.cap.spans

    def only(self):
        self.assertEqual(len(self.spans), 1, [s["attrs"] for s in self.spans])
        return self.spans[0]["attrs"], self.spans[0]

    def openai(self, **kw):
        return self.m["openai"].OpenAI(**kw)

    def anthropic(self):
        return self.m["anthropic"].Anthropic()

    def bedrock(self, service="bedrock-runtime"):
        return self.m["botocore.client"].BaseClient(service, "ap-southeast-2")


def facts(attrs):
    keys = ("gen_ai.provider.name", "gen_ai.operation.name", "gen_ai.request.model", "gen_ai.response.model",
            "cloud.region", "gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "plumbline.guardrail.decision")
    return {k.rsplit(".", 1)[-1] if k != "gen_ai.provider.name" else "provider": attrs[k] for k in keys if k in attrs}


class OpenAITest(Instrumented):
    def test_chat_completion(self):
        r = self.openai().chat.completions.create(model="gpt-4.1-mini", messages=[{"role": "user", "content": "hi"}])
        self.assertEqual(r.usage.prompt_tokens, 120)  # the app gets the real result
        a, s = self.only()
        self.assertEqual(facts(a), {"provider": "openai", "name": "chat", "model": "gpt-4.1-mini-2025-04-14",
                                    "input_tokens": 120, "output_tokens": 30})
        self.assertEqual(a["gen_ai.request.model"], "gpt-4.1-mini")
        self.assertEqual((s["kind"], s["error"]), (3, False))

    def test_parse_responses_embeddings_and_legacy_completions(self):
        c = self.openai()
        c.chat.completions.parse(model="gpt-4.1", messages=[])
        c.responses.create(model="gpt-5", input="hi")
        c.responses.parse(model="gpt-5", input="hi")
        c.embeddings.create(model="text-embedding-3-small", input=["a"])
        c.completions.create(model="gpt-3.5-turbo-instruct", prompt="x")
        got = [(s["attrs"]["gen_ai.operation.name"], s["attrs"].get("gen_ai.usage.input_tokens"),
                s["attrs"].get("gen_ai.usage.output_tokens")) for s in self.spans]
        self.assertEqual(got, [("chat", 120, 30), ("chat", 80, 12), ("chat", 80, 12), ("embeddings", 9, None),
                               ("text_completion", 4, 2)])

    def test_streamed_chat_counts_usage_once(self):
        _instrument._stream_usage = True
        stream = self.openai().chat.completions.create(model="gpt-4.1-mini", messages=[], stream=True)
        self.assertIsInstance(stream, fakes.Stream)
        self.assertEqual(self.spans, [], "the span waits for the stream to end")
        n = sum(1 for _ in stream)
        self.assertEqual(n, 3)  # two content chunks and the usage chunk
        stream.close()
        a, _ = self.only()
        self.assertEqual((a["gen_ai.usage.input_tokens"], a["gen_ai.usage.output_tokens"]), (120, 30))

    def test_stream_usage_is_opt_in_and_never_overrides_the_app(self):
        c = self.openai()
        with c.chat.completions.create(model="m", messages=[], stream=True) as s:
            self.assertEqual(sum(1 for _ in s), 2)  # off by default: no extra chunk
        _instrument._stream_usage = True
        with c.chat.completions.create(model="m", messages=[], stream=True, stream_options={"include_usage": False}) as s:
            self.assertEqual(sum(1 for _ in s), 2)
        self.assertEqual(len(self.spans), 2)
        self.assertTrue(all("gen_ai.usage.input_tokens" not in s["attrs"] for s in self.spans))

        class Omit:  # what openai's stream() helper passes for an argument the app left out
            pass
        with c.chat.completions.create(model="m", messages=[], stream=True, stream_options=Omit()) as s:
            pass
        self.assertEqual(sum(1 for _ in s), 3)

    def test_stream_stopped_early_records_one_call(self):
        with self.openai().chat.completions.create(model="m", messages=[], stream=True) as s:
            next(s)
        self.assertEqual(len(self.spans), 1)

    def test_abandoned_stream_is_recorded_later(self):
        s = self.openai().chat.completions.create(model="m", messages=[], stream=True)
        next(s)
        del s
        gc.collect()
        self.assertEqual(self.spans, [])  # never recorded inside a finalizer
        _core.run_later()
        self.assertEqual(len(self.spans), 1)

    def test_responses_stream_and_failure(self):
        c = self.openai()
        for _ in c.responses.create(model="gpt-5", input="x", stream=True):
            pass
        for _ in c.responses.create(model="gpt-5", input="x", stream=True, _final="response.failed"):
            pass
        ok, failed = self.spans
        self.assertEqual((ok["attrs"]["gen_ai.usage.input_tokens"], ok["error"]), (80, False))
        self.assertTrue(failed["error"])

    def test_errors_are_recorded_and_reraised_unchanged(self):
        with self.assertRaises(fakes.APIError) as cm:
            self.openai().chat.completions.create(model="boom", messages=[])
        self.assertIn(fakes.MARKER, str(cm.exception))
        a, s = self.only()
        self.assertTrue(s["error"])
        self.assertNotIn(fakes.MARKER, json.dumps(s))

    def test_async_create_and_stream(self):
        async def main():
            c = self.m["openai"].AsyncOpenAI()
            await c.chat.completions.create(model="gpt-4.1", messages=[])
            _instrument._stream_usage = True
            s = await c.chat.completions.create(model="gpt-4.1", messages=[], stream=True)
            async with s:
                async for _ in s:
                    pass
            await c.responses.create(model="gpt-5", input="x")
            async for _ in await c.responses.create(model="gpt-5", input="x", stream=True):
                pass
            await c.embeddings.create(model="e", input="x")

        asyncio.run(main())
        got = [(s["attrs"]["gen_ai.operation.name"], s["attrs"].get("gen_ai.usage.input_tokens")) for s in self.spans]
        self.assertEqual(got, [("chat", 120), ("chat", 120), ("chat", 80), ("chat", 80), ("embeddings", 9)])

    def test_provider_names(self):
        self.m["openai"].AzureOpenAI().chat.completions.create(model="gpt-4o", messages=[])
        self.openai(base_url="https://api.groq.com/openai/v1").chat.completions.create(model="llama", messages=[])
        self.assertEqual([s["attrs"]["gen_ai.provider.name"] for s in self.spans], ["azure.ai.openai", "api.groq.com"])


class AnthropicTest(Instrumented):
    def test_message_counts_cached_input(self):
        self.anthropic().messages.create(model="claude-sonnet-4-5", max_tokens=100, messages=[])
        a, _ = self.only()
        # input = uncached 10 + cache writes 5 + cache reads 100 (D-028)
        self.assertEqual(facts(a), {"provider": "anthropic", "name": "chat", "model": "claude-sonnet-4-5-20250929",
                                    "input_tokens": 115, "output_tokens": 20})

    def test_raw_stream_counts_usage_once(self):
        s = self.anthropic().messages.create(model="claude-sonnet-4-5", max_tokens=100, messages=[], stream=True)
        for _ in s:
            pass
        a, _ = self.only()
        self.assertEqual((a["gen_ai.usage.input_tokens"], a["gen_ai.usage.output_tokens"]), (115, 20))

    def test_stream_helper(self):
        c = self.anthropic()
        with c.messages.stream(model="claude-haiku-4-5", max_tokens=10, messages=[]) as stream:
            text = "".join(stream.text_stream)
        self.assertIn(fakes.MARKER, text)
        with c.messages.stream(model="claude-haiku-4-5", max_tokens=10, messages=[]) as stream:
            stream.get_final_message()
        self.assertEqual(len(self.spans), 2)
        for s in self.spans:
            self.assertEqual((s["attrs"]["gen_ai.usage.input_tokens"], s["attrs"]["gen_ai.usage.output_tokens"]), (115, 20))

    def test_async(self):
        async def main():
            c = self.m["anthropic"].AsyncAnthropic()
            await c.messages.create(model="claude", max_tokens=1, messages=[])
            async for _ in await c.messages.create(model="claude", max_tokens=1, messages=[], stream=True):
                pass
            async with c.messages.stream(model="claude", max_tokens=1, messages=[]) as stream:
                async for _ in stream:
                    pass

        asyncio.run(main())
        self.assertEqual([s["attrs"]["gen_ai.usage.input_tokens"] for s in self.spans], [115, 115, 115])

    def test_bedrock_client_and_beta_messages(self):
        self.m["anthropic"].AnthropicBedrock().messages.create(model="anthropic.claude-sonnet-4-5", max_tokens=1, messages=[])
        self.anthropic().beta.messages.create(model="claude", max_tokens=1, messages=[])
        b, beta = self.spans
        self.assertEqual((b["attrs"]["gen_ai.provider.name"], b["attrs"]["cloud.region"]), ("aws.bedrock", "ap-southeast-2"))
        self.assertEqual(beta["attrs"]["gen_ai.provider.name"], "anthropic")

    def test_error(self):
        with self.assertRaises(fakes.APIError):
            self.anthropic().messages.create(model="boom", max_tokens=1, messages=[])
        self.assertTrue(self.only()[1]["error"])


class BedrockTest(Instrumented):
    def test_converse_counts_billed_input_like_cloudwatch(self):
        self.bedrock().converse(modelId="apac.anthropic.claude-sonnet-4-5", messages=[])
        a, _ = self.only()
        # inputTokens excludes cache reads, as CloudWatch InputTokenCount does (D-028).
        self.assertEqual(facts(a), {"provider": "aws.bedrock", "name": "chat", "model": "apac.anthropic.claude-sonnet-4-5",
                                    "region": "ap-southeast-2", "input_tokens": 50, "output_tokens": 9})

    def test_guardrail_decision_from_stop_reason(self):
        c = self.bedrock()
        c.converse(modelId="m", messages=[])  # no guardrail configured: not a guarded call
        c.converse(modelId="m", messages=[], guardrailConfig={"guardrailIdentifier": "g", "guardrailVersion": "1"})
        c.converse(modelId="m", messages=[], guardrailConfig={"guardrailIdentifier": "g"}, _intervene=True)
        for _ in c.converse_stream(modelId="m", messages=[], guardrailConfig={}, _intervene=True)["stream"]:
            pass
        self.assertEqual([s["attrs"].get("plumbline.guardrail.decision") for s in self.spans], [None, "allow", "block", "block"])

    def test_a_provider_block_beats_an_enclosing_scope(self):
        with plumbline.guardrail("flag"):
            self.bedrock().converse(modelId="m", messages=[], guardrailConfig={}, _intervene=True)
        self.assertEqual(self.only()[0]["plumbline.guardrail.decision"], "block")

    def test_stream_invoke_and_other_services(self):
        c = self.bedrock()
        resp = c.converse_stream(modelId="m", messages=[])
        events = list(resp["stream"])
        self.assertEqual(len(events), 4)
        body = c.invoke_model(modelId="anthropic.claude", body=b"{}")["body"].read()
        self.assertIn(fakes.MARKER.encode(), body)  # the app still reads its body
        for _ in c.invoke_model_with_response_stream(modelId="m", body=b"{}")["body"]:
            pass
        self.bedrock("s3").list_objects_v2(Bucket="b")
        got = [(s["attrs"].get("gen_ai.usage.input_tokens"), s["attrs"].get("gen_ai.usage.output_tokens")) for s in self.spans]
        self.assertEqual(got, [(50, 9), (42, 7), (None, None)])

    def test_error(self):
        with self.assertRaises(Exception):
            self.bedrock().converse(modelId="boom", messages=[])
        self.assertTrue(self.only()[1]["error"])


class ScopeTest(Instrumented):
    def test_calls_carry_scopes_and_late_streams_keep_their_context(self):
        with plumbline.agent_run("claims"), plumbline.prompt("triage", "3"):
            with plumbline.guardrail() as g:
                stream = self.openai().chat.completions.create(model="m", messages=[], stream=True)
                g.allow()
        self.assertEqual(len(self.spans), 1)  # only the agent run so far
        for _ in stream:  # consumed after every scope has ended
            pass
        call = self.spans[1]
        self.assertEqual(call["trace_id"], self.spans[0]["trace_id"])
        self.assertEqual(call["parent"], self.spans[0]["span_id"])
        self.assertEqual((call["attrs"]["gen_ai.agent.name"], call["attrs"]["plumbline.prompt.version"],
                          call["attrs"]["plumbline.guardrail.decision"]), ("claims", "3", "allow"))

    def test_install_twice_wraps_once(self):
        _instrument.install()
        self.openai().chat.completions.create(model="m", messages=[])
        self.assertEqual(len(self.spans), 1)

    def test_disabled_passes_straight_through(self):
        _core.set_client(_core.Client())
        r = self.openai().chat.completions.create(model="m", messages=[], stream=True)
        self.assertIs(type(r), fakes.Stream)

    def test_instrumentation_bugs_never_reach_the_app(self):
        orig = _instrument._openai_usage

        def broken(call, usage):
            raise RuntimeError("bug")

        _instrument._openai_usage = broken
        try:
            r = self.openai().chat.completions.create(model="m", messages=[])
        finally:
            _instrument._openai_usage = orig
        self.assertEqual(r.usage.prompt_tokens, 120)
        self.assertEqual(len(self.spans), 1)  # recorded without usage
        self.assertEqual(_core.current_client().stats.instrumentation_errors, 1)


class NoContentTest(unittest.TestCase):
    """Acceptance (S-2): no message content from any client reaches an export."""

    def test_markers_never_reach_an_export(self):
        marker = "MARKER-7f3a9c\u00e9\"\\</"
        exports = []
        exp = Exporter(exports.append, lambda spans: _otlp.encode(spans, {"service.name": "t"}, "t"), interval=0.01)
        with fakes.installed(fakes.make_openai(marker), fakes.make_anthropic(marker), fakes.make_botocore(marker)) as m:
            _instrument.install(stream_usage=True)
            old = _core.current_client()
            _core.set_client(_core.Client(exp, service="t"))
            try:
                o, a = m["openai"].OpenAI(), m["anthropic"].Anthropic()
                b = m["botocore.client"].BaseClient("bedrock-runtime", "us-east-1")
                msgs = [{"role": "user", "content": marker}]
                o.chat.completions.create(model="m", messages=msgs)
                list(o.chat.completions.create(model="m", messages=msgs, stream=True))
                list(o.responses.create(model="m", input=marker, stream=True))
                o.embeddings.create(model="m", input=[marker])
                a.messages.create(model="m", max_tokens=1, messages=msgs)
                list(a.messages.create(model="m", max_tokens=1, messages=msgs, stream=True))
                with a.messages.stream(model="m", max_tokens=1, messages=msgs) as s:
                    list(s.text_stream)
                b.converse(modelId="m", messages=msgs)
                list(b.converse_stream(modelId="m", messages=msgs)["stream"])
                b.invoke_model(modelId="m", body=marker.encode())["body"].read()
                list(b.invoke_model_with_response_stream(modelId="m", body=marker.encode())["body"])
                for bad in (lambda: o.chat.completions.create(model="boom", messages=msgs),
                            lambda: a.messages.create(model="boom", max_tokens=1, messages=msgs),
                            lambda: b.converse(modelId="boom", messages=msgs)):
                    try:
                        bad()
                    except Exception:
                        pass
                self.assertTrue(exp.flush(5))
            finally:
                _core.set_client(old)
                exp.shutdown(1)
        blob = b"".join(exports)
        self.assertEqual(blob.count(b'"spanId"'), 14)
        self.assertNotIn(b"MARKER-7f3a9c", blob)
        self.assertNotIn("MARKER-7f3a9c".encode("utf-16-le"), blob)


class ShutdownTest(unittest.TestCase):
    def test_init_instruments_and_shutdown_flushes(self):
        sent = []
        with fakes.installed(fakes.make_openai()) as m:
            self.assertTrue(plumbline.init(transport=sent.append, service="t"))
            try:
                self.assertIn("openai", plumbline.stats()["instrumented"])
                m["openai"].OpenAI().chat.completions.create(model="m", messages=[])
            finally:
                self.assertTrue(plumbline.shutdown(5))
        spans = [sp for body in sent for sp in json.loads(body)["resourceSpans"][0]["scopeSpans"][0]["spans"]]
        self.assertEqual(len(spans), 1)


if __name__ == "__main__":
    unittest.main()
