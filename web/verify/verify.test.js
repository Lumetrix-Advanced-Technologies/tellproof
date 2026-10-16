// node --test web/verify/verify.test.js   (no packages)
// The browser verifier must return exactly what manifest.json and
// extra-manifest.json expect, the same results as internal/pack in Go.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { jcs, pae, derToRaw, verifyEnvelope, verifyMerkle, parseCanonical, importJWKS, decodeEnvelope, b64decode, PackError, issuedAtSeconds } from "./verify.js";

const VEC = join(dirname(fileURLToPath(import.meta.url)), "..", "..", "spec", "vectors");
const load = (f) => JSON.parse(readFileSync(join(VEC, f), "utf8"));
const enc = new TextEncoder();
const manifest = load("manifest.json");
const extra = load("extra-manifest.json");
const jwks = load(manifest.jwks);
const checks = (r) => ({ signature: r.signature, subject_digest: r.subject_digest, chain: r.chain, merkle: r.merkle, verified: r.verified });

for (const c of manifest.cases) {
  test(`vector ${c.name}`, async () => {
    const r = await verifyEnvelope(load(c.file), jwks, c.prev ? load(c.prev) : null);
    assert.deepEqual(checks(r), c.expect);
  });
}

for (const c of extra.cases) {
  test(`extra vector ${c.name}`, async () => {
    const run = () => verifyEnvelope(load(c.file), jwks, c.prev ? load(c.prev) : null);
    if (c.expect === "error") {
      await assert.rejects(run, (e) => e instanceof PackError && e.result?.verified !== true);
      return;
    }
    assert.deepEqual(checks(await run()), c.expect);
  });
}

test("tampered-payload and wrong-key differ only by subject digest", async () => {
  const t = await verifyEnvelope(load("fail/tampered-payload.dsse.json"), jwks);
  const w = await verifyEnvelope(load("fail/wrong-key.dsse.json"), jwks);
  assert.equal(t.signature, false);
  assert.equal(w.signature, false);
  assert.equal(t.subject_digest, false);
  assert.equal(w.subject_digest, true);
  assert.match(w.signature_error, /does not verify/);
});

test("chain against a previous digest, as the verify API supplies it", async () => {
  const p1 = load("pass/KST-CTA-0001.dsse.json");
  const p2 = load("pass/KST-CTA-0002.dsse.json");
  const r1 = await verifyEnvelope(p1, jwks);
  assert.equal(r1.digest, "sha256:4e9e3cd383c7d979e54864e1e2eef2a58c6590e20ce706b38f3dd8b7b8ecabb6");
  assert.equal((await verifyEnvelope(p2, jwks, r1.digest)).chain, true);
  assert.equal((await verifyEnvelope(p2, jwks, "sha256:" + "0".repeat(64))).chain, false);
  await assert.rejects(() => verifyEnvelope(p2, jwks, "not-a-digest"), TypeError);
});

test("verified results carry the decoded statement and key", async () => {
  const r = await verifyEnvelope(load("pass/KST-CTA-0003.dsse.json"), jwks);
  assert.equal(r.pack_id, "KST-CTA-0003");
  assert.equal(r.seq_no, 3);
  assert.equal(r.key_id, "test-p256-2026-10");
  assert.equal(r.statement.predicate.pack_id, "KST-CTA-0003");
});

const jcsVectors = load("../jcs-vectors.json");
for (const v of jcsVectors.cases) {
  test(`jcs ${v.name}`, () => assert.equal(jcs(JSON.parse(v.input)), v.expected));
}
for (const v of jcsVectors.must_reject) {
  test(`jcs rejects ${v.name}`, () => assert.throws(() => parseCanonical(enc.encode(v.input)), PackError));
}

test("pass payloads are canonical", () => {
  for (const c of manifest.cases.filter((x) => x.name.startsWith("pass"))) {
    assert.doesNotThrow(() => parseCanonical(b64decode(load(c.file).payload)));
  }
});

test("parseCanonical rejects what Go rejects", () => {
  for (const bad of [
    [0xef, 0xbb, 0xbf, 0x7b, 0x7d], // BOM then {}
    [0xff],
    [...enc.encode('{"a":1,"a":1}')],
    [...enc.encode('{"b":1,"a":2}')],
    [...enc.encode('[1.0]')],
    [...enc.encode('[-0]')],
    [...enc.encode('[1e400]')],
    [...enc.encode('["\\ud83d"]')],
  ]) {
    assert.throws(() => parseCanonical(Uint8Array.from(bad)), PackError, String(bad));
  }
});

test("PAE matches the DSSE specification", () => {
  assert.equal(new TextDecoder().decode(pae("http://example.com/HelloWorld", enc.encode("hello world"))), "DSSEv1 29 http://example.com/HelloWorld 11 hello world");
  assert.equal(new TextDecoder().decode(pae("t", enc.encode("é"))), "DSSEv1 1 t 2 é");
});

test("derToRaw converts the vectors' DER signatures", () => {
  for (const c of manifest.cases) {
    const raw = derToRaw(b64decode(load(c.file).signatures[0].sig));
    assert.equal(raw.length, 64);
  }
});

