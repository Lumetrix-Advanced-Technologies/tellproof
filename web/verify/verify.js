// Plumbline pack verification for the browser (and node --test). Pure functions,
// Web Crypto only, no dependencies. Mirrors internal/pack in Go: both must give the
// same result for every case in spec/vectors/manifest.json and extra-manifest.json.

const enc = new TextEncoder();
const subtle = globalThis.crypto.subtle;

export const PAYLOAD_TYPE = "application/vnd.in-toto+json";
export const STATEMENT_TYPE = "https://in-toto.io/Statement/v1";
export const PREDICATE_TYPE = "https://lumetrix.example/plumbline/pack/v0.1"; // placeholder domain until D-008
export const PACK_SCHEMA = "assurance-pack/v0.1";

// PackError: the input is not a valid envelope, statement or JWKS, so it cannot
// be evaluated. It is never "verified".
export class PackError extends Error {
  constructor(message) {
    super(message);
    this.name = "PackError";
  }
}

// ---------- bytes ----------

export const concat = (...parts) => {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
};
export const hex = (bytes) => Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
const unhex = (h) => Uint8Array.from(h.match(/../g) ?? [], (x) => parseInt(x, 16));
export const sha256 = async (bytes) => new Uint8Array(await subtle.digest("SHA-256", bytes));

const B64 = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;
const B64URL = /^[A-Za-z0-9_-]*$/;

// Standard, padded base64 only (as Go's encoding/json decodes it).
export function b64decode(s) {
  if (typeof s !== "string" || !B64.test(s)) throw new PackError("not standard padded base64");
  return Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
}

function b64urlDecode(s) {
  if (typeof s !== "string" || !B64URL.test(s) || s.length % 4 === 1) throw new PackError("not unpadded base64url");
  const std = s.replace(/-/g, "+").replace(/_/g, "/");
  return Uint8Array.from(atob(std + "=".repeat((4 - (std.length % 4)) % 4)), (c) => c.charCodeAt(0));
}

const isObject = (v) => v !== null && typeof v === "object" && !Array.isArray(v);

// ---------- RFC 8785 ----------

// jcs returns the canonical JSON text of a value from JSON.parse: keys sorted by
// UTF-16 code units (Array.prototype.sort's order), ECMAScript number form, and
// only '"', '\' and U+0000-U+001F escaped (what JSON.stringify does for strings).
export function jcs(v) {
  if (v === null) return "null";
  switch (typeof v) {
    case "boolean":
      return v ? "true" : "false";
    case "number":
      if (!Number.isFinite(v)) throw new PackError("number is NaN or infinite");
      return JSON.stringify(v);
    case "string":
      return jcsString(v);
    case "object":
      if (Array.isArray(v)) return "[" + v.map(jcs).join(",") + "]";
      return "{" + Object.keys(v).sort().map((k) => jcsString(k) + ":" + jcs(v[k])).join(",") + "}";
  }
  throw new PackError(`cannot canonicalize a ${typeof v}`);
}

function jcsString(s) {
  // JSON.parse accepts unpaired surrogate escapes; I-JSON does not.
  if (!s.isWellFormed()) throw new PackError("a string holds an unpaired surrogate");
  return JSON.stringify(s);
}

// parseCanonical decodes UTF-8 bytes that must already be canonical JSON. This
// is how the payload is checked: JSON.parse cannot report duplicate keys, but a
// duplicate (or any other non-canonical form) makes the re-encoding differ.
export function parseCanonical(bytes) {
  let text;
  try {
    text = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes);
  } catch {
    throw new PackError("payload is not valid UTF-8");
  }
  let v;
  try {
    v = JSON.parse(text);
  } catch (e) {
    throw new PackError(`payload is not JSON: ${e.message}`);
  }
  if (jcs(v) !== text) throw new PackError("payload is not in canonical form (RFC 8785), or repeats a key");
  return v;
}

