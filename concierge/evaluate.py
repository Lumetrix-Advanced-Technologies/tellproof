"""C-2 evaluate: aggregates + provider usage + registers -> the eleven objectives.

Rules come from spec/objectives.json (five kinds); framework references from
spec/framework-refs.json. Semantics the rules leave open are fixed in D-021.
The output (evaluation.json) is what C-3 turns into a pack predicate.
"""
import csv
import datetime as dt
import hashlib
import json
import math
import re
from collections import defaultdict
from pathlib import Path
from zoneinfo import ZoneInfo

import canon

SPEC = Path(__file__).resolve().parent.parent / "spec"
SCHEMA = "plumbline-concierge-evaluation/v0.1"
SYSTEM_KEYS = {"organisation", "ai_system", "environment", "description", "profiles", "timezone", "period",
               "declared_models", "declared_regions", "providers", "limitations", "sources", "assurance_level"}
DEFAULT_ASSURANCE = {"level": 1, "meaning": "Telemetry and provider-usage exports supplied by the vendor; reconciliation computed by the issuer."}
REGISTER_COLUMNS = {
    "usage": ["provider", "model", "region", "start", "end", "input_tokens", "output_tokens", "requests"],
    "changes": ["date", "kind", "what"],
    "incidents": ["id", "opened", "acknowledged", "resolved", "summary"],
    "evals": ["suite", "model", "prompt_version", "ran_at", "pass_rate"],
}
INCIDENT_WINDOW = dt.timedelta(minutes=60)  # EO-INC-01: acknowledged within 60 min


# ---------- inputs ----------

def parse_time(s: str) -> dt.datetime:
    d = dt.datetime.fromisoformat(s.strip().replace("Z", "+00:00"))
    if d.tzinfo is None:
        raise ValueError(f"time {s!r} has no timezone")
    return d.astimezone(dt.timezone.utc)


def parse_duration(s: str) -> dt.timedelta:
    m = re.fullmatch(r"P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?)?", s)
    if not m or not any(m.groups()):
        raise ValueError(f"unsupported duration {s!r}")
    d, h, mi = (int(x or 0) for x in m.groups())
    return dt.timedelta(days=d, hours=h, minutes=mi)


def read_csv(path: str, kind: str) -> list[dict]:
    with open(path, newline="", encoding="utf-8") as f:
        r = csv.DictReader(f)
        want = REGISTER_COLUMNS[kind]
        missing = [c for c in want if c not in (r.fieldnames or [])]
        if missing:
            raise ValueError(f"{path}: missing columns {missing} (want {want})")
        return [{k: (v or "").strip() for k, v in row.items() if k in want} for row in r]


def load_system(path: str) -> dict:
    s = json.loads(Path(path).read_text())
    unknown = set(s) - SYSTEM_KEYS
    if unknown:
        raise ValueError(f"{path}: unknown keys {sorted(unknown)}")
    for k in ("organisation", "ai_system", "environment", "timezone", "period", "declared_models", "declared_regions"):
        if k not in s:
            raise ValueError(f"{path}: {k} is required")
    return s


def period_bounds(system) -> tuple[dt.datetime, dt.datetime]:
    """Whole days in the system's timezone, as a half-open UTC interval."""
    tz = ZoneInfo(system["timezone"])
    start = dt.datetime.combine(dt.date.fromisoformat(system["period"]["start"]), dt.time(), tz)
    end = dt.datetime.combine(dt.date.fromisoformat(system["period"]["end"]) + dt.timedelta(days=1), dt.time(), tz)
    return start.astimezone(dt.timezone.utc), end.astimezone(dt.timezone.utc)


def objective_set_hash() -> str:
    return "sha256:" + hashlib.sha256(canon.jcs(canon.loads((SPEC / "objectives.json").read_text()))).hexdigest()


# ---------- number formatting (never overstate a result) ----------

def pct_floor(num, den, places=1) -> float:
    """Percentage rounded down, so a value is never shown above the threshold it missed."""
    if den == 0:
        return 0.0
    f = 10**places
    return math.floor(num * 100 * f / den) / f


def pct_text(num, den, places=1) -> str:
    return f"{pct_floor(num, den, places):.{places}f}%"


# ---------- rule kinds ----------

def status_ratio(num, den, rule) -> str:
    if den == 0:
        return "attention"
    r = num / den
    if r >= rule["threshold"]:
        return "pass"
    return "attention" if r >= rule["threshold"] - rule.get("attention_band", 0) else "fail"


def status_max(value, rule) -> str:
    if value <= rule["threshold"]:
        return "pass"
    return "attention" if value <= rule["threshold"] + rule.get("attention_band", 0) else "fail"


