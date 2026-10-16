"""Generate the Plumbline test vectors (pass and fail cases).

Run from this folder: python3 make_vectors.py
Creates the TEST-ONLY key on first run and reuses it after.

Original notes:

Everything here is synthetic: the vendor (Kestrel Assist) is fictional and the
telemetry numbers are generated. The signing key is a throwaway P-256 key that
stands in for the AWS KMS key the real concierge CLI would use.

Format per pack:
  predicate  = the pack (JSON)
  statement  = in-toto Statement v1 whose subject digest is sha256(JCS(predicate))
  payload    = JCS(statement)  (RFC 8785 canonical JSON, UTF-8)
  envelope   = DSSE {payloadType, payload(b64), signatures:[{keyid, sig(b64 DER ECDSA P-256/SHA-256)}]}
  pack digest (used for chaining) = sha256(payload)
"""
import base64, hashlib, hmac, json, random, sys
from pathlib import Path
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.hazmat.primitives import hashes, serialization

OUT = Path(__file__).resolve().parent
PREDICATE_TYPE = "https://lumetrix.example/plumbline/pack/v0.1"  # D-008: replace with the real domain before the first customer pack
(OUT / "pass").mkdir(exist_ok=True)
(OUT / "fail").mkdir(exist_ok=True)
random.seed(42)

# ---------- canonical JSON (RFC 8785 subset: no non-integral floats beyond simple decimals) ----------
def norm(o):
    if isinstance(o, float):
        return int(o) if o.is_integer() else round(o, 4)
    if isinstance(o, dict):
        return {k: norm(v) for k, v in o.items()}
    if isinstance(o, list):
        return [norm(v) for v in o]
    return o

