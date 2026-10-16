"""C-1 ingest: OTLP JSON dumps or Langfuse exports -> hourly aggregates.

Output windows use the batch `metrics` shape of spec/batch.schema.json. Message
content is never read: content attributes and Langfuse input/output are checked
for presence only, and every other attribute is ignored unless it is one of the
mapped names below. Trace IDs leave only as salted Merkle leaves.

Attribute names beyond spec/semconv-map.json (PII, guardrails, prompt versions,
tool approval, escalation) are Plumbline concierge defaults; override them per
design partner with --attributes (D-020).
"""
import datetime as dt
import hashlib
import json
from collections import defaultdict
from pathlib import Path

import merkle

SPEC = Path(__file__).resolve().parent.parent / "spec"
SCHEMA = "plumbline-concierge-aggregates/v0.1"

DEFAULT_ATTRIBUTES = {
    "pii_marker": "plumbline.data_class",  # span carries personal data when the value is in pii_values
    "pii_values": ["personal"],
    "redacted": "plumbline.redaction.applied",  # true when a redaction processor ran
    "guardrail_decision": "plumbline.guardrail.decision",  # allow | block | flag, on the LLM call span
    "prompt_name": "plumbline.prompt.name",
    "prompt_version": "plumbline.prompt.version",
    "slot": "gen_ai.agent.name",  # model transitions are tracked per slot (falls back to service.name)
    "high_impact": "plumbline.tool.high_impact",
    "approved": "plumbline.tool.approved",
    "escalation": "plumbline.escalation",
    "llm_operations": ["chat", "text_completion", "generate_content", "embeddings", "completion", "embedding"],
    "anomaly": {"min_calls": 20, "error_rate": 0.05, "guardrail_block_rate": 0.03},
}


def load_attributes(path: str | None) -> dict:
    attrs = json.loads(json.dumps(DEFAULT_ATTRIBUTES))
    if path:
        override = json.loads(Path(path).read_text())
        unknown = set(override) - set(attrs)
        if unknown:
            raise ValueError(f"attributes file: unknown keys {sorted(unknown)}")
        for k, v in override.items():
            attrs[k] = {**attrs[k], **v} if isinstance(attrs[k], dict) else v
    return attrs


def load_semconv() -> dict:
    return json.loads((SPEC / "semconv-map.json").read_text())


class Span:
    """The only facts kept about a span. No names, no content, no free text."""

    __slots__ = ("trace_id", "start", "kind", "operation", "provider", "model", "region", "input_tokens", "output_tokens",
                 "error", "content_present", "pii", "redacted", "guardrail", "prompt_name", "prompt_version",
                 "slot", "high_impact", "approved", "escalation")

    def __init__(self, **kw):
        for k in self.__slots__:
            setattr(self, k, kw.get(k))


def _truthy(v) -> bool:
    return v is True or (isinstance(v, str) and v.lower() == "true")


def _classify(operation, attrs) -> str:
    if operation == "invoke_agent":
        return "agent"
    if operation == "execute_tool":
        return "tool"
    if operation in attrs["llm_operations"]:
        return "llm"
    return "other"


def _pick(values: dict, names: list[str]):
    for n in names:
        if n in values and values[n] not in (None, ""):
            return values[n]
    return None


def _span_from_values(trace_id, start, operation, values, presence_keys, error, semconv, attrs, langfuse_content=False):
    """values: only the attribute values we are allowed to read (already filtered)."""
    f = semconv["fields"]
    kind = _classify(operation, attrs)
    prefixes = tuple(semconv.get("presence_only_prefixes", []))
    content = langfuse_content or any(k in semconv["presence_only"] or (prefixes and k.startswith(prefixes)) for k in presence_keys)
    model = _pick(values, f["response_model"]) or _pick(values, f["request_model"])
    return Span(
        trace_id=trace_id, start=start, kind=kind, operation=str(operation or ""),
        provider=str(_pick(values, f["provider"]) or "unknown"),
        model=str(model or "unknown"),
        region=str(_pick(values, f["region"]) or "unknown"),
        input_tokens=int(_pick(values, f["input_tokens"]) or 0),
        output_tokens=int(_pick(values, f["output_tokens"]) or 0),
        error=bool(error), content_present=content,
        pii=values.get(attrs["pii_marker"]) in attrs["pii_values"],
        redacted=_truthy(values.get(attrs["redacted"])),
        guardrail=(str(values[attrs["guardrail_decision"]]) if values.get(attrs["guardrail_decision"]) not in (None, "") else None),
        prompt_name=values.get(attrs["prompt_name"]), prompt_version=values.get(attrs["prompt_version"]),
        slot=values.get(attrs["slot"]) or values.get("service.name"),
        high_impact=_truthy(values.get(attrs["high_impact"])), approved=_truthy(values.get(attrs["approved"])),
        escalation=_truthy(values.get(attrs["escalation"])),
    )