# ---------- evaluation ----------

class Facts:
    """Everything the rules measure, computed once from the inputs."""

    def __init__(self, system, windows, usage, changes, incidents, evals, start, end):
        self.system, self.start, self.end = system, start, end
        self.tz = ZoneInfo(system["timezone"])
        self.windows = windows
        m = defaultdict(int)
        traced = defaultdict(lambda: {"tokens": 0, "calls": 0})
        calls_by_model = defaultdict(int)
        transitions, anomalies = [], []
        canary_missed = defaultdict(int)  # kind -> canaries not caught (EO-CAN-01)
        first_seen = {}
        for w in windows:
            wm = w["metrics"]
            for t in wm["tokens"]:
                key = (t["provider"], t["region"])
                traced[key]["tokens"] += t["input_tokens"] + t["output_tokens"]
                traced[key]["calls"] += t["calls"]
                calls_by_model[(t["provider"], t["model"], t["region"])] += t["calls"]
            for k in ("pii_spans", "pii_spans_redacted"):
                m[k] += wm.get("pii", {}).get(k, 0)
            m["content_present_spans"] += wm["content_present_spans"]
            for k in ("llm_calls", "guarded_calls", "blocked"):
                m[k] += wm.get("guardrails", {}).get(k, 0)
            for k in ("runs", "tool_calls", "tool_errors", "high_impact_calls", "high_impact_approved", "escalations"):
                m[k] += wm.get("agent", {}).get(k, 0)
            for c in wm.get("canaries", []):
                m["canaries_run"] += c["run"]
                m["canaries_caught"] += c["caught"]
                if c["run"] > c["caught"]:
                    canary_missed[c["kind"]] += c["run"] - c["caught"]
            transitions += wm.get("transitions", [])
            anomalies += wm.get("anomalies", [])
            ws = parse_time(w["window"]["start"])
            for v in w.get("observed", {}).get("versions", []):
                first_seen.setdefault((v["model"], v["prompt_version"]), ws)
            if not w.get("observed", {}).get("versions"):
                for om in w.get("observed", {}).get("models", []):
                    first_seen.setdefault((om["model"], None), ws)
            m["leaves"] += w["commit"]["leaves"]
        self.m, self.traced, self.calls_by_model = m, traced, calls_by_model
        self.transitions, self.anomalies = transitions, anomalies
        self.canary_missed = canary_missed
        # The first window that observed anything: versions live there were live before the period.
        # A window with only usage or eval runs observed nothing (D-040).
        observed = [w for w in windows if w.get("observed", {}).get("models") or w.get("observed", {}).get("versions")]
        self.first_window = parse_time(observed[0]["window"]["start"]) if observed else start
        self.live_versions = first_seen

        billed = defaultdict(lambda: {"tokens": 0, "requests": 0, "requests_known": True})
        for u in usage:
            b = billed[(u["provider"], u["region"])]
            b["tokens"] += int(u["input_tokens"]) + int(u["output_tokens"])
            if u["requests"] == "":
                b["requests_known"] = False
            else:
                b["requests"] += int(u["requests"])
        self.billed = billed
        self.changes, self.incidents, self.evals = changes, incidents, evals
        # "unknown" is not a model or a region: it cannot be checked against a declaration (limitations say so)
        self.models_observed = sorted({(p, model) for (p, model, _r) in calls_by_model if model != "unknown"})
        self.regions_observed = sorted({r for (_p, r) in traced if r != "unknown"})
        self.calls_without_model = sum(c for (_p, model, _r), c in calls_by_model.items() if model == "unknown")
        self.calls_without_region = sum(t["calls"] for (_p, r), t in traced.items() if r == "unknown")

    # coverage over providers that have usage data; others are listed, not counted.
    # A provider whose usage carries no region (OpenAI, Anthropic: D-028) is compared across all its regions.
    def coverage(self):
        regionless = {p for (p, r) in self.billed if r == ""}
        traced = defaultdict(lambda: {"tokens": 0, "calls": 0})
        for (p, r), t in self.traced.items():
            k = (p, "") if p in regionless else (p, r)
            traced[k]["tokens"] += t["tokens"]
            traced[k]["calls"] += t["calls"]
        rows, num, den = [], 0, 0
        for key in sorted(set(self.billed) | set(traced)):
            b, t = self.billed.get(key), traced.get(key, {"tokens": 0, "calls": 0})
            if b is not None:
                num += t["tokens"]
                den += b["tokens"]
            rows.append({
                "provider": self.system.get("providers", {}).get(key[0], key[0]),
                "region": key[1],
                "billed_tokens": b["tokens"] if b else 0,
                "traced_tokens": t["tokens"],
                "billed_requests": b["requests"] if b and b["requests_known"] else None,
                "traced_requests": t["calls"],
                "has_usage": b is not None,
            })
        return rows, num, den


