"""Encoding of recorded spans as an OTLP/HTTP JSON ExportTraceServiceRequest."""

from __future__ import annotations

import json

SPAN_KIND_INTERNAL = 1
SPAN_KIND_CLIENT = 3
STATUS_ERROR = 2


def _value(v):
    if isinstance(v, bool):
        return {"boolValue": v}
    if isinstance(v, int):
        return {"intValue": str(v)}  # OTLP JSON carries 64-bit ints as strings
    if isinstance(v, float):
        return {"doubleValue": v}
    return {"stringValue": str(v)}


def _attrs(d: dict) -> list:
    return [{"key": k, "value": _value(v)} for k, v in sorted(d.items())]


def encode(spans: list, resource: dict, scope_version: str) -> bytes:
    out = []
    for s in spans:
        span = {
            "traceId": s["trace_id"],
            "spanId": s["span_id"],
            "name": s["name"],
            "kind": s["kind"],
            "startTimeUnixNano": str(s["start"]),
            "endTimeUnixNano": str(s["end"]),
            "attributes": _attrs(s["attrs"]),
            "status": {"code": STATUS_ERROR} if s["error"] else {},
        }
        if s.get("parent"):
            span["parentSpanId"] = s["parent"]
        out.append(span)
    req = {"resourceSpans": [{
        "resource": {"attributes": _attrs(resource)},
        "scopeSpans": [{"scope": {"name": "tellproof", "version": scope_version}, "spans": out}],
    }]}
    return json.dumps(req, separators=(",", ":"), ensure_ascii=False).encode()