def _readable_names(semconv, attrs) -> set[str]:
    names = {n for ns in semconv["fields"].values() for n in ns}
    names |= {attrs[k] for k in ("pii_marker", "redacted", "guardrail_decision", "prompt_name", "prompt_version",
                                  "slot", "high_impact", "approved", "escalation")}
    names |= {"service.name"}
    return names


# ---------- OTLP JSON ----------

def _otlp_value(v: dict):
    for k in ("stringValue", "boolValue", "doubleValue"):
        if k in v:
            return v[k]
    if "intValue" in v:
        return int(v["intValue"])  # may arrive as a string
    return None  # arrays, maps, bytes: never needed, never read


def _otlp_attrs(kvs, readable, presence):
    """Return (values for readable names only, set of keys present). Values of
    every other attribute, including content attributes, are not read."""
    values, keys = {}, set()
    for kv in kvs or []:
        k = kv.get("key")
        keys.add(k)
        if k in readable and k not in presence:
            values[k] = _otlp_value(kv.get("value") or {})
    return values, keys


def _ns_to_dt(ns) -> dt.datetime:
    return dt.datetime.fromtimestamp(int(ns) / 1e9, tz=dt.timezone.utc)


def parse_otlp(docs, semconv, attrs):
    readable = _readable_names(semconv, attrs)
    presence = set(semconv["presence_only"])
    for doc in docs:
        for rs in doc.get("resourceSpans", []):
            res_values, _ = _otlp_attrs((rs.get("resource") or {}).get("attributes"), readable, presence)
            for ss in rs.get("scopeSpans", []):
                for sp in ss.get("spans", []):
                    values, keys = _otlp_attrs(sp.get("attributes"), readable, presence)
                    for ev in sp.get("events", []) or []:  # older SDKs put content in span events
                        _, ekeys = _otlp_attrs(ev.get("attributes"), set(), presence)
                        keys |= ekeys
                    merged = {**res_values, **values}
                    error = (sp.get("status") or {}).get("code") in (2, "STATUS_CODE_ERROR")
                    yield _span_from_values(str(sp.get("traceId", "")), _ns_to_dt(sp["startTimeUnixNano"]),
                                            _pick(merged, semconv["fields"]["operation"]), merged, keys, error, semconv, attrs)


# ---------- Langfuse export (JSON array/JSONL of observations, or CSV) ----------
# Fields read (verify against a real export before a partner's first pack):
# traceId, type (GENERATION | AGENT | TOOL | SPAN | EVENT ...), startTime, model,
# usageDetails.input/output (or usage.input/output, promptTokens/completionTokens,
# inputTokens/outputTokens), level (ERROR), promptName, promptVersion, metadata.
# input and output are checked for presence only.

LANGFUSE_KIND = {"GENERATION": "chat", "AGENT": "invoke_agent", "TOOL": "execute_tool"}


def _parse_iso(s: str) -> dt.datetime:
    d = dt.datetime.fromisoformat(s.replace("Z", "+00:00"))
    return d.astimezone(dt.timezone.utc) if d.tzinfo else d.replace(tzinfo=dt.timezone.utc)


