"""The real client libraries, when installed, against a local fake API.

Skipped when a library is missing (CI installs them in the sdk-python-libs
job; they are test-only, never SDK dependencies).
"""

import importlib.util
import json
import os
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "src"))
sys.path.insert(0, HERE)

from tellproof import _core, _instrument  # noqa: E402
import scenario  # noqa: E402

MARK = "REAL-LIB-MARKER"


def has(name):
    try:
        return importlib.util.find_spec(name) is not None
    except ValueError:
        return False


def sse(events):
    out = ""
    for name, data in events:
        if name:
            out += f"event: {name}\n"
        out += "data: " + (data if isinstance(data, str) else json.dumps(data)) + "\n\n"
    return out.encode()


CHAT = {"id": "c", "object": "chat.completion", "created": 1, "model": "gpt-4.1-mini-2025-04-14",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": MARK}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15}}


def chunk(content=None, usage=None):
    return {"id": "c", "object": "chat.completion.chunk", "created": 1, "model": "gpt-4.1-mini-2025-04-14",
            "choices": [] if content is None else [{"index": 0, "delta": {"content": content}, "finish_reason": None}],
            "usage": usage}


MSG_USAGE = {"input_tokens": 10, "output_tokens": 5, "cache_creation_input_tokens": 2, "cache_read_input_tokens": 3}
MESSAGE = {"id": "m", "type": "message", "role": "assistant", "model": "claude-sonnet-4-5-20250929",
           "content": [{"type": "text", "text": MARK}], "stop_reason": "end_turn", "stop_sequence": None,
           "usage": MSG_USAGE}
MSG_EVENTS = [
    ("message_start", {"type": "message_start", "message": {**MESSAGE, "content": [], "stop_reason": None,
                                                              "usage": {**MSG_USAGE, "output_tokens": 1}}}),
    ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
    ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": MARK}}),
    ("content_block_stop", {"type": "content_block_stop", "index": 0}),
    ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                       "usage": {"output_tokens": 5}}),
    ("message_stop", {"type": "message_stop"}),
]
CONVERSE = {"output": {"message": {"role": "assistant", "content": [{"text": MARK}]}}, "stopReason": "end_turn",
            "usage": {"inputTokens": 7, "outputTokens": 2, "totalTokens": 9}, "metrics": {"latencyMs": 1}}


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
        stream = body.get("stream") is True
        if self.path.endswith("/chat/completions"):
            if stream:
                usage = {"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15}
                events = [(None, chunk(MARK)), (None, chunk(MARK))]
                if (body.get("stream_options") or {}).get("include_usage"):
                    events.append((None, chunk(None, usage)))
                return self.reply(sse(events + [(None, "[DONE]")]), "text/event-stream")
            return self.reply(json.dumps(CHAT).encode())
        if self.path.endswith("/messages"):
            if stream:
                return self.reply(sse(MSG_EVENTS), "text/event-stream")
            return self.reply(json.dumps(MESSAGE).encode())
        if self.path.endswith("/converse"):
            return self.reply(json.dumps(CONVERSE).encode())
        self.send_response(404)
        self.end_headers()

    def reply(self, data, ctype="application/json"):
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *a):
        pass


class RealLibs(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.url = f"http://127.0.0.1:{cls.srv.server_port}"
        _instrument.install(stream_usage=True)

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()
        cls.srv.server_close()
        _instrument._stream_usage = False

    def setUp(self):
        self.cap = scenario.Capture()
        old = _core.current_client()
        _core.set_client(_core.Client(self.cap, service="t"))
        self.addCleanup(_core.set_client, old)

    def tokens(self):
        return [(s["attrs"]["gen_ai.provider.name"], s["attrs"].get("gen_ai.usage.input_tokens"),
                 s["attrs"].get("gen_ai.usage.output_tokens")) for s in self.cap.spans]

    def assertNoContent(self):
        self.assertNotIn(MARK, json.dumps(self.cap.spans))

    @unittest.skipUnless(has("openai"), "openai not installed")
    def test_openai(self):
        import openai
        c = openai.OpenAI(api_key="test", base_url=self.url + "/v1", max_retries=0)
        r = c.chat.completions.create(model="gpt-4.1-mini", messages=[{"role": "user", "content": MARK}])
        self.assertEqual(r.choices[0].message.content, MARK)
        text = "".join(ch.choices[0].delta.content or "" for ch in
                       c.chat.completions.create(model="gpt-4.1-mini", messages=[], stream=True) if ch.choices)
        self.assertEqual(text, MARK * 2)
        with c.chat.completions.stream(model="gpt-4.1-mini", messages=[]) as s:
            for _ in s:
                pass
        self.assertEqual(self.tokens(), [("127.0.0.1", 12, 3)] * 3)  # a local base URL names the host
        self.assertNoContent()

    @unittest.skipUnless(has("anthropic"), "anthropic not installed")
    def test_anthropic(self):
        import anthropic
        c = anthropic.Anthropic(api_key="test", base_url=self.url, max_retries=0)
        c.messages.create(model="claude-sonnet-4-5", max_tokens=10, messages=[{"role": "user", "content": MARK}])
        for _ in c.messages.create(model="claude-sonnet-4-5", max_tokens=10, messages=[], stream=True):
            pass
        with c.messages.stream(model="claude-sonnet-4-5", max_tokens=10, messages=[]) as s:
            self.assertEqual("".join(s.text_stream), MARK)
        self.assertEqual(self.tokens(), [("anthropic", 15, 5)] * 3)
        self.assertNoContent()

    @unittest.skipUnless(has("boto3"), "boto3 not installed")
    def test_bedrock(self):
        import boto3
        c = boto3.client("bedrock-runtime", region_name="ap-southeast-2", endpoint_url=self.url,
                         aws_access_key_id="test", aws_secret_access_key="test")
        r = c.converse(modelId="anthropic.claude-sonnet-4-5", messages=[{"role": "user", "content": [{"text": MARK}]}])
        self.assertEqual(r["output"]["message"]["content"][0]["text"], MARK)
        self.assertEqual(self.tokens(), [("aws.bedrock", 7, 2)])
        self.assertEqual(self.cap.spans[0]["attrs"]["cloud.region"], "ap-southeast-2")
        self.assertNoContent()


if __name__ == "__main__":
    unittest.main()
