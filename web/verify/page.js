// Verify page: load a pack from a share link or from local files, run the checks
// in verify.js, and render. DOM is built with createElement and textContent only;
// pack contents are shown only when the issuer's signature verifies.
import { verifyEnvelope, importJWKS, decodeStatement, b64decode, PackError } from "./verify.js";

const $ = (id) => document.getElementById(id);

// el("td", { class: "num", title: "x" }, child, "text", ...)
function el(tag, attrs = {}, ...children) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") n.className = v;
    else n.setAttribute(k, v === true ? "" : String(v));
  }
  for (const c of children.flat()) {
    if (c === undefined || c === null || c === false) continue;
    n.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return n;
}
const clear = (n) => n.replaceChildren();

const fmtN = (n) => (typeof n === "number" ? n.toLocaleString("en-AU") : "—");
function fmtDate(iso, timeZone) {
  if (typeof iso !== "string") return "";
  const dateOnly = /^\d{4}-\d{2}-\d{2}$/.test(iso);
  const d = new Date(dateOnly ? iso + "T00:00:00Z" : iso);
  if (Number.isNaN(d.getTime())) return iso;
  try {
    return d.toLocaleDateString("en-AU", { day: "numeric", month: "short", year: "numeric", timeZone: dateOnly ? "UTC" : timeZone || "UTC" });
  } catch {
    return iso;
  }
}

// ---------- status words: never colour alone ----------

const STATUS = {
  pass: { mark: "✓", word: "Pass", cls: "pass" },
  fail: { mark: "✕", word: "Fail", cls: "fail" },
  skip: { mark: "–", word: "Not checked", cls: "skip" },
};
const OBJECTIVE_STATUS = {
  pass: { label: "✓ Pass", cls: "pill pass" },
  attention: { label: "! Attention", cls: "pill attention" },
  fail: { label: "✕ Fail", cls: "pill fail" },
};

function checkItem(state, text, detail) {
  const s = STATUS[state];
  return el("li", {}, el("span", { class: `mk ${s.cls}`, "aria-hidden": "true" }, s.mark),
    el("span", {}, el("span", { class: `word ${s.cls}` }, s.word), " ", text, detail ? el("small", {}, detail) : null));
}

function verdictTitle(r) {
  if (r.error) return "Failed: this is not a valid pack";
  if (r.verified) return "Verified, unaltered since signing";
  if (!r.subject_digest) return "Failed: this pack was changed after signing";
  if (r.key_status === "revoked") return "Failed: signed with a key the issuer has revoked";
  if (r.key_status) return "Failed: signed outside its key's validity period";
  if (!r.signature) return "Failed: the issuer's signature does not match this pack";
  if (r.chain === false) return "Failed: this pack does not follow the previous one";
  return "Failed: the disclosed trace is not in the commitment";
}

// ---------- state ----------

let state = { packs: [], current: 0, jwksText: "", mode: "offline", shareUrl: "" };

function showOnly(id) {
  for (const s of ["loading", "load-error", "offline", "report"]) $(s).hidden = s !== id;
}

function loadError(title, text) {
  $("load-error-title").textContent = title;
  $("load-error-text").textContent = text;
  showOnly("load-error");
}

// ---------- loading ----------