def evaluate_objective(o, f: Facts, refs) -> dict:
    rule, kind = o["rule"], o["rule"]["kind"]
    m = f.m
    values = {}
    if o["id"] == "EO-LOG-01" or (kind == "ratio_at_least" and rule["numerator"] == "traced_tokens"):
        _, num, den = f.coverage()
        status, measured = status_ratio(num, den, rule), f"{pct_text(num, den)} of billed tokens traced" if den else "No provider usage supplied"
        threshold = f">= {rule['threshold'] * 100:.1f}% of provider-billed tokens traced"
        values = {"numerator": num, "denominator": den}
    elif kind == "ratio_at_least":
        num, den = m[rule["numerator"]], m[rule["denominator"]]
        status = status_ratio(num, den, rule)
        values = {"numerator": num, "denominator": den}
        if rule["numerator"] == "pii_spans_redacted":
            measured = f"{pct_text(num, den)} of tagged spans redacted" if den else "No spans tagged as personal data"
            threshold = f"{rule['threshold'] * 100:g}% of spans tagged as personal data pass the redaction processor"
        elif rule["numerator"] == "guarded_calls":
            measured = f"{pct_text(num, den)} covered; {pct_text(m['blocked'], den, 2)} blocked" if den else "No LLM calls"
            threshold = f">= {rule['threshold'] * 100:g}% of LLM calls pass input and output guardrails"
        elif rule["numerator"] == "canaries_caught":
            measured = "No guardrail canaries run"
            if den:
                measured = f"{num} of {den} canaries caught"
                missed = sorted(f.canary_missed)
                if missed:
                    measured += "; missed: " + ", ".join(f"{k} ({f.canary_missed[k]})" for k in missed)
            threshold = "Every synthetic known-bad probe sent through the guardrail is caught"
        else:
            measured, threshold = f"{pct_text(num, den)}", f">= {rule['threshold'] * 100:g}%"
    elif kind == "max_value":
        if rule["metric"] == "tool_error_rate":
            calls = m["tool_calls"]
            value = m["tool_errors"] / calls if calls else 0.0
            status = status_max(value, rule)
            measured = (f"{pct_text(m['tool_errors'], calls, 2)} tool errors; {pct_text(m['escalations'], m['runs'])} of runs escalated; "
                        f"{m['high_impact_approved']} of {m['high_impact_calls']} high-impact calls approved") if calls else "No tool calls"
            threshold = f"Tool-call error rate <= {rule['threshold'] * 100:g}%"
            values = {"value": value, "tool_errors": m["tool_errors"], "tool_calls": calls}
        else:
            raise ValueError(f"{o['id']}: no measure for metric {rule['metric']}")
    elif kind == "zero_count":
        if rule["metric"] == "content_present_spans":
            n = m["content_present_spans"]
            measured, threshold = f"{n} spans with message content attributes", "No prompt or completion text exported to telemetry"
        elif rule["metric"] == "unlogged_transitions":
            unlogged = [t for t in f.transitions if not _logged(t, f)]
            n = len(unlogged)
            logged_changes = [c for c in f.changes if _in_period_date(c["date"], f)]
            measured = f"{len(logged_changes)} change(s) logged, {n} unlogged transition(s)"
            threshold = "Every model or prompt change seen in telemetry is in the change register"
            values["unlogged"] = unlogged
        elif rule["metric"] == "unacknowledged_anomalies":
            unack = [a for a in f.anomalies if not _acknowledged(a, f)]
            n = len(unack)
            in_period = [i for i in f.incidents if f.start <= parse_time(i["opened"]) < f.end]
            measured = (f"{len(in_period)} incident(s); all acknowledged in time" if n == 0 else
                        f"{len(in_period)} incident(s); {n} anomaly window(s) without an incident acknowledged within 60 min")
            threshold = "Every guardrail spike or error burst has an incident record acknowledged within 60 min"
            values["unacknowledged"] = unack
        else:
            raise ValueError(f"{o['id']}: no measure for metric {rule['metric']}")
        status = "pass" if n == 0 else "fail"
        values["count"] = n
    elif kind == "all_declared":
        if rule["observed"] == "models_observed":
            declared = {(d["provider"], d["model"]) for d in f.system["declared_models"]}
            observed = f.models_observed
            bad = [f"{p} {mo}" for p, mo in observed if (p, mo) not in declared]
            measured = f"{len(observed) - len(bad)} of {len(observed)} models observed are declared" + (f"; undeclared: {', '.join(bad)}" if bad else "")
            threshold = "Every model in use is on the declared inventory"
        else:
            declared = set(f.system["declared_regions"])
            observed = f.regions_observed
            bad = [r for r in observed if r not in declared]
            measured = (f"All calls in declared regions ({', '.join(observed)})" if not bad else
                        f"Calls in undeclared region(s): {', '.join(bad)}")
            threshold = f"Every region in use is declared ({', '.join(sorted(declared))})"
        status = "pass" if not bad else "fail"
        values = {"observed": [list(x) if isinstance(x, tuple) else x for x in observed], "undeclared": bad}
    elif kind == "freshness_within":
        status, measured, values = _eval_freshness(rule, f)
        threshold = (f"Eval run on each live model and prompt version before release (at most {rule['max_age'][1:].lower()} before); "
                     f"pass rate >= {rule.get('min_pass_rate', 0) * 100:g}%")
    else:
        raise ValueError(f"{o['id']}: unknown rule kind {kind}")
    return {
        "id": o["id"], "title": o["title"], "status": status, "threshold": threshold, "measured": measured,
        "basis": o["basis"], "maps": [map_label(refs[r]) for r in o["maps_to"]], "rule": kind, "values": values,
    }


