// Generates es-numbers.json: float64 bit patterns and the ECMAScript String(x)
// form that JCS (RFC 8785) requires. Deterministic; rerun to reproduce:
//   node internal/jcs/testdata/gen-es-numbers.mjs > internal/jcs/testdata/es-numbers.json
let seed = 0x9e3779b97f4a7c15n;
const next = () => { seed ^= seed << 13n; seed &= (1n << 64n) - 1n; seed ^= seed >> 7n; seed ^= seed << 17n; seed &= (1n << 64n) - 1n; return seed; };
const dv = new DataView(new ArrayBuffer(8));
const bits = (x) => { dv.setFloat64(0, x); return dv.getBigUint64(0).toString(16).padStart(16, "0"); };
const fromBits = (b) => { dv.setBigUint64(0, b); return dv.getFloat64(0); };

const xs = [0, -0, 1, -1, 0.1, 0.5, 1.5, 100, 1e6, 1e20, 1e21, 1e-6, 1e-7, 5e-324, -5e-324,
  1.7976931348623157e308, 2.2250738585072014e-308, 2 ** 53, 2 ** 53 + 2, -(2 ** 53), 0.30000000000000004,
  123456789012345680000, 4.35, 0.000001234, 1.234e-7, 9.999999999999999e20, 1e21 - 65536];
for (let e = -330; e <= 310; e++) { const p = Number(`1e${e}`); if (Number.isFinite(p)) xs.push(p, -p, p * 1.5); }
for (const p of [1e-7, 1e-6, 1e20, 1e21]) { const b = BigInt("0x" + bits(p)); xs.push(fromBits(b - 1n), fromBits(b + 1n)); }
for (let i = 0; i < 600; i++) { const x = fromBits(next()); if (Number.isFinite(x)) xs.push(x); }
for (let i = 0; i < 300; i++) xs.push(Number(next() % 10000000000n) / 100);
for (let i = 0; i < 300; i++) xs.push(Number(next() >> 11n));

const seen = new Set(); const out = [];
for (const x of xs) { const b = bits(x); if (!seen.has(b)) { seen.add(b); out.push([b, String(x)]); } }
process.stdout.write("[\n" + out.map((p) => JSON.stringify(p)).join(",\n") + "\n]\n");