def jcs(o) -> bytes:
    return json.dumps(norm(o), sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")

def sha256hex(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()

def pae(ptype: str, body: bytes) -> bytes:
    t = ptype.encode()
    return b"DSSEv1 %d %s %d %s" % (len(t), t, len(body), body)

# ---------- key (stand-in for KMS) ----------
KEY_FILE = OUT / "test-private-key.pem"
if KEY_FILE.exists():
    key = serialization.load_pem_private_key(KEY_FILE.read_bytes(), None)
else:
    key = ec.generate_private_key(ec.SECP256R1())
    KEY_FILE.write_bytes(key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
pub = key.public_key().public_numbers()
b64u = lambda b: base64.urlsafe_b64encode(b).rstrip(b"=").decode()
KID = "test-p256-2026-10"
JWK = {"kty": "EC", "crv": "P-256", "kid": KID, "use": "sig", "alg": "ES256",
       "x": b64u(pub.x.to_bytes(32, "big")), "y": b64u(pub.y.to_bytes(32, "big"))}
(OUT / "test-jwks.json").write_text(json.dumps({"keys": [JWK]}, indent=2))
(OUT / "test-public-key.pem").write_bytes(key.public_key().public_bytes(
    serialization.Encoding.PEM, serialization.PublicFormat.SubjectPublicKeyInfo))

# ---------- Merkle commitment over salted trace IDs ----------
SALT_SECRET = b"vendor-held-salt-secret-sample"
def leaf_hash(trace_id: str, salt: bytes) -> bytes:
    return hashlib.sha256(b"\x00" + salt + trace_id.encode()).digest()
def node_hash(l: bytes, r: bytes) -> bytes:
    return hashlib.sha256(b"\x01" + l + r).digest()

def merkle(trace_ids, disclose_idx):
    level = [leaf_hash(t, hmac.new(SALT_SECRET, t.encode(), "sha256").digest()[:16]) for t in trace_ids]
    idx, proof = disclose_idx, []
    while len(level) > 1:
        nxt = []
        for i in range(0, len(level), 2):
            if i + 1 < len(level):
                nxt.append(node_hash(level[i], level[i + 1]))
            else:
                nxt.append(level[i])  # odd node carried up unchanged
        sib = idx ^ 1
        if sib < len(level):
            proof.append({"side": "L" if sib < idx else "R", "hash": level[sib].hex()})
        idx //= 2
        level = nxt
    return level[0].hex(), proof

# ---------- synthetic periods ----------
PERIODS = [
    dict(seq=1, start="2026-07-13", end="2026-08-11", issued="2026-08-12T09:00:00+10:00", traced_req=318402,
         cov_bedrock=97.9, cov_openai=99.2, pii_pct=100, grd_pct=99.8, grd_block=0.41,
         eval_status="pass", eval_note="Eval suite v7 run 2026-08-05 on prompt v13 and current models; pass rate 94.2% (threshold 90%).",
         eval_val="94.2% pass, last run 6 days before period end",
         tool_err=0.62, escal=3.1, incidents=[], changes=[
             {"date": "2026-07-21", "kind": "guardrail", "what": "Guardrail policy gp-2026.07 enabled: blocks policy-number exfiltration patterns"}]),
    dict(seq=2, start="2026-08-12", end="2026-09-10", issued="2026-09-11T09:00:00+10:00", traced_req=356880,
         cov_bedrock=98.4, cov_openai=99.4, pii_pct=100, grd_pct=99.9, grd_block=0.38,
         eval_status="pass", eval_note="Eval suite v8 run 2026-09-04 on prompt v14 before release; pass rate 95.0% (threshold 90%).",
         eval_val="95.0% pass on prompt v14, run before release",
         tool_err=0.55, escal=2.8, incidents=[
             {"id": "INC-0007", "opened": "2026-08-27T14:12:00+10:00", "acknowledged": "2026-08-27T14:31:00+10:00", "resolved": "2026-08-27T16:05:00+10:00",
              "summary": "Guardrail block rate spiked to 4.6% for 40 minutes after an upstream claims-form change; rule tuned, no customer data exposed"}],
         changes=[{"date": "2026-09-04", "kind": "prompt", "what": "Triage system prompt v13 to v14 (adds hail-damage routing)"}]),
    dict(seq=3, start="2026-09-11", end="2026-10-10", issued="2026-10-11T09:00:00+11:00", traced_req=401227,
         cov_bedrock=98.6, cov_openai=99.5, pii_pct=100, grd_pct=99.9, grd_block=0.36,
         eval_status="attention", eval_note="Prompt v15 went live 2026-10-06. No eval run recorded on v15 by period end (rule: run before release). Last run: suite v8 on v14, pass rate 95.0%.",
         eval_val="No run on live prompt v15 (live 4 days)",
         tool_err=0.49, escal=2.6, incidents=[],
         changes=[{"date": "2026-09-22", "kind": "model", "what": "Embedding model switched to text-embedding-3-small (from -large) for retrieval"},
                  {"date": "2026-10-06", "kind": "prompt", "what": "Triage system prompt v14 to v15 (stricter escalation wording)"}]),
]

def tokens(req, per):  # rough token totals
    return int(req * per)

envelopes, prev_digest = [], None
for p in PERIODS:
    # provider-side usage vs traced usage
    bed_req_billed = p["traced_req"] + int(p["traced_req"] * (100 - p["cov_bedrock"]) / p["cov_bedrock"])
    bed_tok_billed = tokens(bed_req_billed, 2140)
    bed_tok_traced = round(bed_tok_billed * p["cov_bedrock"] / 100)
    oai_req_billed = int(p["traced_req"] * 1.6)
    oai_tok_billed = tokens(oai_req_billed, 310)
    oai_tok_traced = round(oai_tok_billed * p["cov_openai"] / 100)
    total_billed = bed_tok_billed + oai_tok_billed
    total_traced = bed_tok_traced + oai_tok_traced
    cov_total = round(100 * total_traced / total_billed, 1)

    trace_ids = [f"tr_{p['seq']}_{i:07d}" for i in range(p["traced_req"])]
    disclose = random.randrange(p["traced_req"])
    root, proof = merkle(trace_ids, disclose)
    salt = hmac.new(SALT_SECRET, trace_ids[disclose].encode(), "sha256").digest()[:16]

    status_eval = p["eval_status"]
    objectives = [
        dict(id="EO-LOG-01", title="Trace coverage", status="pass", threshold=">= 98.0% of provider-billed tokens traced",
             measured=f"{cov_total}% of billed tokens traced", basis="Traced token counts reconciled hourly against AWS Bedrock CloudWatch and the OpenAI Usage API, scoped to production keys.",
             maps=["EU AI Act Art. 12", "AIUC-1 E Accountability", "ISO/IEC 42001 A.6.2.8*", "APRA: continuous monitoring"]),
        dict(id="EO-LIN-01", title="Model and provider lineage", status="pass", threshold="Every model in use is on the declared inventory",
             measured="3 of 3 models observed are declared", basis="gen_ai.provider.name and gen_ai.request.model / gen_ai.response.model on every span; legacy gen_ai.system accepted.",
             maps=["APRA: AI supply-chain mapping", "CSA AICM STA", "AIUC-1 E Accountability"]),
        dict(id="EO-CHG-01", title="Material-change log", status="pass", threshold="Every model, prompt or guardrail change is logged with a date",
             measured=f"{len(p['changes'])} change(s) logged, 0 unlogged transitions", basis="Model-name and prompt-version transitions detected in spans, matched to the vendor change register.",
             maps=["APRA: material change notification", "CSA AICM CCC", "ISO/IEC 42001 A.6.2.6*"]),
        dict(id="EO-PII-01", title="PII redaction coverage", status="pass", threshold="100% of spans tagged as personal data pass the redaction processor",
             measured=f"{p['pii_pct']}% of tagged spans redacted", basis="Redaction-processor events joined to spans carrying data_class=personal.",
             maps=["AIUC-1 A Data & Privacy", "CSA AICM DSP", "Privacy Act APP 11"]),
        dict(id="EO-CNT-01", title="Content minimisation", status="pass", threshold="No prompt or completion text exported to telemetry",
             measured="0 spans with message content attributes", basis="Absence of opt-in gen_ai.input.messages / gen_ai.output.messages attributes across all exported spans.",
             maps=["AIUC-1 A Data & Privacy", "CSA AICM DSP"]),
        dict(id="EO-GRD-01", title="Guardrail coverage", status="pass", threshold=">= 99.5% of LLM calls pass input and output guardrails",
             measured=f"{p['grd_pct']}% covered; {p['grd_block']}% blocked", basis="Guardrail decision events per call ID, compared with the traced call count.",
             maps=["AIUC-1 B Security", "AIUC-1 C Safety"]),
        dict(id="EO-EVAL-01", title="Eval freshness", status=status_eval, threshold="Eval suite run on each live model and prompt version before release; pass rate >= 90%",
             measured=p["eval_val"], basis=p["eval_note"],
             maps=["AIUC-1 D Reliability", "ISO/IEC 42001 A.6.2.4*"]),
        dict(id="EO-INC-01", title="Incident register", status="pass", threshold="Every guardrail spike or error burst has an incident record acknowledged within 60 min",
             measured=f"{len(p['incidents'])} incident(s); all acknowledged in time", basis="Error-rate and guardrail-rate anomaly windows matched to the incident register.",
             maps=["AIUC-1 E Accountability", "CSA AICM SEF", "APRA CPS 230"]),
        dict(id="EO-AGT-01", title="Agent action safety", status="pass", threshold="Tool-call error rate < 1.0%; every refund or payout tool call escalated to a human",
             measured=f"{p['tool_err']}% tool errors; {p['escal']}% of runs escalated; 100% of payout calls escalated", basis="execute_tool spans inside invoke_agent trees; payout tool calls matched to human-approval events.",
             maps=["AIUC-1 C Safety", "AIUC-1 D Reliability"]),
        dict(id="EO-RES-01", title="Inference residency", status="pass", threshold="All generation runs in ap-southeast-2; any other region is a declared subprocessor",
             measured="100% of generation in ap-southeast-2; embeddings via declared US subprocessor", basis="Bedrock region on every generation span; OpenAI embedding calls receive redacted text only.",
             maps=["APRA CPS 230", "APRA: AI supply-chain mapping", "Privacy Act APP 8"]),
    ]

    predicate = {
        "pack_schema": "assurance-pack/v0.1",
        "pack_id": f"KST-CTA-{p['seq']:04d}",
        "seq_no": p["seq"],
        "prev_pack_digest": f"sha256:{prev_digest}" if prev_digest else None,
        "issued_at": p["issued"],
        "period": {"start": p["start"], "end": p["end"], "timezone": "Australia/Sydney"},
        "sample_notice": "SAMPLE. Fictional vendor, synthetic telemetry, throwaway signing key. Not evidence about any real company.",
        "subject": {
            "organisation": "Kestrel Assist Pty Ltd (fictional)",
            "ai_system": "Claims Triage Agent",
            "environment": "production",
            "description": "LLM agent that reads home-insurance claim lodgements, routes them and drafts customer replies for an APRA-regulated insurer.",
        },
        "profiles": ["Enterprise AI security review", "APRA AI letter (30 Apr 2026)"],
        "assurance_level": {"level": 1, "meaning": "Telemetry and provider-usage exports supplied by the vendor; reconciliation computed by the issuer."},
        "sources": [
            {"kind": "traces", "system": "Langfuse Cloud (EU region)", "method": "Observations API export, aggregates only"},
            {"kind": "provider_usage", "system": "AWS Bedrock CloudWatch (ap-southeast-2)", "method": "Invocations and token counts by ModelId"},
            {"kind": "provider_usage", "system": "OpenAI Usage API", "method": "Completions and embeddings usage by project"},
            {"kind": "registers", "system": "Vendor change and incident registers", "method": "CSV export"},
        ],
        "coverage": {
            "basis": "tokens",
            "total_pct": cov_total,
            "providers": [
                {"provider": "AWS Bedrock", "region": "ap-southeast-2", "billed_tokens": bed_tok_billed, "traced_tokens": bed_tok_traced,
                 "billed_requests": bed_req_billed, "traced_requests": p["traced_req"]},
                {"provider": "OpenAI", "region": "us", "billed_tokens": oai_tok_billed, "traced_tokens": oai_tok_traced,
                 "billed_requests": oai_req_billed, "traced_requests": round(oai_req_billed * p["cov_openai"] / 100)},
            ],
        },
        "lineage": [
            {"provider": "AWS Bedrock", "model": "anthropic.claude-sonnet-4-5", "role": "triage and reply drafting", "region": "ap-southeast-2", "share_of_calls_pct": 71.3},
            {"provider": "AWS Bedrock", "model": "anthropic.claude-haiku-4-5", "role": "intent classification", "region": "ap-southeast-2", "share_of_calls_pct": 28.7},
            {"provider": "OpenAI", "model": "text-embedding-3-small" if p["seq"] == 3 else "text-embedding-3-large", "role": "retrieval embeddings (redacted text)", "region": "us", "share_of_calls_pct": None},
        ],
        "changes": p["changes"],
        "incidents": p["incidents"],
        "objectives": objectives,
        "commitments": {
            "trace_merkle_root": root,
            "leaf_count": p["traced_req"],
            "leaf": "sha256(0x00 || salt16 || trace_id)",
            "node": "sha256(0x01 || left || right); odd node carried up",
            "disclosed": {"trace_id": trace_ids[disclose], "leaf_index": disclose, "salt": salt.hex(), "proof": proof},
        },
        "limitations": [
            "Proves what the exported telemetry shows. It does not prove the model behaves well on inputs that never occurred.",
            "Coverage below 100% means some provider-billed usage has no matching trace; untraced usage is not assessed.",
            "Level 1: the vendor supplied the provider-usage exports. Level 2 would pull them directly with a read-only key.",
            "Framework references marked * are Annex A control IDs pending check against a licensed copy of ISO/IEC 42001.",
        ],
    }

    pred_bytes = jcs(predicate)
    statement = {
        "_type": "https://in-toto.io/Statement/v1",
        "subject": [{"name": predicate["pack_id"], "digest": {"sha256": sha256hex(pred_bytes)}}],
        "predicateType": PREDICATE_TYPE,
        "predicate": predicate,
    }
    payload = jcs(statement)
    ptype = "application/vnd.in-toto+json"
    sig = key.sign(pae(ptype, payload), ec.ECDSA(hashes.SHA256()))
    env = {"payloadType": ptype, "payload": base64.b64encode(payload).decode(),
           "signatures": [{"keyid": KID, "sig": base64.b64encode(sig).decode()}]}
    envelopes.append(env)
    prev_digest = sha256hex(payload)
    (OUT / "pass" / f"{predicate['pack_id']}.dsse.json").write_text(json.dumps(env, indent=2))
    print(predicate["pack_id"], "digest", prev_digest, "cov", cov_total, "root", root[:16])


# ---------- fail vectors ----------
statements = [json.loads(base64.b64decode(e["payload"])) for e in envelopes]
def sign_statement(st, k=key, kid=KID):
    payload = jcs(st)
    sig = k.sign(pae("application/vnd.in-toto+json", payload), ec.ECDSA(hashes.SHA256()))
    return {"payloadType": "application/vnd.in-toto+json", "payload": base64.b64encode(payload).decode(),
            "signatures": [{"keyid": kid, "sig": base64.b64encode(sig).decode()}]}
def write(name, env): (OUT / "fail" / name).write_text(json.dumps(env, indent=2))
import copy
# 1. payload edited after signing (signature and subject digest both fail)
st = copy.deepcopy(statements[2]); st["predicate"]["coverage"]["total_pct"] = 99.9
env = dict(envelopes[2]); env["payload"] = base64.b64encode(jcs(st)).decode(); write("tampered-payload.dsse.json", env)
# 2. signed by a different key
write("wrong-key.dsse.json", sign_statement(statements[0], ec.generate_private_key(ec.SECP256R1()), KID))
# 3. chain broken: pack 2 points at the wrong previous digest, correctly signed
st = copy.deepcopy(statements[1]); st["predicate"]["prev_pack_digest"] = "sha256:" + "0" * 64
st["subject"][0]["digest"]["sha256"] = sha256hex(jcs(st["predicate"])); write("broken-chain-0002.dsse.json", sign_statement(st))
# 4. disclosed trace proof does not reach the root, correctly signed
st = copy.deepcopy(statements[0]); st["predicate"]["commitments"]["disclosed"]["salt"] = "00" * 16
st["subject"][0]["digest"]["sha256"] = sha256hex(jcs(st["predicate"])); write("bad-merkle-proof.dsse.json", sign_statement(st))
# 5. subject digest does not match the predicate, correctly signed
st = copy.deepcopy(statements[0]); st["subject"][0]["digest"]["sha256"] = "f" * 64; write("subject-digest-mismatch.dsse.json", sign_statement(st))

T = {"signature": True, "subject_digest": True, "chain": None, "merkle": True, "verified": True}
def exp(**kw): d = dict(T); d.update(kw); d["verified"] = all(v is not False for k, v in d.items() if k != "verified"); return d
cases = [
  {"name": "pass-0001", "file": "pass/KST-CTA-0001.dsse.json", "prev": None, "expect": exp()},
  {"name": "pass-0002", "file": "pass/KST-CTA-0002.dsse.json", "prev": "pass/KST-CTA-0001.dsse.json", "expect": exp(chain=True)},
  {"name": "pass-0003", "file": "pass/KST-CTA-0003.dsse.json", "prev": "pass/KST-CTA-0002.dsse.json", "expect": exp(chain=True)},
  {"name": "tampered-payload", "file": "fail/tampered-payload.dsse.json", "prev": "pass/KST-CTA-0002.dsse.json", "expect": exp(signature=False, subject_digest=False, chain=True)},
  {"name": "wrong-key", "file": "fail/wrong-key.dsse.json", "prev": None, "expect": exp(signature=False)},
  {"name": "broken-chain", "file": "fail/broken-chain-0002.dsse.json", "prev": "pass/KST-CTA-0001.dsse.json", "expect": exp(chain=False)},
  {"name": "bad-merkle-proof", "file": "fail/bad-merkle-proof.dsse.json", "prev": None, "expect": exp(merkle=False)},
  {"name": "subject-digest-mismatch", "file": "fail/subject-digest-mismatch.dsse.json", "prev": None, "expect": exp(subject_digest=False)},
]
(OUT / "manifest.json").write_text(json.dumps({"version": 1, "jwks": "test-jwks.json", "public_key_pem": "test-public-key.pem",
  "chain_rule": "chain is null when prev is null; otherwise predicate.prev_pack_digest must equal 'sha256:' + hex(sha256(base64decode(prev.payload)))",
  "cases": cases}, indent=2))
print("wrote", len(cases), "cases")
