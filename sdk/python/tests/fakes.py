"""Fake openai, anthropic and botocore modules with the real libraries' call
shapes (module paths, method names and signatures, response, usage and stream
event fields, checked against the libraries' sources on 15 Oct 2026). Every
piece of content they return or raise carries the caller's marker, so a test
can prove none of it reaches an export.
"""

from __future__ import annotations

import sys
import types
from types import SimpleNamespace as NS

MARKER = "CONTENT-MARKER"


class APIError(Exception):
    pass


class Stream:
    """openai.Stream / anthropic.Stream: iterator, context manager, close()."""

    def __init__(self, items):
        self._it = iter(items)
        self.response = NS(status_code=200)
        self.closed = False

    def __iter__(self):
        return self

    def __next__(self):
        return next(self._it)

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()

    def close(self):
        self.closed = True


class AsyncStream:
    def __init__(self, items):
        self._items = list(items)
        self.closed = False

    def __aiter__(self):
        return self

    async def __anext__(self):
        if not self._items:
            raise StopAsyncIteration
        return self._items.pop(0)

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        await self.close()

    async def close(self):
        self.closed = True


def _module(name: str, **attrs) -> types.ModuleType:
    m = types.ModuleType(name)
    m.__dict__.update(attrs)
    return m


# --- openai ----------------------------------------------------------------

def _chat_completion(model, marker):
    return NS(id="chatcmpl-1", object="chat.completion", model=model + "-2025-04-14",
              choices=[NS(index=0, finish_reason="stop", message=NS(role="assistant", content=marker))],
              usage=NS(prompt_tokens=120, completion_tokens=30, total_tokens=150,
                       prompt_tokens_details=NS(cached_tokens=100)))


def _chat_chunks(model, marker, include_usage):
    chunks = [NS(object="chat.completion.chunk", model=model + "-2025-04-14", usage=None,
                 choices=[NS(index=0, delta=NS(content=part), finish_reason=None)]) for part in (marker, marker)]
    if include_usage:  # the extra final chunk: no choices, only usage
        chunks.append(NS(object="chat.completion.chunk", model=model + "-2025-04-14", choices=[],
                         usage=NS(prompt_tokens=120, completion_tokens=30, total_tokens=150)))
    return chunks


def _response(model, marker, status="completed"):
    return NS(id="resp_1", object="response", model=model + "-2025-04-14", status=status,
              output=[NS(type="message", content=[NS(type="output_text", text=marker)])],
              usage=NS(input_tokens=80, output_tokens=12, total_tokens=92, input_tokens_details=NS(cached_tokens=0)))


def _response_events(model, marker, final="response.completed"):
    return [NS(type="response.created", response=NS(model=model, usage=None)),
            NS(type="response.output_text.delta", delta=marker),
            NS(type=final, response=_response(model, marker, "failed" if final == "response.failed" else "completed"))]


class _Resource:
    def __init__(self, client):
        self._client = client


def make_openai(marker=MARKER):
    class Completions(_Resource):
        def create(self, *, model, messages, stream=False, stream_options=None, **kw):
            if model == "boom":
                raise APIError(f"invalid request: {marker}")
            if stream:
                return Stream(_chat_chunks(model, marker, bool(stream_options and stream_options.get("include_usage"))))
            return _chat_completion(model, marker)

        def parse(self, *, model, messages, **kw):
            return _chat_completion(model, marker)

    class AsyncCompletions(_Resource):
        async def create(self, *, model, messages, stream=False, stream_options=None, **kw):
            if model == "boom":
                raise APIError(f"invalid request: {marker}")
            if stream:
                return AsyncStream(_chat_chunks(model, marker, bool(stream_options and stream_options.get("include_usage"))))
            return _chat_completion(model, marker)

        async def parse(self, *, model, messages, **kw):
            return _chat_completion(model, marker)

    class Responses(_Resource):
        def create(self, *, model, input, stream=False, **kw):
            if stream:
                return Stream(_response_events(model, marker, kw.get("_final", "response.completed")))
            return _response(model, marker)

        def parse(self, *, model, input, **kw):
            return _response(model, marker)

    class AsyncResponses(_Resource):
        async def create(self, *, model, input, stream=False, **kw):
            if stream:
                return AsyncStream(_response_events(model, marker))
            return _response(model, marker)

    class Embeddings(_Resource):
        def create(self, *, model, input, **kw):
            return NS(object="list", model=model, data=[NS(embedding=[0.1, 0.2])],
                      usage=NS(prompt_tokens=9, total_tokens=9))

    class AsyncEmbeddings(_Resource):
        async def create(self, *, model, input, **kw):
            return NS(object="list", model=model, data=[], usage=NS(prompt_tokens=9, total_tokens=9))

    class LegacyCompletions(_Resource):
        def create(self, *, model, prompt, **kw):
            return NS(model=model, choices=[NS(text=marker)], usage=NS(prompt_tokens=4, completion_tokens=2))

    class OpenAI:
        def __init__(self, base_url="https://api.openai.com/v1/"):
            self.base_url = base_url
            self.chat = NS(completions=Completions(self))
            self.responses = Responses(self)
            self.embeddings = Embeddings(self)
            self.completions = LegacyCompletions(self)

    class AsyncOpenAI(OpenAI):
        def __init__(self, base_url="https://api.openai.com/v1/"):
            self.base_url = base_url
            self.chat = NS(completions=AsyncCompletions(self))
            self.responses = AsyncResponses(self)
            self.embeddings = AsyncEmbeddings(self)

    class AzureOpenAI(OpenAI):
        def __init__(self):
            super().__init__("https://contoso.openai.azure.com/openai/")

    mods = {
        "openai": _module("openai", OpenAI=OpenAI, AsyncOpenAI=AsyncOpenAI, AzureOpenAI=AzureOpenAI, Stream=Stream,
                          AsyncStream=AsyncStream, APIError=APIError),
        "openai.resources": _module("openai.resources"),
        "openai.resources.chat": _module("openai.resources.chat"),
        "openai.resources.chat.completions": _module("openai.resources.chat.completions", Completions=Completions,
                                                     AsyncCompletions=AsyncCompletions),
        "openai.resources.responses": _module("openai.resources.responses", Responses=Responses,
                                              AsyncResponses=AsyncResponses),
        "openai.resources.embeddings": _module("openai.resources.embeddings", Embeddings=Embeddings,
                                               AsyncEmbeddings=AsyncEmbeddings),
        "openai.resources.completions": _module("openai.resources.completions", Completions=LegacyCompletions),
    }
    return mods