test("derToRaw is as strict as Go", () => {
  const sig = b64decode(load("pass/KST-CTA-0001.dsse.json").signatures[0].sig);
  const rLen = sig[3];
  const sOff = 4 + rLen;
  const cases = {
    "long-form length": Uint8Array.from([0x30, 0x81, sig[1], ...sig.subarray(2)]),
    "trailing byte": Uint8Array.from([0x30, sig[1] + 1, ...sig.subarray(2), 0x00]),
    "wrong outer tag": Uint8Array.from([0x31, ...sig.subarray(1)]),
    "wrong integer tag": Uint8Array.from([...sig.subarray(0, 2), 0x03, ...sig.subarray(3)]),
    "non-minimal integer": Uint8Array.from([0x30, sig[1] + 1, 0x02, rLen + 1, 0x00, ...sig.subarray(4)]),
    "negative integer": (() => {
      const b = Uint8Array.from(sig);
      b[sOff + 2] |= 0x80;
      if (b[sOff + 1] === 33) b[sOff + 2] = 0x80; // the 0x00 pad becomes a negative first byte
      return b;
    })(),
    "too short": sig.subarray(0, 6),
  };
  for (const [name, der] of Object.entries(cases)) assert.throws(() => derToRaw(der), PackError, name);
});

test("importJWKS is as strict as Go", async () => {
  const k = jwks.keys[0];
  const base = { kty: k.kty, crv: k.crv, kid: k.kid, x: k.x, y: k.y };
  assert.equal((await importJWKS({ keys: [base] })).size, 1);
  const bad = {
    "no keys": { keys: [] },
    "unknown top-level member": { keys: [base], expires: 1 },
    "unknown key member": { keys: [{ ...base, iat: 1 }] },
    "nbf as a string": { keys: [{ ...base, nbf: "1" }] },
    "negative exp": { keys: [{ ...base, exp: -1 }] },
    "exp not after nbf": { keys: [{ ...base, nbf: 5, exp: 5 }] },
    "null nbf": { keys: [{ ...base, nbf: null }] },
    "revocation without kid": { keys: [base], revoked: [{ revoked_at: 1 }] },
    "kid revoked twice": { keys: [base], revoked: [{ kid: base.kid, revoked_at: 1 }, { kid: base.kid, revoked_at: 2 }] },
    "missing kid": { keys: [{ ...base, kid: undefined }] },
    "duplicate kid": { keys: [base, base] },
    RSA: { keys: [{ ...base, kty: "RSA" }] },
    "P-384": { keys: [{ ...base, crv: "P-384" }] },
    "alg ES384": { keys: [{ ...base, alg: "ES384" }] },
    "use enc": { keys: [{ ...base, use: "enc" }] },
    "padded x": { keys: [{ ...base, x: base.x + "=" }] },
    "short x": { keys: [{ ...base, x: base.x.slice(0, 40) }] },
    "point not on the curve": { keys: [{ ...base, y: "A".repeat(43) }] },
  };
  for (const [name, set] of Object.entries(bad)) {
    const clean = JSON.parse(JSON.stringify(set)); // drop undefined members
    await assert.rejects(() => importJWKS(clean), PackError, name);
  }
});

test("decodeEnvelope is as strict as Go", () => {
  const ok = { payloadType: "application/vnd.in-toto+json", payload: "e30=", signatures: [{ keyid: "k", sig: "AAAA" }] };
  assert.doesNotThrow(() => decodeEnvelope(ok));
  const bad = {
    "unknown field": { ...ok, extra: 1 },
    "unknown signature field": { ...ok, signatures: [{ keyid: "k", sig: "AAAA", cert: "x" }] },
    "missing payloadType": { payload: ok.payload, signatures: ok.signatures },
    "empty payload": { ...ok, payload: "" },
    "URL-safe base64": { ...ok, payload: "-_8=" },
    "unpadded base64": { ...ok, payload: "e30" },
    "no signatures": { ...ok, signatures: [] },
    "empty sig": { ...ok, signatures: [{ keyid: "k", sig: "" }] },
    "not an object": [],
  };
  for (const [name, env] of Object.entries(bad)) assert.throws(() => decodeEnvelope(env), PackError, name);
});

test("verifyMerkle on a vector's disclosed trace", async () => {
  const st = JSON.parse(new TextDecoder().decode(b64decode(load("pass/KST-CTA-0002.dsse.json").payload)));
  assert.equal(await verifyMerkle(st.predicate.commitments), true);
  st.predicate.commitments.disclosed.trace_id += "x";
  assert.equal(await verifyMerkle(st.predicate.commitments), false);
});

// spec/vectors/key-manifest.json (D-042): which JWKS keys may vouch for a pack.
const keyCases = load("key-manifest.json").cases;
for (const c of keyCases) {
  test(`key vector ${c.name}`, async () => {
    if (c.expect === "jwks_error") {
      await assert.rejects(importJWKS(load(c.jwks)), PackError);
      return;
    }
    const r = await verifyEnvelope(load(c.file), load(c.jwks), null);
    assert.deepEqual(checks(r), c.expect, r.signature_error);
    assert.equal(r.key_status !== "", c.expect.signature === false, "key_status is set exactly when a matching signature's key may not vouch");
  });
}

test("issued_at is parsed strictly, as Go parses it", () => {
  assert.equal(issuedAtSeconds("1970-01-01T00:01:40Z"), 100);
  assert.equal(issuedAtSeconds("1970-01-01T10:01:40+10:00"), 100);
  assert.equal(issuedAtSeconds("1970-01-01T00:01:40.999Z"), 100); // whole seconds, as Go's Unix()
  for (const bad of ["1970-01-01 00:01:40Z", "1970-02-30T00:00:00Z", "1970-01-01T24:00:00Z", "1970-01-01T00:00:60Z", "1970-01-01T00:00:00+24:00", "1970-01-01T00:00:00"]) {
    assert.equal(issuedAtSeconds(bad), null, bad);
  }
});
