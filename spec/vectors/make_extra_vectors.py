"""Extra vectors: cases every verifier must agree on beyond the core manifest.

Writes spec/vectors/extra/*.dsse.json and spec/vectors/extra-manifest.json, signed
with the same TEST-ONLY key as make_vectors.py. It reads the existing pass vectors
and never changes them. Each case changes one thing about pass/KST-CTA-0001 and
re-signs it, so the expected result names only that defect.

expect is either a result object (same shape as manifest.json) or "error": the
payload is not a valid v0.1 pack statement (D-016, D-018), so a verifier must
refuse to evaluate it and must never report it verified.

Run from the repo root:  python3 spec/vectors/make_extra_vectors.py
"""
import base64, copy, hashlib, json
from pathlib import Path
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec

OUT = Path(__file__).resolve().parent
KEY = serialization.load_pem_private_key((OUT / "test-private-key.pem").read_bytes(), None)
KID = "test-p256-2026-10"
INTOTO = "application/vnd.in-toto+json"
BATCH = "application/vnd.lumetrix.plumbline.batch+json"


def norm(o):  # same canonical subset as make_vectors.py
    if isinstance(o, float):
        return int(o) if o.is_integer() else round(o, 4)
    if isinstance(o, dict):
        return {k: norm(v) for k, v in o.items()}
    if isinstance(o, list):
        return [norm(v) for v in o]
    return o


def jcs(o) -> bytes:
    return json.dumps(norm(o), sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


def pae(ptype: str, body: bytes) -> bytes:
    t = ptype.encode()
    return b"DSSEv1 %d %s %d %s" % (len(t), t, len(body), body)


def envelope(payload: bytes, ptype=INTOTO, kid=KID) -> dict:
    sig = KEY.sign(pae(ptype, payload), ec.ECDSA(hashes.SHA256()))
    return {"payloadType": ptype, "payload": base64.b64encode(payload).decode(),
            "signatures": [{"keyid": kid, "sig": base64.b64encode(sig).decode()}]}


def fix_digest(st: dict) -> dict:
    st["subject"][0]["digest"]["sha256"] = hashlib.sha256(jcs(st["predicate"])).hexdigest()
    return st


base_payload = base64.b64decode(json.loads((OUT / "pass" / "KST-CTA-0001.dsse.json").read_text())["payload"])
BASE = json.loads(base_payload)
assert jcs(BASE) == base_payload, "pass vector 1 must already be canonical"


def edited(f) -> dict:
    st = copy.deepcopy(BASE)
    f(st)
    return fix_digest(st)


def pred(st):
    return st["predicate"]


ALL_OK = {"signature": True, "subject_digest": True, "chain": None, "merkle": True, "verified": True}
cases = []


def add(name, env, expect, why):
    (OUT / "extra").mkdir(exist_ok=True)
    (OUT / "extra" / f"{name}.dsse.json").write_text(json.dumps(env, indent=2) + "\n")
    cases.append({"name": name, "file": f"extra/{name}.dsse.json", "prev": None, "expect": expect, "why": why})


# ---- evaluated: a result object is expected ----
add("pass-no-disclosure", envelope(jcs(edited(lambda st: pred(st)["commitments"].update(disclosed=None)))),
    dict(ALL_OK, merkle=None), "disclosed is null (schema-valid): the Merkle check is not run (D-016)")
add("wrong-payload-type", envelope(base_payload, ptype=BATCH),
    dict(ALL_OK, signature=False, verified=False), "signed as an agent batch, not a pack: never accept it as a pack")
add("unknown-keyid", envelope(base_payload, kid="not-in-jwks"),
    dict(ALL_OK, signature=False, verified=False), "keyid is not in the JWKS: keys are looked up by kid (D-014)")

# ---- not a valid v0.1 pack statement: "error" is expected ----
dup = base_payload.replace(b'{"_type":', b'{"_type":"https://in-toto.io/Statement/v1","_type":', 1)
add("reject-duplicate-key", envelope(dup), "error", "duplicate object key (I-JSON)")
add("reject-non-canonical", envelope(json.dumps(BASE, indent=2, sort_keys=True, ensure_ascii=False).encode()), "error",
    "payload is not JCS(statement) (D-018)")
add("reject-unknown-field", envelope(jcs(edited(lambda st: pred(st).update(owner="x")))), "error",
    "field outside the v0.1 schema")
add("reject-case-variant-key", envelope(jcs(edited(lambda st: pred(st).update(Prev_Pack_Digest="sha256:" + "0" * 64)))), "error",
    "a key differing only in case from a schema field (D-016)")
add("reject-wrong-predicate-type", envelope(jcs(edited(lambda st: st.update(predicateType="https://example.com/other/v1")))), "error",
    "predicateType is not the pack v0.1 type")
add("reject-null-string", envelope(jcs(edited(lambda st: pred(st).update(pack_id=None)))), "error",
    "a required string is null")
add("reject-null-array", envelope(jcs(edited(lambda st: pred(st).update(limitations=None)))), "error",
    "a required array is null")
add("reject-fractional-integer", envelope(jcs(edited(lambda st: pred(st).update(seq_no=1.5)))), "error",
    "seq_no is not an integer")
add("reject-empty-optional", envelope(jcs(edited(lambda st: pred(st)["subject"].update(description="")))), "error",
    "an optional field is present but empty (D-016)")

desc = BASE["predicate"]["subject"]["description"].encode()
lone = base_payload.replace(desc, b"\\ud800" + desc[1:], 1)
add("reject-lone-surrogate", envelope(lone), "error", "a string holds an unpaired surrogate escape (I-JSON)")
bad_utf8 = base_payload.replace(desc, b"\xff" + desc[1:], 1)
add("reject-invalid-utf8", envelope(bad_utf8), "error", "payload is not valid UTF-8")

manifest = {
    "version": 1,
    "jwks": "test-jwks.json",
    "note": "Cases beyond manifest.json that every verifier must agree on. expect is a result object, or \"error\": "
            "not a valid v0.1 pack statement, so it must not be evaluated or reported verified. Generated by make_extra_vectors.py.",
    "cases": cases,
}
(OUT / "extra-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
print("wrote", len(cases), "extra cases")