// ---------- DSSE ----------

export function pae(type, body) {
  const t = enc.encode(type);
  return concat(enc.encode(`DSSEv1 ${t.length} `), t, enc.encode(` ${body.length} `), body);
}

// derToRaw converts an ASN.1 DER ECDSA P-256 signature to the raw r||s form Web
// Crypto wants. It is as strict as Go's parser: short-form lengths, minimal
// non-negative integers, no trailing bytes.
export function derToRaw(der) {
  const fail = () => {
    throw new PackError("signature is not a DER-encoded ECDSA P-256 signature");
  };
  if (!(der instanceof Uint8Array) || der.length < 8 || der[0] !== 0x30 || der[1] > 0x7f || der[1] !== der.length - 2) fail();
  let i = 2;
  const int = () => {
    if (der[i] !== 0x02) fail();
    const len = der[i + 1];
    if (len === undefined || len < 1 || len > 33 || i + 2 + len > der.length) fail();
    let v = der.subarray(i + 2, i + 2 + len);
    if (v[0] & 0x80) fail(); // negative
    if (v[0] === 0 && len > 1) {
      if (!(v[1] & 0x80)) fail(); // non-minimal
      v = v.subarray(1);
    }
    if (v.length > 32) fail();
    i += 2 + len;
    const out = new Uint8Array(32);
    out.set(v, 32 - v.length);
    return out;
  };
  const r = int();
  const s = int();
  if (i !== der.length) fail();
  return concat(r, s);
}

const exactKeys = (o, allowed, what) => {
  for (const k of Object.keys(o)) if (!allowed.includes(k)) throw new PackError(`${what}: unknown field ${JSON.stringify(k)}`);
};

// decodeEnvelope checks an envelope object as strictly as Go's dsse.Decode.
export function decodeEnvelope(env) {
  if (!isObject(env)) throw new PackError("envelope is not a JSON object");
  exactKeys(env, ["payloadType", "payload", "signatures"], "envelope");
  if (typeof env.payloadType !== "string" || env.payloadType === "") throw new PackError("envelope: payloadType is missing");
  const payload = b64decode(env.payload ?? "");
  if (payload.length === 0) throw new PackError("envelope: payload is missing");
  if (!Array.isArray(env.signatures) || env.signatures.length === 0) throw new PackError("envelope: signatures is empty");
  const signatures = env.signatures.map((s, i) => {
    if (!isObject(s)) throw new PackError(`envelope: signatures[${i}] is not an object`);
    exactKeys(s, ["keyid", "sig"], `envelope: signatures[${i}]`);
    if (s.keyid != null && typeof s.keyid !== "string") throw new PackError(`envelope: signatures[${i}].keyid is not a string`);
    const sig = b64decode(s.sig ?? "");
    if (sig.length === 0) throw new PackError(`envelope: signatures[${i}].sig is empty`);
    return { keyid: s.keyid ?? "", sig };
  });
  return { payloadType: env.payloadType, payload, signatures };
}

async function checkSignature(env, keys) {
  if (env.payloadType !== PAYLOAD_TYPE) {
    return { ok: false, error: `unexpected payload type ${JSON.stringify(env.payloadType)}, want ${JSON.stringify(PAYLOAD_TYPE)}` };
  }
  const data = pae(env.payloadType, env.payload);
  let firstError = null;
  for (const s of env.signatures) {
    const key = s.keyid === "" ? undefined : keys.get(s.keyid);
    if (!key) continue;
    let ok = false;
    try {
      ok = await subtle.verify({ name: "ECDSA", hash: "SHA-256" }, key, derToRaw(s.sig), data);
    } catch {
      ok = false;
    }
    if (ok) return { ok: true, keyId: s.keyid };
    firstError ??= `signature does not verify (keyid ${JSON.stringify(s.keyid)})`;
  }
  return { ok: false, error: firstError ?? `no signature from a trusted key: envelope keyids ${JSON.stringify(env.signatures.map((s) => s.keyid))}` };
}