# --- anthropic -------------------------------------------------------------

def _anthropic_usage(final=True):
    return NS(input_tokens=10, cache_creation_input_tokens=5, cache_read_input_tokens=100,
              output_tokens=20 if final else 1, service_tier="standard")


def _message(model, marker):
    return NS(id="msg_1", type="message", role="assistant", model=model + "-20250929",
              content=[NS(type="text", text=marker)], stop_reason="end_turn", usage=_anthropic_usage())


def _anthropic_events(model, marker):
    start = NS(id="msg_1", type="message", model=model + "-20250929", content=[], usage=_anthropic_usage(final=False))
    return [NS(type="message_start", message=start),
            NS(type="content_block_start", index=0, content_block=NS(type="text", text="")),
            NS(type="content_block_delta", index=0, delta=NS(type="text_delta", text=marker)),
            NS(type="content_block_stop", index=0),
            # Cumulative; newer API versions repeat only some input fields here.
            NS(type="message_delta", delta=NS(stop_reason="end_turn"),
               usage=NS(output_tokens=20, input_tokens=10, cache_creation_input_tokens=None, cache_read_input_tokens=None)),
            NS(type="message_stop")]


class MessageStream:
    """anthropic.lib.streaming.MessageStream: accumulates a snapshot."""

    def __init__(self, events, model, marker):
        self._events, self._snapshot, self._model, self._marker = list(events), None, model, marker

    def __iter__(self):
        while self._events:
            ev = self._events.pop(0)
            if ev.type == "message_start":
                self._snapshot = NS(model=ev.message.model, usage=_anthropic_usage(final=False), content=[])
            elif ev.type == "message_delta":
                self._snapshot.usage = _anthropic_usage(final=True)
            yield ev

    @property
    def text_stream(self):
        for ev in self:
            if ev.type == "content_block_delta":
                yield ev.delta.text

    def _current_snapshot_or_none(self):
        return self._snapshot

    def get_final_message(self):
        for _ in self:
            pass
        return self._snapshot

    def close(self):
        pass


class AsyncMessageStream(MessageStream):
    def __aiter__(self):
        self._sync = iter(self)
        return self

    async def __anext__(self):
        try:
            return next(self._sync)
        except StopIteration:
            raise StopAsyncIteration


class MessageStreamManager:
    def __init__(self, make):
        self._make = make

    def __enter__(self):
        return self._make()

    def __exit__(self, *exc):
        return None


class AsyncMessageStreamManager(MessageStreamManager):
    async def __aenter__(self):
        return self._make()

    async def __aexit__(self, *exc):
        return None


