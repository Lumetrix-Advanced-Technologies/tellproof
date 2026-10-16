"""Offline verifier for assurance packs (sample).

Usage: python verify.py ISSUER-KEY KST-CTA-0001.dsse.json KST-CTA-0002.dsse.json ...
ISSUER-KEY is the issuer's public key in PEM, or its JWKS (keys by kid, with nbf/exp
and a revoked list, D-042). Pass the packs oldest first so the chain can be checked.
Checks: DSSE signature (by a key that may vouch for the pack's issued_at), subject
digest == sha256(JCS(predicate)), chain links, disclosed-trace Merkle proof.
Exit status: 0 verified, 1 a check failed, 2 the key file is malformed.
"""
import base64, hashlib, json, re, sys
from datetime import datetime
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.exceptions import InvalidSignature


def norm(o):
    if isinstance(o, float):
        return int(o) if o.is_integer() else o
    if isinstance(o, dict):
        return {k: norm(v) for k, v in o.items()}
    if isinstance(o, list):
        return [norm(v) for v in o]
    return o


def jcs(o):
    return json.dumps(norm(o), sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def pae(t, body):
    t = t.encode()
    return b"DSSEv1 %d %s %d %s" % (len(t), t, len(body), body)


class KeyFileError(ValueError):
    pass


MAX_SAFE = 2**53 - 1
JWK_MEMBERS = {"kty", "crv", "kid", "use", "alg", "x", "y", "nbf", "exp"}
ISSUED_AT = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$")


def numeric_date(v, what):
    """A JSON number with a whole value in [0, 2^53-1] (D-042); 1e9 and 1000000000 are the same number."""
    if isinstance(v, bool) or not isinstance(v, (int, float)) or (isinstance(v, float) and not v.is_integer()) or not 0 <= v <= MAX_SAFE:
        raise KeyFileError(f"{what} must be a whole number of seconds (NumericDate)")
    return int(v)


def b64u(s):
    if not isinstance(s, str) or "=" in s:
        raise KeyFileError("x and y must be unpadded base64url")
    return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))


def load_keys(path):
    """PEM: one key that may sign under any kid. JWKS: keys by kid with validity, and revoked kids."""
    raw = open(path, "rb").read()
    if not raw.lstrip().startswith(b"{"):
        return {None: (serialization.load_pem_public_key(raw), None, None)}, set()
    try:
        doc = json.loads(raw)
    except ValueError as e:
        raise KeyFileError(f"jwks: {e}") from None
    if not isinstance(doc, dict) or set(doc) - {"keys", "revoked"} or not isinstance(doc.get("keys"), list) or not doc["keys"]:
        raise KeyFileError("jwks: want {keys: [...], revoked?: [...]}")
    keys = {}
    for i, k in enumerate(doc["keys"]):
        if not isinstance(k, dict) or set(k) - JWK_MEMBERS:
            raise KeyFileError(f"jwks: keys[{i}] has members a JWK here cannot have")
        for f, v in k.items():
            if f not in ("nbf", "exp") and not isinstance(v, str):
                raise KeyFileError(f"jwks: keys[{i}].{f} is not a string")
        if not k.get("kid") or k.get("kty") != "EC" or k.get("crv") != "P-256" or k.get("alg", "ES256") not in ("", "ES256") or k.get("use", "sig") not in ("", "sig"):
            raise KeyFileError(f"jwks: keys[{i}] must be an ES256 (EC P-256) signing key with a kid")
        if k["kid"] in keys:
            raise KeyFileError(f"jwks: duplicate kid {k['kid']!r}")
        nbf = numeric_date(k["nbf"], "nbf") if "nbf" in k else None
        exp = numeric_date(k["exp"], "exp") if "exp" in k else None
        if nbf is not None and exp is not None and exp <= nbf:
            raise KeyFileError(f"jwks: keys[{i}]: exp must be after nbf")
        x, y = b64u(k.get("x")), b64u(k.get("y"))
        if len(x) != 32 or len(y) != 32:
            raise KeyFileError(f"jwks: keys[{i}]: x and y must be 32 bytes")
        try:
            pub = ec.EllipticCurvePublicNumbers(int.from_bytes(x, "big"), int.from_bytes(y, "big"), ec.SECP256R1()).public_key()
        except ValueError:
            raise KeyFileError(f"jwks: keys[{i}]: not a valid P-256 point") from None
        keys[k["kid"]] = (pub, nbf, exp)
    revoked = set()
    if not isinstance(doc.get("revoked", []), list):
        raise KeyFileError("jwks: revoked must be an array")
    for i, r in enumerate(doc.get("revoked", [])):
        if not isinstance(r, dict) or set(r) - {"kid", "revoked_at", "reason"} or not isinstance(r.get("kid"), str) or not r.get("kid") \
                or "revoked_at" not in r or ("reason" in r and not isinstance(r["reason"], str)):
            raise KeyFileError(f"jwks: revoked[{i}] needs a kid and a revoked_at NumericDate")
        numeric_date(r["revoked_at"], "revoked_at")
        if r["kid"] in revoked:
            raise KeyFileError(f"jwks: kid {r['kid']!r} is revoked twice")
        revoked.add(r["kid"])
    return keys, revoked