def map_label(ref) -> str:
    """Framework, reference and our own short title, asterisked when not yet verified (CLAUDE.md).
    When the reference adds nothing (APRA letter themes, or a ref already in the framework's
    name such as CPS 230), the label is "framework: title"."""
    star = "" if ref["verified"] else "*"
    if ref["ref"] == "theme" or ref["ref"] in ref["framework"]:
        return f"{ref['framework']}: {ref['title']}{star}"
    return f"{ref['framework']} {ref['ref']} {ref['title']}{star}"


def _local_date(t: dt.datetime, f: Facts) -> dt.date:
    return t.astimezone(f.tz).date()


def _in_period_date(d: str, f: Facts) -> bool:
    day = dt.date.fromisoformat(d)
    return _local_date(f.start, f) <= day <= _local_date(f.end - dt.timedelta(microseconds=1), f)


def _logged(t, f: Facts) -> bool:
    """A transition is logged when the change register has the same kind within a day either side (local dates)."""
    day = _local_date(parse_time(t["at"]), f)
    return any(c["kind"] == t["kind"] and abs((dt.date.fromisoformat(c["date"]) - day).days) <= 1 for c in f.changes)


def _acknowledged(a, f: Facts) -> bool:
    """An anomaly is acknowledged when an incident opened within 60 min of its window was acknowledged within 60 min of opening."""
    start, end = parse_time(a["start"]), parse_time(a["end"])
    for i in f.incidents:
        opened = parse_time(i["opened"])
        if start - INCIDENT_WINDOW <= opened <= end + INCIDENT_WINDOW and i["acknowledged"]:
            if parse_time(i["acknowledged"]) - opened <= INCIDENT_WINDOW:
                return True
    return False


def _eval_freshness(rule, f: Facts):
    """D-021: each live (model, prompt version) needs a passing eval that ran before it was first
    seen and at most max_age before; versions already live in the first window only need one
    that ran before. A version without one is attention while it has been live less than
    max_age at period end, fail after that."""
    max_age, min_rate = parse_duration(rule["max_age"]), rule.get("min_pass_rate", 0)
    passing = [e for e in f.evals if float(e["pass_rate"]) >= min_rate]
    problems, latest = [], None
    for (model, version), seen in sorted(f.live_versions.items(), key=lambda kv: kv[1]):
        carried = seen <= f.first_window
        ok = [e for e in passing if e["model"] == model and (version is None or e["prompt_version"] == version)
              and parse_time(e["ran_at"]) <= seen and (carried or seen - parse_time(e["ran_at"]) <= max_age)]
        if ok:
            best = max(ok, key=lambda e: parse_time(e["ran_at"]))
            latest = best if latest is None or parse_time(best["ran_at"]) > parse_time(latest["ran_at"]) else latest
            continue
        live = f.end - seen
        problems.append({"model": model, "prompt_version": version, "live_days": live.days,
                         "status": "attention" if live < max_age else "fail"})
    if not f.live_versions:
        return "attention", "No model or prompt versions observed", {"problems": []}
    if not problems:
        age = (f.end - parse_time(latest["ran_at"])).days
        return "pass", f"{float(latest['pass_rate']) * 100:.1f}% pass, last run {age} days before period end", {"problems": []}
    worst = "fail" if any(p["status"] == "fail" for p in problems) else "attention"
    p = problems[0]
    what = f"{p['model']} prompt v{p['prompt_version']}" if p["prompt_version"] is not None else p["model"]
    extra = f" (+{len(problems) - 1} more)" if len(problems) > 1 else ""
    return worst, f"No passing run on live {what} (live {p['live_days']} days){extra}", {"problems": problems}