// ---------- JWKS ----------

// NumericDate (D-042): a JSON number with a whole value in [0, 2^53-1].
const isNumericDate = (v) => typeof v === "number" && Number.isInteger(v) && v >= 0 && v <= Number.MAX_SAFE_INTEGER;

// importJWKS mirrors Go's pack.ParseJWKS (D-042): ES256 (EC P-256) keys by kid,
// each with an optional nbf and exp (NumericDate), an optional "revoked" list of
// {kid, revoked_at, reason}, no unknown members, coordinates checked on the
// curve. Returns a Map kid -> CryptoKey carrying .validity (kid -> {nbf, exp})
// and .revoked (kid -> revoked_at).
export async function importJWKS(jwks) {
  if (!isObject(jwks)) throw new PackError("jwks: not a JSON object");
  exactKeys(jwks, ["keys", "revoked"], "jwks");
  if (!Array.isArray(jwks.keys) || jwks.keys.length === 0) throw new PackError("jwks: no keys");
  const keys = new Map();
  const validity = new Map();
  for (const [i, k] of jwks.keys.entries()) {
    const where = `jwks: keys[${i}]`;
    if (!isObject(k)) throw new PackError(`${where} is not an object`);
    exactKeys(k, ["kty", "crv", "kid", "use", "alg", "x", "y", "nbf", "exp"], where);
    for (const f of Object.keys(k)) {
      if (f === "nbf" || f === "exp") {
        if (!isNumericDate(k[f])) throw new PackError(`${where}.${f} must be a whole number of seconds (NumericDate)`);
      } else if (typeof k[f] !== "string") throw new PackError(`${where}.${f} is not a string`);
    }
    if (!k.kid) throw new PackError(`${where}: kid is required`);
    if (k.kty !== "EC" || k.crv !== "P-256") throw new PackError(`${where}: want kty EC and crv P-256`);
    if (k.alg !== undefined && k.alg !== "" && k.alg !== "ES256") throw new PackError(`${where}: alg is not ES256`);
    if (k.use !== undefined && k.use !== "" && k.use !== "sig") throw new PackError(`${where}: use is not sig`);
    if (k.nbf !== undefined && k.exp !== undefined && k.exp <= k.nbf) throw new PackError(`${where}: exp must be after nbf`);
    let x, y;
    try {
      x = b64urlDecode(k.x ?? "");
      y = b64urlDecode(k.y ?? "");
    } catch {
      throw new PackError(`${where}: x and y must be unpadded base64url`);
    }
    if (x.length !== 32 || y.length !== 32) throw new PackError(`${where}: x and y must be 32 bytes`);
    if (keys.has(k.kid)) throw new PackError(`jwks: duplicate kid ${JSON.stringify(k.kid)}`);
    let key;
    try {
      key = await subtle.importKey("jwk", { kty: "EC", crv: "P-256", x: k.x, y: k.y }, { name: "ECDSA", namedCurve: "P-256" }, false, ["verify"]);
    } catch {
      throw new PackError(`${where}: not a valid P-256 point`);
    }
    keys.set(k.kid, key);
    validity.set(k.kid, { nbf: k.nbf, exp: k.exp });
  }
  const revoked = new Map();
  if (jwks.revoked !== undefined) {
    if (!Array.isArray(jwks.revoked)) throw new PackError("jwks: revoked must be an array");
    for (const [i, r] of jwks.revoked.entries()) {
      const where = `jwks: revoked[${i}]`;
      if (!isObject(r)) throw new PackError(`${where} is not an object`);
      exactKeys(r, ["kid", "revoked_at", "reason"], where);
      if (typeof r.kid !== "string" || r.kid === "" || !isNumericDate(r.revoked_at)) throw new PackError(`${where} needs a kid and a revoked_at NumericDate`);
      if (r.reason !== undefined && typeof r.reason !== "string") throw new PackError(`${where}.reason is not a string`);
      if (revoked.has(r.kid)) throw new PackError(`jwks: kid ${JSON.stringify(r.kid)} is revoked twice`);
      revoked.set(r.kid, r.revoked_at);
    }
  }
  Object.defineProperty(keys, "validity", { value: validity });
  Object.defineProperty(keys, "revoked", { value: revoked });
  return keys;
}