def parse_langfuse(observations, semconv, attrs):
    readable = _readable_names(semconv, attrs)
    for ob in observations:
        meta = ob.get("metadata") if isinstance(ob.get("metadata"), dict) else {}
        values = {k: v for k, v in meta.items() if k in readable and k not in semconv["presence_only"]}
        usage = ob.get("usageDetails") or ob.get("usage") or {}
        if isinstance(usage, dict):
            values.setdefault("gen_ai.usage.input_tokens", usage.get("input"))
            values.setdefault("gen_ai.usage.output_tokens", usage.get("output"))
        for src, dst in (("promptTokens", "gen_ai.usage.input_tokens"), ("completionTokens", "gen_ai.usage.output_tokens"),
                         ("inputTokens", "gen_ai.usage.input_tokens"), ("outputTokens", "gen_ai.usage.output_tokens")):
            if ob.get(src) not in (None, "") and values.get(dst) in (None, ""):
                values[dst] = ob[src]
        if ob.get("model"):
            values.setdefault("gen_ai.response.model", ob["model"])
        for k in ("provider", "region"):
            if meta.get(k):
                values.setdefault({"provider": "gen_ai.provider.name", "region": "cloud.region"}[k], meta[k])
        if ob.get("promptName"):
            values.setdefault(attrs["prompt_name"], ob["promptName"])
        if ob.get("promptVersion") not in (None, ""):
            values.setdefault(attrs["prompt_version"], str(ob["promptVersion"]))
        values = {k: v for k, v in values.items() if v not in (None, "")}
        content = ob.get("input") not in (None, "", {}, []) or ob.get("output") not in (None, "", {}, [])
        operation = LANGFUSE_KIND.get(str(ob.get("type", "")).upper())
        yield _span_from_values(str(ob.get("traceId", "")), _parse_iso(ob["startTime"]), operation, values, set(),
                                str(ob.get("level", "")).upper() == "ERROR", semconv, attrs, langfuse_content=content)


def _csv_rows(path: Path):
    import csv
    with path.open(newline="", encoding="utf-8") as f:
        for row in csv.DictReader(f):
            meta = {k[len("metadata."):]: v for k, v in row.items() if k and k.startswith("metadata.")}
            ob = {k: v for k, v in row.items() if k and not k.startswith("metadata.")}
            ob["metadata"] = meta
            yield ob


# ---------- reading files ----------

def read_json_docs(path: Path):
    text = path.read_text(encoding="utf-8")
    try:
        doc = json.loads(text)
        yield from (doc if isinstance(doc, list) else [doc])
    except json.JSONDecodeError:
        for line in text.splitlines():  # JSON lines (one document per line)
            if line.strip():
                yield json.loads(line)


def read_spans(kind: str, paths: list[Path], semconv, attrs):
    for p in paths:
        if kind == "otlp":
            yield from parse_otlp(read_json_docs(p), semconv, attrs)
        elif p.suffix.lower() == ".csv":
            yield from parse_langfuse(_csv_rows(p), semconv, attrs)
        else:
            yield from parse_langfuse(read_json_docs(p), semconv, attrs)


# ---------- aggregation ----------

def _hour(t: dt.datetime) -> dt.datetime:
    return t.replace(minute=0, second=0, microsecond=0)


def _iso(t: dt.datetime) -> str:
    return t.strftime("%Y-%m-%dT%H:%M:%SZ")