def evaluate(system_path, aggregate_paths, usage_paths, changes_path=None, incidents_path=None, evals_path=None) -> dict:
    system = load_system(system_path)
    start, end = period_bounds(system)
    windows = []
    for p in aggregate_paths:
        doc = json.loads(Path(p).read_text())
        if doc.get("schema") != "plumbline-concierge-aggregates/v0.1":
            raise ValueError(f"{p}: not a concierge aggregates file")
        windows += [w for w in doc["windows"] if start <= parse_time(w["window"]["start"]) < end]
    windows.sort(key=lambda w: w["window"]["start"])
    usage = [u for p in usage_paths for u in read_csv(p, "usage") if start <= parse_time(u["start"]) < end]
    changes = read_csv(changes_path, "changes") if changes_path else []
    incidents = read_csv(incidents_path, "incidents") if incidents_path else []
    evals = read_csv(evals_path, "evals") if evals_path else []
    f = Facts(system, windows, usage, changes, incidents, evals, start, end)

    objectives = json.loads((SPEC / "objectives.json").read_text())["objectives"]
    refs = {r["id"]: r for r in json.loads((SPEC / "framework-refs.json").read_text())["refs"]}
    results = [evaluate_objective(o, f, refs) for o in objectives]

    cov_rows, num, den = f.coverage()
    total_calls = sum(f.calls_by_model.values())
    roles = {(d["provider"], d["model"]): d.get("role", "") for d in system["declared_models"]}
    lineage = [{"provider": system.get("providers", {}).get(p, p), "model": model, "role": roles.get((p, model), ""), "region": r,
                "share_of_calls_pct": round(100 * c / total_calls, 1) if total_calls else None}
               for (p, model, r), c in sorted(f.calls_by_model.items(), key=lambda kv: -kv[1])]
    limitations = list(system.get("limitations", [])) + [
        "Proves what the exported telemetry shows. It does not prove the model behaves well on inputs that never occurred.",
        "Coverage below 100% means some provider-billed usage has no matching trace; untraced usage is not assessed.",
    ] + [f"No provider usage was supplied for {r['provider']} in {r['region']}; its traced usage is listed but not counted in coverage."
         for r in cov_rows if not r["has_usage"]] + (
        [f"{f.calls_without_model} call(s) named no model; the model inventory cannot be checked for them."] if f.calls_without_model else []) + (
        [f"{f.calls_without_region} call(s) carried no region; region declarations cannot be checked for them."] if f.calls_without_region else [])
    file_digest = lambda p: {"name": Path(p).name, "sha256": hashlib.sha256(Path(p).read_bytes()).hexdigest()}  # noqa: E731
    return {
        "schema": SCHEMA,
        "objective_set_hash": objective_set_hash(),
        "period": {**system["period"], "timezone": system["timezone"], "utc_start": start.strftime("%Y-%m-%dT%H:%M:%SZ"),
                   "utc_end": end.strftime("%Y-%m-%dT%H:%M:%SZ")},
        "subject": {k: system[k] for k in ("organisation", "ai_system", "environment", "description") if k in system},
        "profiles": system.get("profiles", []),
        "assurance_level": system.get("assurance_level", DEFAULT_ASSURANCE),
        "sources": system.get("sources", []),
        "objectives": results,
        "coverage": {"basis": "tokens", "total_pct": pct_floor(num, den), "providers": [{k: v for k, v in r.items() if k != "has_usage"} for r in cov_rows]},
        "lineage": lineage,
        "changes": [c for c in changes if _in_period_date(c["date"], f)],
        "incidents": [{k: v for k, v in i.items() if v} for i in incidents if start <= parse_time(i["opened"]) < end],
        "limitations": limitations,
        "windows": len(windows),
        "leaf_count": f.m["leaves"],
        "inputs": {"system": file_digest(system_path), "aggregates": [file_digest(p) for p in aggregate_paths],
                   "usage": [file_digest(p) for p in usage_paths],
                   **{k: file_digest(v) for k, v in (("changes", changes_path), ("incidents", incidents_path), ("evals", evals_path)) if v}},
    }
