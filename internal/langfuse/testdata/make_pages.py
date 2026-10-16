"""Recorded Langfuse API pages for the same synthetic traffic as
concierge/testdata/otlp-sample.jsonl (both come from make_samples.build()), so
the Langfuse path can be checked against the OTLP path. Observations API v2 and
Scores API v3 shapes as documented (checked 15 Oct 2026); metadata in the
nested form Langfuse's OpenTelemetry ingestion stores (verify with a partner).

  python3 internal/langfuse/testdata/make_pages.py
"""
import json
import sys
from pathlib import Path

OUT = Path(__file__).resolve().parent
sys.path.insert(0, str(OUT.parents[2] / "concierge" / "testdata"))
import make_samples  # noqa: E402

TYPE = {"invoke_agent": "AGENT", "chat": "GENERATION", "execute_tool": "TOOL"}


def iso(t):
    return t.strftime("%Y-%m-%dT%H:%M:%S.000Z")


def observations():
    obs = []
    for n, (tid, spans) in enumerate(make_samples.build()):
        for j, o in enumerate(spans):
            a = dict(o["attrs"])
            ob = {"id": f"obs-{n}-{j}", "traceId": tid, "startTime": iso(o["t"]), "endTime": iso(o["t"]), "projectId": "proj-1",
                  "parentObservationId": None if j == 0 else f"obs-{n}-0", "type": TYPE[o["kind"]],
                  "name": f"{o['kind']} {make_samples.MARKER[:12]}", "level": "ERROR" if o["error"] else "DEFAULT",
                  "statusMessage": make_samples.MARKER if o["error"] else None, "version": None, "environment": "production"}
            if o["kind"] == "chat":
                model = a.pop("gen_ai.response.model", None) or a.pop("gen_ai.request.model")
                a.pop("gen_ai.request.model", None)
                inp = a.pop("gen_ai.usage.input_tokens", None) or a.pop("gen_ai.usage.prompt_tokens")
                out = a.pop("gen_ai.usage.output_tokens", None) or a.pop("gen_ai.usage.completion_tokens")
                ob.update({"providedModelName": model, "internalModelId": None, "modelParameters": {},
                           "usageDetails": {"input": inp, "output": out, "total": inp + out},
                           "inputUsage": inp, "outputUsage": out, "totalUsage": inp + out})
                if "plumbline.prompt.name" in a:
                    ob["promptName"] = a.pop("plumbline.prompt.name")
                    ob["promptVersion"] = int(a.pop("plumbline.prompt.version"))
                if o.get("content"):
                    ob["input"] = json.dumps([{"role": "user", "content": make_samples.MARKER}])
                    ob["output"] = json.dumps({"role": "assistant", "content": "Dear " + make_samples.MARKER})
                    a["gen_ai.input.messages"] = make_samples.MARKER  # content inside metadata too: counted, never read
            ob["metadata"] = {"attributes": {**a, "user.question": make_samples.MARKER},
                              "resourceAttributes": {"service.name": "claims-triage"}}
            obs.append(ob)
    obs.sort(key=lambda x: (x["startTime"], x["id"]), reverse=True)  # the API returns newest first
    return obs


def write_observation_pages(obs, size=300):
    pages = [obs[i:i + size] for i in range(0, len(obs), size)]
    for i, page in enumerate(pages, 1):
        cursor = f"cursor-{i + 1}" if i < len(pages) else None
        (OUT / f"observations-{i}.json").write_text(json.dumps({"data": page, "meta": {"cursor": cursor}}) + "\n")
    return len(pages)


def write_scores(obs):
    gens = [o for o in obs if o["type"] == "GENERATION" and o.get("promptVersion") is not None]
    gens.sort(key=lambda o: o["startTime"])
    scores = []
    for k, g in enumerate(gens[:20]):  # 20 boolean eval scores on early generations: 15 pass
        scores.append({"id": f"score-{k}", "projectId": "proj-1", "name": "triage-eval", "value": k % 4 != 0, "dataType": "BOOLEAN",
                       "source": "EVAL", "timestamp": g["startTime"], "environment": "production",
                       "subject": {"kind": "observation", "id": g["id"], "traceId": g["traceId"]}})
    scores.append({"id": "score-trace", "projectId": "proj-1", "name": "triage-eval", "value": 0.95, "dataType": "NUMERIC",
                   "source": "EVAL", "timestamp": gens[25]["startTime"], "environment": "production",
                   "subject": {"kind": "trace", "id": gens[25]["traceId"]}})
    scores.append({"id": "score-other", "projectId": "proj-1", "name": "helpfulness", "value": "good", "dataType": "CATEGORICAL",
                   "source": "ANNOTATION", "timestamp": gens[0]["startTime"], "environment": "production",
                   "subject": {"kind": "observation", "id": gens[0]["id"], "traceId": gens[0]["traceId"]}})
    (OUT / "scores-1.json").write_text(json.dumps({"data": scores[:12], "meta": {"cursor": "s2"}}) + "\n")
    (OUT / "scores-2.json").write_text(json.dumps({"data": scores[12:], "meta": {"cursor": None}}) + "\n")


if __name__ == "__main__":
    o = observations()
    n = write_observation_pages(o)
    write_scores(o)
    print(f"wrote {len(o)} observations in {n} pages and 22 scores in 2 pages")