def make_anthropic(marker=MARKER):
    class Messages(_Resource):
        def create(self, *, model, max_tokens, messages, stream=False, **kw):
            if model == "boom":
                raise APIError(f"overloaded: {marker}")
            if stream:
                return Stream(_anthropic_events(model, marker))
            return _message(model, marker)

        def stream(self, *, model, max_tokens, messages, **kw):
            return MessageStreamManager(lambda: MessageStream(_anthropic_events(model, marker), model, marker))

    class AsyncMessages(_Resource):
        async def create(self, *, model, max_tokens, messages, stream=False, **kw):
            if stream:
                return AsyncStream(_anthropic_events(model, marker))
            return _message(model, marker)

        def stream(self, *, model, max_tokens, messages, **kw):
            return AsyncMessageStreamManager(lambda: AsyncMessageStream(_anthropic_events(model, marker), model, marker))

    class Anthropic:
        def __init__(self):
            self.messages = Messages(self)
            self.beta = NS(messages=BetaMessages(self))

    class AsyncAnthropic:
        def __init__(self):
            self.messages = AsyncMessages(self)

    class AnthropicBedrock(Anthropic):
        aws_region = "ap-southeast-2"

    class BetaMessages(Messages):
        pass

    return {
        "anthropic": _module("anthropic", Anthropic=Anthropic, AsyncAnthropic=AsyncAnthropic,
                             AnthropicBedrock=AnthropicBedrock),
        "anthropic.resources": _module("anthropic.resources"),
        "anthropic.resources.messages": _module("anthropic.resources.messages", Messages=Messages,
                                                AsyncMessages=AsyncMessages),
        "anthropic.resources.beta": _module("anthropic.resources.beta"),
        "anthropic.resources.beta.messages": _module("anthropic.resources.beta.messages", Messages=BetaMessages),
    }


# --- botocore (Bedrock Runtime) ---------------------------------------------

class EventStream:
    """botocore.eventstream.EventStream: iterable of event dicts, close()."""

    def __init__(self, events):
        self._events = events

    def __iter__(self):
        return iter(self._events)

    def close(self):
        pass


class StreamingBody:
    def __init__(self, data: bytes):
        self._data = data

    def read(self, n=-1):
        return self._data


def make_botocore(marker=MARKER):
    class ClientError(Exception):
        pass

    class BaseClient:
        def __init__(self, service, region):
            self.meta = NS(service_model=NS(service_name=service), region_name=region)

        def _make_api_call(self, operation_name, api_params):
            model = api_params.get("modelId", "")
            if model == "boom":
                raise ClientError(f"ValidationException: {marker}")
            stop = "guardrail_intervened" if api_params.get("_intervene") else "end_turn"
            if operation_name == "Converse":
                return {"output": {"message": {"role": "assistant", "content": [{"text": marker}]}},
                        "stopReason": stop, "metrics": {"latencyMs": 100},
                        "usage": {"inputTokens": 50, "outputTokens": 9, "totalTokens": 159,
                                  "cacheReadInputTokens": 100, "cacheWriteInputTokens": 0},
                        "ResponseMetadata": {"HTTPStatusCode": 200, "HTTPHeaders": {}}}
            if operation_name == "ConverseStream":
                return {"stream": EventStream([
                    {"messageStart": {"role": "assistant"}},
                    {"contentBlockDelta": {"delta": {"text": marker}, "contentBlockIndex": 0}},
                    {"messageStop": {"stopReason": stop}},
                    {"metadata": {"usage": {"inputTokens": 50, "outputTokens": 9, "totalTokens": 59},
                                  "metrics": {"latencyMs": 100}}}]),
                        "ResponseMetadata": {"HTTPStatusCode": 200, "HTTPHeaders": {}}}
            if operation_name == "InvokeModel":
                return {"body": StreamingBody(('{"content":[{"text":"%s"}]}' % marker).encode()),
                        "contentType": "application/json",
                        "ResponseMetadata": {"HTTPStatusCode": 200, "HTTPHeaders": {
                            "x-amzn-bedrock-input-token-count": "42", "x-amzn-bedrock-output-token-count": "7"}}}
            if operation_name == "InvokeModelWithResponseStream":
                return {"body": EventStream([{"chunk": {"bytes": marker.encode()}}]),
                        "ResponseMetadata": {"HTTPStatusCode": 200, "HTTPHeaders": {}}}
            return {"Contents": [{"Key": marker}]}  # another service, another operation

        # boto3's generated methods call _make_api_call with the operation name.
        def converse(self, **kw):
            return self._make_api_call("Converse", kw)

        def converse_stream(self, **kw):
            return self._make_api_call("ConverseStream", kw)

        def invoke_model(self, **kw):
            return self._make_api_call("InvokeModel", kw)

        def invoke_model_with_response_stream(self, **kw):
            return self._make_api_call("InvokeModelWithResponseStream", kw)

        def list_objects_v2(self, **kw):
            return self._make_api_call("ListObjectsV2", kw)

    return {"botocore": _module("botocore"), "botocore.client": _module("botocore.client", BaseClient=BaseClient,
                                                                         ClientError=ClientError)}


class installed:
    """Put fake modules in sys.modules for the duration of a with block."""

    def __init__(self, *mod_maps):
        self.mods = {}
        for m in mod_maps:
            self.mods.update(m)

    def __enter__(self):
        self.saved = {k: sys.modules.get(k) for k in self.mods}
        sys.modules.update(self.mods)
        return self.mods

    def __exit__(self, *exc):
        for k, v in self.saved.items():
            if v is None:
                sys.modules.pop(k, None)
            else:
                sys.modules[k] = v