const ISSUED_AT = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(\.\d+)?(Z|([+-])(\d{2}):(\d{2}))$/;

// issuedAtSeconds parses an RFC 3339 date-time strictly (no Date.parse
// leniency) and returns whole Unix seconds, or null.
export function issuedAtSeconds(s) {
  const m = ISSUED_AT.exec(s);
  if (!m) return null;
  const [y, mo, d, h, mi, se] = m.slice(1, 7).map(Number);
  const ms = Date.UTC(y, mo - 1, d, h, mi, se);
  const t = new Date(ms);
  if (t.getUTCFullYear() !== y || t.getUTCMonth() !== mo - 1 || t.getUTCDate() !== d || t.getUTCHours() !== h || t.getUTCMinutes() !== mi || t.getUTCSeconds() !== se) return null;
  let offset = 0;
  if (m[8] !== "Z") {
    const oh = Number(m[10]), om = Number(m[11]);
    if (oh > 23 || om > 59) return null;
    offset = (m[9] === "+" ? 1 : -1) * (oh * 3600 + om * 60);
  }
  return ms / 1000 - offset;
}

// keyVouches mirrors Go's KeySet.Vouches: null when the key may vouch for a pack A revoked key vouches for nothing.
// issued at issuedAt, otherwise {status, message}: status is revoked,
// not_yet_valid, expired or bad_issued_at. A revoked key vouches for nothing.
export function keyVouches(keys, kid, issuedAt) {
  if (keys.revoked && keys.revoked.has(kid)) return { status: "revoked", message: "the signing key has been revoked" };
  const v = keys.validity ? keys.validity.get(kid) : undefined;
  if (!v || (v.nbf === undefined && v.exp === undefined)) return null;
  const t = issuedAtSeconds(issuedAt);
  if (t === null) return { status: "bad_issued_at", message: "issued_at is not an RFC 3339 date-time, so the key's validity cannot be checked" };
  if (v.nbf !== undefined && t < v.nbf) return { status: "not_yet_valid", message: "the pack was issued before its signing key's nbf" };
  if (v.exp !== undefined && t >= v.exp) return { status: "expired", message: "the pack was issued at or after its signing key's exp" };
  return null;
}

// ---------- statement shape (the Go v0.1 types, field for field) ----------

const S = "string"; // required string (may be empty)
const S_OPT = "string?"; // optional: absent, or a non-empty string
const I = "integer";
const N = "number";
const arr = (item, optional = false) => ({ arr: item, optional }); // optional: absent, or non-empty
const obj = (fields) => ({ obj: fields });
const nullable = (spec) => ({ nullable: spec }); // must be present; null or spec