def aggregate(spans, attrs, salt_secret: bytes):
    """Returns (windows, leaves). Each window: {window, metrics, commit}. leaves:
    hex leaf hashes in window order, then by each trace's first span time."""
    spans = sorted(spans, key=lambda s: (s.start, s.trace_id))
    first_seen = {}
    for s in spans:
        if s.trace_id and s.trace_id not in first_seen:
            first_seen[s.trace_id] = s.start

    by_hour = defaultdict(list)
    for s in spans:
        by_hour[_hour(s.start)].append(s)
    traces_by_hour = defaultdict(list)
    for tid, t in sorted(first_seen.items(), key=lambda kv: (kv[1], kv[0])):
        traces_by_hour[_hour(t)].append(tid)

    # transitions over the whole input, in time order
    last_model, last_prompt = {}, {}
    transitions = defaultdict(list)
    for s in spans:
        if s.kind != "llm":
            continue
        if s.model != "unknown":
            # Per slot and operation, so a chat model and an embeddings model
            # used side by side are not read as changes (D-033).
            slot = (s.slot or "default", s.operation)
            if slot in last_model and last_model[slot] != s.model:
                transitions[_hour(s.start)].append({"at": _iso(s.start), "kind": "model", "from": last_model[slot], "to": s.model})
            last_model[slot] = s.model
        if s.prompt_version not in (None, ""):
            name = s.prompt_name or "default"
            v = str(s.prompt_version)
            if name in last_prompt and last_prompt[name] != v:
                transitions[_hour(s.start)].append({"at": _iso(s.start), "kind": "prompt", "from": last_prompt[name], "to": v})
            last_prompt[name] = v

    th = attrs["anomaly"]
    windows, all_leaves = [], []
    for hour in sorted(set(by_hour) | set(traces_by_hour)):
        hs = by_hour[hour]
        tokens = defaultdict(lambda: {"input_tokens": 0, "output_tokens": 0, "calls": 0})
        llm = [s for s in hs if s.kind == "llm"]
        for s in llm:
            t = tokens[(s.provider, s.model, s.region)]
            t["input_tokens"] += s.input_tokens
            t["output_tokens"] += s.output_tokens
            t["calls"] += 1
        tools = [s for s in hs if s.kind == "tool"]
        high = [s for s in tools if s.high_impact]
        guarded = [s for s in llm if s.guardrail is not None]
        blocked = [s for s in guarded if s.guardrail == "block"]
        pii = [s for s in hs if s.pii]
        end = hour + dt.timedelta(hours=1)
        anomalies = []
        if len(llm) >= th["min_calls"]:
            if sum(s.error for s in llm) / len(llm) >= th["error_rate"]:
                anomalies.append({"start": _iso(hour), "end": _iso(end), "kind": "error_rate"})
            if len(blocked) / len(llm) >= th["guardrail_block_rate"]:
                anomalies.append({"start": _iso(hour), "end": _iso(end), "kind": "guardrail_rate"})
        metrics = {
            "tokens": [{"provider": p, "model": m, "region": r, **v} for (p, m, r), v in sorted(tokens.items())],
            "pii": {"pii_spans": len(pii), "pii_spans_redacted": sum(s.redacted for s in pii)},
            "content_present_spans": sum(s.content_present for s in hs),
            "guardrails": {"llm_calls": len(llm), "guarded_calls": len(guarded), "blocked": len(blocked)},
            "evals": [],
            "agent": {"runs": sum(s.kind == "agent" for s in hs), "tool_calls": len(tools), "tool_errors": sum(s.error for s in tools),
                      "high_impact_calls": len(high), "high_impact_approved": sum(s.approved for s in high),
                      "escalations": sum(s.escalation for s in hs)},
            "transitions": transitions[hour],
            "anomalies": anomalies,
            "late_spans": 0,
        }
        leaves = [merkle.leaf_hash(merkle.salt(salt_secret, tid), tid) for tid in traces_by_hour[hour]]
        commit = {"root": merkle.root(leaves).hex() if leaves else "0" * 64, "leaves": len(leaves)}
        # Which model and prompt versions were live, for eval freshness (EO-EVAL-01). The batch
        # batch schema v0.2 carries it beside metrics (D-021, P3-0).
        observed = {
            "models": [{"provider": p, "model": m} for p, m in sorted({(s.provider, s.model) for s in llm if s.model != "unknown"})],
            "versions": [{"model": m, "prompt_version": v} for m, v in
                         sorted({(s.model, str(s.prompt_version)) for s in llm if s.model != "unknown" and s.prompt_version not in (None, "")})],
        }
        windows.append({"window": {"start": _iso(hour), "end": _iso(end)}, "metrics": metrics, "observed": observed, "commit": commit})
        all_leaves.extend(l.hex() for l in leaves)
    return windows, all_leaves


def ingest(kind: str, paths: list[str], salt_secret: bytes, attributes_path: str | None = None) -> tuple[dict, list[str]]:
    if kind not in ("otlp", "langfuse"):
        raise ValueError("source must be otlp or langfuse")
    if not salt_secret:
        raise ValueError("a salt secret is required (PLUMBLINE_SALT_SECRET)")
    semconv = load_semconv()
    attrs = load_attributes(attributes_path)
    files = [Path(p) for p in paths]
    windows, leaves = aggregate(read_spans(kind, files, semconv, attrs), attrs, salt_secret)
    out = {
        "schema": SCHEMA,
        "semconv_map": semconv["version"],
        "source": {"kind": kind, "files": [{"name": f.name, "sha256": hashlib.sha256(f.read_bytes()).hexdigest()} for f in files]},
        "attributes": attrs,
        "windows": windows,
    }
    return out, leaves