function tokenFromURL() {
  const q = new URLSearchParams(location.search).get("t");
  const h = new URLSearchParams(location.hash.replace(/^#/, "")).get("t");
  return q ?? h;
}

async function loadShareLink(token) {
  showOnly("loading");
  if (!/^[A-Za-z0-9_-]{16,64}$/.test(token)) return loadError("This link is not valid", "The share token in this address is malformed. Check that the whole link was copied.");
  const api = (document.querySelector('meta[name="plumbline-api"]')?.content || location.origin).replace(/\/+$/, "");
  const get = (path) => fetch(api + path, { credentials: "omit", cache: "no-store", referrerPolicy: "no-referrer" });
  let res;
  try {
    res = await get(`/v1/verify/${encodeURIComponent(token)}`);
  } catch {
    return loadError("The pack could not be loaded", "The Plumbline service did not respond. Try again, or ask the vendor for the pack file and check it offline.");
  }
  if (res.status === 410) return loadError("This link is no longer active", "The vendor revoked this link or it expired. Ask them for a new one.");
  if (res.status === 404) return loadError("No pack at this link", "Check that the whole link was copied.");
  if (!res.ok) return loadError("The pack could not be loaded", `The Plumbline service answered with HTTP ${res.status}.`);
  let body, jwks, jwksText;
  try {
    body = await res.json();
    const jr = await get("/.well-known/jwks.json");
    if (!jr.ok) throw new Error(`HTTP ${jr.status}`);
    jwksText = await jr.text();
    jwks = JSON.parse(jwksText);
  } catch (e) {
    return loadError("The pack could not be loaded", `The pack or the issuer's keys could not be read (${e.message}).`);
  }
  const prev = typeof body?.prev_envelope_digest === "string" ? body.prev_envelope_digest : null;
  state = { packs: [], current: 0, jwksText, mode: "online", shareUrl: location.href };
  let keys;
  try {
    keys = await importJWKS(jwks);
  } catch (e) {
    return loadError("The issuer's keys are not valid", e.message);
  }
  state.packs = [await check(body?.envelope, JSON.stringify(body?.envelope ?? null, null, 2), keys, prev, "share link")];
  renderReport();
}

async function loadFiles(event) {
  event.preventDefault();
  const jwksFile = $("jwks-file").files[0];
  const files = [...$("pack-files").files];
  if (!jwksFile || files.length === 0) return;
  let keys, jwksText;
  try {
    jwksText = await jwksFile.text();
    keys = await importJWKS(JSON.parse(jwksText));
  } catch (e) {
    return loadError("The issuer's keys are not valid", `${jwksFile.name}: ${e.message}`);
  }
  // Read every file, then order by the sequence number each claims (checked below).
  const items = await Promise.all(files.map(async (f) => {
    const text = await f.text();
    let env = null;
    let seq = Infinity;
    try {
      env = JSON.parse(text);
      seq = decodeStatement(b64decode(env.payload)).predicate.seq_no;
    } catch {
      // ordered last; the checks report what is wrong
    }
    return { name: f.name, text, env, seq };
  }));
  items.sort((a, b) => a.seq - b.seq);

  state = { packs: [], current: 0, jwksText, mode: "offline", shareUrl: "" };
  let prev = null;
  for (const [i, it] of items.entries()) {
    // A previous file that is not an envelope breaks the next link (D-017).
    const r = await check(it.env, it.text, keys, i === 0 ? null : prev ?? false, it.name);
    state.packs.push(r);
    prev = r.envelopeOK ? it.env : null;
  }
  state.current = state.packs.length - 1;
  renderReport();
}

// check verifies one envelope. prev: null (first pack), false (previous file was
// not an envelope), an envelope object, or a "sha256:" digest.
async function check(env, rawText, keys, prev, source) {
  const r = { source, rawText, envelopeOK: false };
  try {
    Object.assign(r, await verifyEnvelope(env, keys, prev === false ? null : prev));
    r.envelopeOK = true;
    if (prev === false) {
      r.chain = false;
      r.verified = false;
    }
  } catch (e) {
    if (!(e instanceof PackError)) throw e;
    Object.assign(r, e.result ?? {}, { error: e.message, verified: false });
    r.envelopeOK = e.result !== undefined;
  }
  return r;
}

// ---------- rendering ----------

function renderReport() {
  showOnly("report");
  renderChain();
  renderPack();
}

function renderChain() {
  const nav = $("chain");
  clear(nav);
  nav.hidden = state.packs.length < 2;
  state.packs.forEach((r, i) => {
    const s = r.verified ? STATUS.pass : STATUS.fail;
    const b = el("button", { type: "button", class: "link", "aria-current": String(i === state.current) },
      el("b", {}, r.pack_id || r.source),
      el("span", { class: `word ${s.cls}` }, `${s.mark} ${r.verified ? "Verified" : "Failed"}`),
      r.digest ? el("span", { class: "mono" }, r.digest) : null);
    b.addEventListener("click", () => {
      state.current = i;
      renderChain();
      renderPack();
    });
    nav.append(b);
  });
}

function renderPack() {
  const r = state.packs[state.current];
  const st = r.signature && !r.error ? r.statement : null;
  const p = st?.predicate;

  // seal
  $("seal").className = "seal " + (r.verified ? "ok" : "bad");
  $("stamp").textContent = r.verified ? "✓" : "✕";
  $("seal-title").textContent = verdictTitle(r);
  $("seal-sub").textContent = state.mode === "online" ? "Checked in your browser against the issuer's published keys." : `Checked in your browser from ${r.source}.`;
  const checks = $("checks");
  clear(checks);
  if (r.error) checks.append(checkItem("fail", "Not a valid pack", r.error));
  if (r.envelopeOK) {
    checks.append(r.signature ? checkItem("pass", "Signature is valid", `key ${r.key_id} · ES256`) : checkItem("fail", r.key_status ? "Signing key cannot vouch for this pack" : "Signature does not verify", r.signature_error));
    if (!r.error) {
      const subject = r.statement?.subject?.[0]?.digest?.sha256;
      checks.append(r.subject_digest ? checkItem("pass", "Contents match the signed digest", `sha256:${subject}`) : checkItem("fail", "Contents differ from the signed digest", `sha256:${subject}`));
      const prevDigest = r.statement?.predicate?.prev_pack_digest;
      if (r.chain === null) checks.append(checkItem("skip", "Link to the previous pack", state.mode === "online" ? "this is the first pack in its chain" : "no previous pack given"));
      else checks.append(r.chain ? checkItem("pass", "Links to the previous pack", prevDigest) : checkItem("fail", "Does not link to the previous pack", prevDigest ?? "no previous pack named"));
      const d = r.statement?.predicate?.commitments?.disclosed;
      if (r.merkle === null) checks.append(checkItem("skip", "Disclosed trace", "no trace disclosed in this pack"));
      else checks.append(checkItem(r.merkle ? "pass" : "fail", r.merkle ? "Disclosed trace is in the commitment" : "Disclosed trace is not in the commitment",
        `${d.trace_id} · leaf ${fmtN(d.leaf_index)} of ${fmtN(r.statement.predicate.commitments.leaf_count)}`));
    }
  }

  renderIdent(r, p);
  renderContent(p);
  $("raw-statement-box").hidden = !st;
  $("raw-statement").textContent = st ? JSON.stringify(st, null, 2) : "";
  $("raw-envelope").textContent = r.rawText;
  $("raw-jwks").textContent = state.jwksText;
  const packArgs = state.mode === "online" ? "pack.dsse.json" : state.packs.map((x) => x.source).join(" ");
  $("cmd").textContent = `plumbline-verify --jwks jwks.json ${packArgs}`;
  $("print-footer").textContent = [
    p ? `Pack ${p.pack_id} (#${p.seq_no})` : `File ${r.source}`,
    r.digest ? r.digest : null,
    state.mode === "online" ? state.shareUrl : `checked offline from ${r.source}`,
    `${r.verified ? "Verified" : "Failed"} ${new Date().toISOString()}`,
  ].filter(Boolean).join(" · ");
}

function renderIdent(r, p) {
  const box = $("ident");
  clear(box);
  if (!p) {
    box.append(el("p", { class: "label" }, "AI assurance pack"), el("h2", {}, r.pack_id || r.source),
      el("p", { class: "muted" }, "The contents are not shown because the issuer's signature does not verify, so they cannot be attributed to the issuer."),
      r.digest ? el("div", { class: "facts" }, el("div", {}, el("span", { class: "label" }, "Pack digest"), el("span", { class: "mono" }, r.digest))) : null);
    return;
  }
  box.append(
    el("p", { class: "label" }, "AI assurance pack · ", p.profiles.join(" + ")),
    el("h2", {}, p.subject.ai_system),
    el("p", {}, p.subject.organisation, " · ", el("span", { class: "muted" }, p.subject.environment)),
    p.subject.description ? el("p", { class: "muted" }, p.subject.description) : null,
    p.sample_notice ? el("p", { class: "notice sample-notice", role: "note" }, p.sample_notice) : null,
    el("div", { class: "facts" },
      el("div", {}, el("span", { class: "label" }, "Pack"), el("span", { class: "mono" }, `${p.pack_id} · #${p.seq_no}`)),
      el("div", {}, el("span", { class: "label" }, "Period"), el("span", {}, `${fmtDate(p.period.start)} – ${fmtDate(p.period.end)}`), el("span", { class: "muted small" }, p.period.timezone)),
      el("div", {}, el("span", { class: "label" }, "Issued"), el("span", {}, fmtDate(p.issued_at, p.period.timezone))),
      el("div", {}, el("span", { class: "label" }, "Assurance level"), el("span", {}, `Level ${p.assurance_level.level}`), el("span", { class: "muted small" }, p.assurance_level.meaning))),
    el("div", { class: "facts" }, el("div", {}, el("span", { class: "label" }, "Pack digest"), el("span", { class: "mono" }, r.digest))));
}

function section(title, intro, ...body) {
  return el("section", { class: "block" }, el("div", { class: "sec-head" }, el("h2", {}, title), intro ? el("p", { class: "muted" }, intro) : null), ...body);
}

function table(head, rows, attrs = {}) {
  return el("div", { class: "tbl" }, el("table", attrs,
    el("thead", {}, el("tr", {}, head.map(([h, cls]) => el("th", { class: cls }, h)))),
    el("tbody", {}, rows)));
}

function renderContent(p) {
  const box = $("content");
  clear(box);
  if (!p) return;

  const counts = { pass: 0, attention: 0, fail: 0 };
  for (const o of p.objectives) if (Object.hasOwn(counts, o.status)) counts[o.status]++;
  box.append(section("Summary", "Everything below is rendered from the signed pack.",
    el("div", { class: "strip" }, [
      ["Objectives passing", `${counts.pass} / ${p.objectives.length}`],
      ["Not passing", `${counts.attention} attention · ${counts.fail} fail`],
      ["Trace coverage", `${p.coverage.total_pct}%`],
      ["Traced calls", fmtN(p.commitments.leaf_count)],
    ].map(([l, v]) => el("div", { class: "stat" }, el("span", { class: "label" }, l), el("span", { class: "v" }, v))))));

  box.append(section("Evidence objectives", "Each objective is computed from telemetry, not self-reported. Framework references are IDs and short titles only; an asterisk marks one not yet confirmed against the source.",
    table([["Objective"], ["Measured this period"], ["Rule"], ["Status"], ["Maps to"]], p.objectives.map((o) => {
      const s = OBJECTIVE_STATUS[o.status] ?? { label: o.status, cls: "pill neutral" };
      return el("tr", {},
        el("td", {}, el("div", { class: "eo-id" }, o.id), el("div", { class: "eo-title" }, o.title), el("div", { class: "basis" }, o.basis)),
        el("td", {}, o.measured), el("td", { class: "muted" }, o.threshold), el("td", {}, el("span", { class: s.cls }, s.label)),
        el("td", {}, el("div", { class: "chips" }, o.maps.map((m) => el("span", { class: "chip" }, m)))));
    }), { class: "obj" })));

  box.append(section("Trace coverage", "Provider-billed usage reconciled against traced usage: this shows the logs are complete enough to rely on, not just unedited.",
    table([["Provider"], ["Region"], ["Billed tokens", "num"], ["Traced tokens", "num"], ["Traced ÷ billed", "num"], ["Billed requests", "num"], ["Traced requests", "num"]],
      p.coverage.providers.map((x) => el("tr", {}, el("td", {}, x.provider), el("td", { class: "mono" }, x.region), el("td", { class: "num" }, fmtN(x.billed_tokens)),
        el("td", { class: "num" }, fmtN(x.traced_tokens)), el("td", { class: "num" }, x.billed_tokens > 0 ? `${(Math.floor((1000 * x.traced_tokens) / x.billed_tokens) / 10).toFixed(1)}%` : "—"),
        el("td", { class: "num" }, fmtN(x.billed_requests)), el("td", { class: "num" }, fmtN(x.traced_requests)))))));

  const events = p.changes.map((c) => ({ d: c.date, node: el("span", {}, el("span", { class: "kind" }, c.kind), c.what) }))
    .concat(p.incidents.map((x) => ({
      d: x.opened.slice(0, 10),
      node: el("span", {}, el("span", { class: "kind incident" }, `incident ${x.id}`), x.summary,
        x.acknowledged ? el("span", { class: "muted small" }, ` · acknowledged ${fmtDate(x.acknowledged, p.period.timezone)}`) : null,
        x.resolved ? el("span", { class: "muted small" }, ` · resolved ${fmtDate(x.resolved, p.period.timezone)}`) : null),
    })))
    .sort((a, b) => a.d.localeCompare(b.d));
  box.append(el("section", { class: "block" }, el("div", { class: "two" },
    el("div", {}, el("h2", {}, "Models in use"),
      table([["Model"], ["Role"], ["Region"], ["Calls", "num"]], p.lineage.map((m) => el("tr", {}, el("td", { class: "mono" }, m.model), el("td", {}, m.role),
        el("td", { class: "mono" }, m.region), el("td", { class: "num" }, m.share_of_calls_pct == null ? "—" : `${m.share_of_calls_pct}%`)))),
      el("p", { class: "label" }, "Sources"),
      el("ul", { class: "log" }, p.sources.map((s) => el("li", {}, el("span", { class: "kind" }, s.kind.replace(/_/g, " ")), el("span", {}, s.system, el("br"), el("span", { class: "muted small" }, s.method)))))),
    el("div", {}, el("h2", {}, "Changes and incidents"),
      el("ul", { class: "log" }, events.length ? events.map((e) => el("li", {}, el("time", { datetime: e.d }, fmtDate(e.d)), e.node)) : el("li", {}, el("span", { class: "muted" }, "No changes or incidents this period.")))))));

  const c = p.commitments;
  const kv = [["Merkle root", c.trace_merkle_root], ["Leaves", `${fmtN(c.leaf_count)} traced calls`], ["Leaf", c.leaf], ["Node", c.node]];
  if (c.disclosed) kv.push(["Disclosed trace", `${c.disclosed.trace_id} (leaf ${fmtN(c.disclosed.leaf_index)})`], ["Proof", `${c.disclosed.proof.length} sibling hashes`]);
  else kv.push(["Disclosed trace", "none in this pack"]);
  box.append(section("Trace commitment", "Every traced call is committed to a salted Merkle root. The vendor can reveal any single trace to an auditor without exposing the rest.",
    el("dl", { class: "kv" }, kv.map(([k, v]) => [el("dt", {}, k), el("dd", {}, v)]))));

  box.append(section("What this pack does not prove", null, el("ul", { class: "limits" }, p.limitations.map((l) => el("li", {}, l)))));
}

// ---------- start ----------

function init() {
  $("offline-form").addEventListener("submit", (e) => loadFiles(e).catch((err) => loadError("The packs could not be checked", err.message)));
  $("btn-print").addEventListener("click", () => window.print());
  // Print in the light theme regardless of the viewer's setting.
  let theme;
  addEventListener("beforeprint", () => {
    theme = document.documentElement.dataset.theme;
    document.documentElement.dataset.theme = "light";
  });
  addEventListener("afterprint", () => {
    if (theme === undefined) delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = theme;
  });
  if (!(globalThis.crypto && crypto.subtle)) {
    return loadError("This browser cannot run the checks", "Web Crypto is unavailable here (it needs HTTPS). Use plumbline-verify instead.");
  }
  const token = tokenFromURL();
  if (token) loadShareLink(token).catch((err) => loadError("The pack could not be checked", err.message));
  else showOnly("offline");
}

init();