const PREDICATE = obj({
  pack_schema: S,
  pack_id: S,
  seq_no: I,
  prev_pack_digest: nullable(S),
  issued_at: S,
  period: obj({ start: S, end: S, timezone: S }),
  sample_notice: S_OPT,
  subject: obj({ organisation: S, ai_system: S, environment: S, description: S_OPT }),
  profiles: arr(S),
  assurance_level: obj({ level: I, meaning: S }),
  sources: arr(obj({ kind: S, system: S, method: S })),
  coverage: obj({
    basis: S,
    total_pct: N,
    providers: arr(obj({ provider: S, region: S, billed_tokens: I, traced_tokens: I, billed_requests: nullable(I), traced_requests: nullable(I) })),
  }),
  lineage: arr(obj({ provider: S, model: S, role: S, region: S, share_of_calls_pct: nullable(N) })),
  changes: arr(obj({ date: S, kind: S, what: S })),
  incidents: arr(obj({ id: S, opened: S, acknowledged: S_OPT, resolved: S_OPT, summary: S })),
  objectives: arr(obj({ id: S, title: S, status: S, threshold: S, measured: S, basis: S, maps: arr(S) })),
  commitments: obj({
    trace_merkle_root: S,
    leaf_count: I,
    leaf: S,
    node: S,
    disclosed: nullable(obj({ trace_id: S, leaf_index: I, salt: S, proof: arr(obj({ side: S, hash: S })) })),
  }),
  limitations: arr(S),
  objective_set_hash: S_OPT,
  batch_digests: arr(S, true),
});
const STATEMENT = obj({ _type: S, subject: arr(obj({ name: S, digest: { map: S } })), predicateType: S, predicate: PREDICATE });

const isOptional = (spec) => spec === S_OPT || (typeof spec === "object" && spec.arr !== undefined && spec.optional);

function checkShape(v, spec, path) {
  const bad = (want) => {
    throw new PackError(`${path} must be ${want}`);
  };
  if (spec === S) return typeof v === "string" || bad("a string");
  if (spec === S_OPT) return (typeof v === "string" && v !== "") || bad("absent or a non-empty string");
  if (spec === I) return Number.isSafeInteger(v) || bad("an integer");
  if (spec === N) return typeof v === "number" || bad("a number");
  if (spec.nullable !== undefined) return v === null || checkShape(v, spec.nullable, path);
  if (spec.arr !== undefined) {
    if (!Array.isArray(v)) bad("an array");
    if (spec.optional && v.length === 0) bad("absent or a non-empty array");
    v.forEach((x, i) => checkShape(x, spec.arr, `${path}[${i}]`));
    return true;
  }
  if (!isObject(v)) bad("an object");
  if (spec.map !== undefined) {
    for (const k of Object.keys(v)) checkShape(v[k], spec.map, `${path}.${k}`);
    return true;
  }
  for (const k of Object.keys(v)) {
    if (!Object.hasOwn(spec.obj, k)) throw new PackError(`${path}.${k} is not a field of a v0.1 pack`);
  }
  for (const [k, s] of Object.entries(spec.obj)) {
    if (!Object.hasOwn(v, k)) {
      if (isOptional(s)) continue;
      throw new PackError(`${path}.${k} is missing`);
    }
    checkShape(v[k], s, `${path}.${k}`);
  }
  return true;
}

const HEX64 = /^[0-9a-f]{64}$/;
const HEX32 = /^[0-9a-f]{32}$/;
const DIGEST_REF = /^sha256:[0-9a-f]{64}$/;

// decodeStatement mirrors Go's pack.DecodeStatement: canonical I-JSON, the v0.1
// types exactly, our statement and predicate types, one sha256 subject, and
// well-formed digests, salt and proof steps.
export function decodeStatement(payload) {
  const st = parseCanonical(payload);
  checkShape(st, STATEMENT, "statement");
  const p = st.predicate;
  const fail = (msg) => {
    throw new PackError(`statement: ${msg}`);
  };
  if (st._type !== STATEMENT_TYPE) fail(`_type is ${JSON.stringify(st._type)}`);
  if (st.predicateType !== PREDICATE_TYPE) fail(`predicateType is ${JSON.stringify(st.predicateType)}`);
  if (st.subject.length !== 1) fail(`want exactly one subject, got ${st.subject.length}`);
  if (!HEX64.test(st.subject[0].digest.sha256 ?? "")) fail("subject digest must have a lowercase hex sha256");
  if (p.pack_schema !== PACK_SCHEMA) fail(`pack_schema is ${JSON.stringify(p.pack_schema)}`);
  if (p.prev_pack_digest !== null && !DIGEST_REF.test(p.prev_pack_digest)) fail("prev_pack_digest is malformed");
  if (!HEX64.test(p.commitments.trace_merkle_root)) fail("commitments.trace_merkle_root must be 64 lowercase hex");
  if (p.objective_set_hash !== undefined && !DIGEST_REF.test(p.objective_set_hash)) fail("objective_set_hash is malformed");
  for (const d of p.batch_digests ?? []) if (!DIGEST_REF.test(d)) fail("batch_digests holds a malformed digest");
  const d = p.commitments.disclosed;
  if (d !== null) {
    if (!HEX32.test(d.salt)) fail("commitments.disclosed.salt must be 32 lowercase hex");
    d.proof.forEach((s, i) => {
      if (s.side !== "L" && s.side !== "R") fail(`commitments.disclosed.proof[${i}].side must be L or R`);
      if (!HEX64.test(s.hash)) fail(`commitments.disclosed.proof[${i}].hash must be 64 lowercase hex`);
    });
  }
  return st;
}