def vouches(kid, entry, revoked, issued_at):
    """None when the key may vouch for a pack issued at issued_at, else why not (D-042)."""
    if kid in revoked:
        return "the signing key has been revoked"
    _, nbf, exp = entry
    if nbf is None and exp is None:
        return None
    try:
        if not isinstance(issued_at, str) or not ISSUED_AT.match(issued_at):
            raise ValueError
        t = int(datetime.fromisoformat(issued_at.replace("Z", "+00:00")).timestamp() // 1)
    except ValueError:
        return "issued_at is not an RFC 3339 date-time, so the key's validity cannot be checked"
    if nbf is not None and t < nbf:
        return "the pack was issued before its signing key's nbf"
    if exp is not None and t >= exp:
        return "the pack was issued at or after its signing key's exp"
    return None


def check(keyset, path, prev_digest):
    keys, revoked = keyset
    env = json.load(open(path))
    payload = base64.b64decode(env["payload"])
    sig = base64.b64decode(env["signatures"][0]["sig"])
    kid = env["signatures"][0].get("keyid")
    entry = keys.get(None) or keys.get(kid)
    ok = True
    if entry is None:
        print(f"  signature ............ INVALID (unknown key {kid!r})")
        return None, False
    try:
        entry[0].verify(sig, pae(env["payloadType"], payload), ec.ECDSA(hashes.SHA256()))
    except InvalidSignature:
        print("  signature ............ INVALID")
        return None, False
    st = json.loads(payload)
    pred = st["predicate"]
    why = vouches(kid, entry, revoked, pred.get("issued_at"))
    if why:
        print(f"  signature ............ REFUSED: {why}")
        return None, False
    print("  signature ............ valid")
    d = hashlib.sha256(jcs(pred)).hexdigest()
    match = d == st["subject"][0]["digest"]["sha256"]
    print("  subject digest ....... " + ("matches" if match else "MISMATCH"))
    ok &= match
    if prev_digest is not None:
        link = pred["prev_pack_digest"] == f"sha256:{prev_digest}"
        print("  chain link ........... " + ("links to previous pack" if link else "BROKEN"))
        ok &= link
    c = pred["commitments"]
    disc = c["disclosed"]
    if disc is None:  # schema-valid: no trace disclosed, so there is no proof to check (D-016)
        print("  disclosed trace ...... none disclosed (not checked)")
        return hashlib.sha256(payload).hexdigest(), ok
    h = hashlib.sha256(b"\x00" + bytes.fromhex(disc["salt"]) + disc["trace_id"].encode()).digest()
    for step in disc["proof"]:
        s = bytes.fromhex(step["hash"])
        h = hashlib.sha256(b"\x01" + (s + h if step["side"] == "L" else h + s)).digest()
    inc = h.hex() == c["trace_merkle_root"]
    print("  disclosed trace ...... " + ("included in commitment" if inc else "NOT included"))
    ok &= inc
    return hashlib.sha256(payload).hexdigest(), ok


if __name__ == "__main__":
    try:
        keyset = load_keys(sys.argv[1])
    except KeyFileError as e:
        print(f"key file: {e}")
        sys.exit(2)
    prev, all_ok = None, True
    for p in sys.argv[2:]:
        print(p)
        prev, ok = check(keyset, p, prev)
        all_ok &= ok
    print("RESULT:", "VERIFIED" if all_ok else "FAILED")
    sys.exit(0 if all_ok else 1)