// ---------- checks ----------

export async function verifyMerkle(c) {
  const d = c.disclosed;
  let h = await sha256(concat(new Uint8Array([0]), unhex(d.salt), enc.encode(d.trace_id)));
  for (const s of d.proof) {
    const x = unhex(s.hash);
    h = await sha256(concat(new Uint8Array([1]), s.side === "L" ? x : h, s.side === "L" ? h : x));
  }
  return hex(h) === c.trace_merkle_root;
}

export const packDigest = async (payload) => "sha256:" + hex(await sha256(payload));

// verifyEnvelope runs every check on a pack envelope (a parsed JSON object), each
// on its own as the Go verifier does. jwks is a JWKS object or the Map from
// importJWKS. prev is the previous pack's envelope, its digest ("sha256:<hex>",
// as the verify API returns it), or null to skip the chain check. Throws
// PackError (with .result) when the pack cannot be evaluated.
export async function verifyEnvelope(envObj, jwks, prev = null) {
  const keys = jwks instanceof Map ? jwks : await importJWKS(jwks);
  const env = decodeEnvelope(envObj);
  const r = {
    signature: false,
    subject_digest: false,
    chain: null,
    merkle: null,
    verified: false,
    pack_id: "",
    seq_no: 0,
    digest: await packDigest(env.payload),
    key_id: "",
    signature_error: "",
    key_status: "", // revoked | not_yet_valid | expired | bad_issued_at when the signature matched but its key may not vouch
    statement: null,
  };
  const sig = await checkSignature(env, keys);
  r.signature = sig.ok;
  r.key_id = sig.keyId ?? "";
  r.signature_error = sig.error ?? "";

  let st;
  try {
    st = decodeStatement(env.payload);
  } catch (e) {
    e.result = r;
    throw e;
  }
  const p = st.predicate;
  if (r.signature) {
    const why = keyVouches(keys, r.key_id, p.issued_at);
    if (why) {
      r.signature = false;
      r.signature_error = why.message;
      r.key_status = why.status;
    }
  }
  r.statement = st;
  r.pack_id = p.pack_id;
  r.seq_no = p.seq_no;
  r.subject_digest = hex(await sha256(enc.encode(jcs(p)))) === st.subject[0].digest.sha256;
  if (prev !== null && prev !== undefined) {
    let prevDigest = prev;
    if (typeof prev !== "string") prevDigest = await packDigest(decodeEnvelope(prev).payload);
    else if (!DIGEST_REF.test(prev)) throw new TypeError("prev digest must be sha256:<64 hex>");
    r.chain = p.prev_pack_digest === prevDigest;
  }
  if (p.commitments.disclosed !== null) r.merkle = await verifyMerkle(p.commitments);
  r.verified = r.signature && r.subject_digest && r.chain !== false && r.merkle !== false;
  return r;
}
