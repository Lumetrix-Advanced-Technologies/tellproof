// The local dashboard (D-053). Reads /api/log (the verified local evidence
// log), filters it to the chosen range and draws it. Plain DOM and SVG: every
// string from the log goes in through textContent, never parsed as HTML.

const SVG = "http://www.w3.org/2000/svg";
const SLOTS = 6; // validated categorical slots; the rest fold into "Other"
const REFRESH_MS = 30_000;
const RANGES = { "24h": 24 * 3600e3, "7d": 7 * 86400e3, "30d": 30 * 86400e3, all: Infinity };

const $ = (id) => document.getElementById(id);
const fmt = new Intl.NumberFormat();
const compact = new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 });
const pct = (n, d) => (d > 0 ? (100 * n) / d : null);
const pctText = (v) => (v == null ? "n/a" : `${v >= 99.95 || v === 0 ? v.toFixed(0) : v.toFixed(1)}%`);

function el(tag, attrs = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k === "style") e.style.cssText = v;
    else e.setAttribute(k, v);
  }
  for (const k of kids) if (k != null) e.append(k instanceof Node ? k : String(k));
  return e;
}
function sv(tag, attrs = {}) {
  const e = document.createElementNS(SVG, tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  return e;
}

// A link can pick the range: #range=7d.
const linked = /range=(24h|7d|30d|all)/.exec(location.hash);
let state = { range: linked ? linked[1] : load("range") || "24h", data: null, firstLoad: !linked };
function load(k) { try { return localStorage.getItem("plumbline." + k); } catch { return null; } }
function save(k, v) { try { localStorage.setItem("plumbline." + k, v); } catch { /* private window */ } }

// ---- time buckets -------------------------------------------------------

const HOUR = 3600e3, DAY = 86400e3;
function bucketKey(t, daily) {
  const d = new Date(t);
  if (daily) { d.setHours(0, 0, 0, 0); return d.getTime(); }
  return Math.floor(t / HOUR) * HOUR;
}
function nextBucket(t, daily) {
  if (!daily) return t + HOUR;
  const d = new Date(t); d.setDate(d.getDate() + 1); return d.getTime();
}
function labelFor(t, daily, withDate) {
  const d = new Date(t);
  if (daily) return d.toLocaleDateString(undefined, { day: "numeric", month: "short" });
  const hm = d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  return withDate ? `${d.toLocaleDateString(undefined, { day: "numeric", month: "short" })} ${hm}` : hm;
}
function longLabel(t, daily) {
  const d = new Date(t);
  if (daily) return d.toLocaleDateString(undefined, { weekday: "short", day: "numeric", month: "short", year: "numeric" });
  const end = new Date(t + HOUR);
  return `${d.toLocaleDateString(undefined, { weekday: "short", day: "numeric", month: "short" })}, ${d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" })}–${end.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" })}`;
}

// ---- model identity: colour follows the entity, fixed by whole-log rank --

function modelKey(m) { return `${m.provider} · ${m.model}`; }
function assignSlots(log) {
  const totals = new Map();
  for (const p of log.series) for (const m of p.models) totals.set(modelKey(m), (totals.get(modelKey(m)) || 0) + m.calls);
  const order = [...totals.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).map(([k]) => k);
  const slot = new Map();
  order.forEach((k, i) => slot.set(k, i < SLOTS ? `var(--series-${i + 1})` : "var(--series-other)"));
  const named = order.slice(0, SLOTS);
  return { slot, named, hasOther: order.length > SLOTS };
}

// ---- data shaping ---------------------------------------------------------

function shape(log, rangeKey, now) {
  const span = RANGES[rangeKey];
  const points = log.series.map((p) => ({ ...p, t: Date.parse(p.start) }));
  const lastT = points.length ? points[points.length - 1].t : now;
  const from = span === Infinity ? (points.length ? points[0].t : now - DAY) : now - span;
  const to = span === Infinity ? lastT + HOUR : now;
  const daily = (to - from) > 7 * DAY + HOUR;
  const inRange = points.filter((p) => p.t >= from && p.t < to);
  const buckets = new Map();
  for (let t = bucketKey(from, daily); t < to; t = nextBucket(t, daily)) buckets.set(t, emptyBucket(t));
  for (const p of inRange) {
    const k = bucketKey(p.t, daily);
    if (!buckets.has(k)) buckets.set(k, emptyBucket(k));
    const b = buckets.get(k);
    for (const f of ["llm_calls", "guarded_calls", "blocked_calls", "pii_spans", "pii_spans_redacted", "content_present_spans", "tool_calls", "tool_errors", "traces_committed"]) b[f] += p[f];
    for (const m of p.models) {
      const key = modelKey(m);
      b.calls.set(key, (b.calls.get(key) || 0) + m.calls);
      b.input += m.input_tokens; b.output += m.output_tokens;
    }
  }
  const list = [...buckets.values()].sort((a, b) => a.t - b.t);
  // Range totals and the per-model table.
  const tot = emptyBucket(0);
  const models = new Map();
  for (const p of inRange) {
    for (const f of ["llm_calls", "guarded_calls", "blocked_calls", "pii_spans", "pii_spans_redacted", "content_present_spans", "tool_calls", "tool_errors", "traces_committed"]) tot[f] += p[f];
    for (const m of p.models) {
      const k = `${m.provider}\u0000${m.model}\u0000${m.region}`;
      const row = models.get(k) || { provider: m.provider, model: m.model, region: m.region, calls: 0, input: 0, output: 0 };
      row.calls += m.calls; row.input += m.input_tokens; row.output += m.output_tokens;
      models.set(k, row);
      tot.input += m.input_tokens; tot.output += m.output_tokens;
    }
  }
  const inT = (s) => { const t = Date.parse(s); return t >= from && t < to; };
  return {
    daily, from, to, buckets: list, tot, any: inRange.length > 0,
    models: [...models.values()].sort((a, b) => b.calls - a.calls),
    transitions: log.transitions.filter((x) => inT(x.at)),
    anomalies: log.anomalies.filter((x) => inT(x.start)),
    evals: log.evals.filter((x) => inT(x.ran_at)),
  };
}
function emptyBucket(t) {
  return { t, calls: new Map(), input: 0, output: 0, llm_calls: 0, guarded_calls: 0, blocked_calls: 0, pii_spans: 0, pii_spans_redacted: 0,
    content_present_spans: 0, tool_calls: 0, tool_errors: 0, traces_committed: 0 };
}

// ---- axes -----------------------------------------------------------------

// The top of a 4-tick axis whose ticks are clean steps (1, 2, 2.5 or 5 × 10^n).
function niceMax(v, ticks = 4) {
  if (v <= 0) return ticks;
  const raw = v / ticks;
  const exp = Math.pow(10, Math.floor(Math.log10(raw)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * exp >= raw) return m * exp * ticks;
  return 10 * exp * ticks;
}
function frame(width, height) {
  return { w: width, h: height, l: 48, r: 12, t: 10, b: 26 };
}
function drawYAxis(svg, f, max, ticks, format) {
  for (let i = 0; i <= ticks; i++) {
    const v = (max * i) / ticks;
    const y = f.t + (f.h - f.t - f.b) * (1 - i / ticks);
    svg.append(sv("line", { class: "grid-line", x1: f.l, x2: f.w - f.r, y1: y, y2: y }));
    const tx = sv("text", { class: "axis-text", x: f.l - 8, y: y + 4, "text-anchor": "end" });
    tx.textContent = format(v);
    svg.append(tx);
  }
}
// Ticks on round local times: over a day, every few hours (the date at
// midnight); over several days, at midnights labelled with the date.
function drawXAxis(svg, f, buckets, x, daily) {
  const n = buckets.length;
  if (!n) return;
  const want = Math.max(2, Math.floor((f.w - f.l - f.r) / 84));
  const hourOf = (t) => new Date(t).getHours();
  const dateLabel = (t) => new Date(t).toLocaleDateString(undefined, { day: "numeric", month: "short" });
  let picks = [];
  if (daily) {
    const k = Math.max(1, Math.ceil(n / want));
    for (let i = 0; i < n; i += k) picks.push([i, dateLabel(buckets[i].t)]);
  } else if (n <= 48) {
    const k = [1, 2, 3, 4, 6, 12].find((h) => n / h <= want) || 12;
    buckets.forEach((b, i) => {
      if (hourOf(b.t) % k === 0) picks.push([i, hourOf(b.t) === 0 ? dateLabel(b.t) : labelFor(b.t, false, false)]);
    });
  } else {
    const days = buckets.map((b, i) => [i, b]).filter(([, b]) => hourOf(b.t) === 0);
    const k = Math.max(1, Math.ceil(days.length / want));
    picks = days.filter((_, j) => j % k === 0).map(([i, b]) => [i, dateLabel(b.t)]);
  }
  for (const [i, text] of picks) {
    svg.append(sv("line", { class: "grid-line", x1: x(i), x2: x(i), y1: f.h - f.b, y2: f.h - f.b + 4 }));
    const tx = sv("text", { class: "axis-text", x: x(i), y: f.h - 6, "text-anchor": "middle" });
    tx.textContent = text;
    svg.append(tx);
  }
}

// ---- tooltip --------------------------------------------------------------

const tip = () => $("tooltip");
function showTip(clientX, clientY, title, rows) {
  const t = tip();
  t.replaceChildren(el("div", { class: "t" }, title));
  for (const r of rows) {
    t.append(el("div", { class: "row" }, el("i", { class: "k", style: `background:${r.color}` }), el("b", {}, r.value), el("span", {}, r.label)));
  }
  t.hidden = false;
  const pad = 14, w = t.offsetWidth, h = t.offsetHeight;
  let x = clientX + pad, y = clientY + pad;
  if (x + w > innerWidth - 8) x = clientX - w - pad;
  if (y + h > innerHeight - 8) y = clientY - h - pad;
  t.style.left = `${Math.max(8, x)}px`; t.style.top = `${Math.max(8, y)}px`;
}
function hideTip() { tip().hidden = true; }

// Keyboard: a focused chart moves its readout with the arrow keys.
function keyNav(box, n, show) {
  let i = -1;
  box.tabIndex = 0;
  box.onkeydown = (e) => {
    if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
    e.preventDefault();
    i = i < 0 ? n - 1 : Math.max(0, Math.min(n - 1, i + (e.key === "ArrowRight" ? 1 : -1)));
    show(i, true);
  };
  box.onblur = () => { i = -1; hideTip(); };
}

// ---- stacked columns: calls per bucket by model ---------------------------

function drawCalls(box, sh, ids) {
  box.replaceChildren();
  const width = Math.max(320, box.clientWidth), height = 250;
  const f = frame(width, height);
  const svg = sv("svg", { viewBox: `0 0 ${width} ${height}`, role: "img", "aria-label": "Model calls per time bucket, stacked by model" });
  const n = sh.buckets.length;
  const stackKeys = [...ids.named, ...(ids.hasOther ? ["Other"] : [])];
  const valueOf = (b, k) => (k === "Other" ? [...b.calls.entries()].filter(([m]) => !ids.named.includes(m)).reduce((s, [, v]) => s + v, 0) : b.calls.get(k) || 0);
  const totals = sh.buckets.map((b) => [...b.calls.values()].reduce((s, v) => s + v, 0));
  const max = niceMax(Math.max(...totals, 0));
  drawYAxis(svg, f, max, 4, (v) => compact.format(v));
  const band = (f.w - f.l - f.r) / Math.max(1, n);
  const bw = Math.max(2, Math.min(24, band * 0.68));
  const x = (i) => f.l + band * i + band / 2;
  const y = (v) => f.t + (f.h - f.t - f.b) * (1 - v / max);
  const colour = (k) => (k === "Other" ? "var(--series-other)" : ids.slot.get(k));
  const cols = [];
  sh.buckets.forEach((b, i) => {
    const g = sv("g", { class: "bar" });
    let acc = 0;
    const segs = stackKeys.map((k) => [k, valueOf(b, k)]).filter(([, v]) => v > 0);
    segs.forEach(([k, v], si) => {
      const y0 = y(acc), y1 = y(acc + v);
      acc += v;
      const top = si === segs.length - 1;
      const hgt = Math.max(0, y0 - y1 - (si > 0 ? 2 : 0)); // 2px surface gap between stacked segments
      const yy = y1;
      if (hgt <= 0) return;
      const r = top ? Math.min(4, bw / 2, hgt) : 0;
      const x0 = x(i) - bw / 2;
      const d = `M${x0},${yy + hgt} V${yy + r} Q${x0},${yy} ${x0 + r},${yy} H${x0 + bw - r} Q${x0 + bw},${yy} ${x0 + bw},${yy + r} V${yy + hgt} Z`;
      g.append(sv("path", { d, fill: colour(k) }));
    });
    svg.append(g);
    cols.push(g);
  });
  drawXAxis(svg, f, sh.buckets, x, sh.daily);
  const show = (i, fromKey, ev) => {
    cols.forEach((c, j) => c.style.opacity = j === i ? "1" : "0.55");
    const b = sh.buckets[i];
    const rows = stackKeys.map((k) => ({ k, v: valueOf(b, k) })).filter((r) => r.v > 0).sort((a, b2) => b2.v - a.v)
      .map((r) => ({ color: colour(r.k), value: fmt.format(r.v), label: r.k }));
    rows.unshift({ color: "transparent", value: fmt.format(totals[i]), label: "model calls in total" });
    const rect = box.getBoundingClientRect();
    const cx = fromKey ? rect.left + (x(i) / width) * rect.width : ev.clientX;
    const cy = fromKey ? rect.top + 20 : ev.clientY;
    showTip(cx, cy, longLabel(b.t, sh.daily), rows);
  };
  const reset = () => { cols.forEach((c) => c.style.opacity = "1"); hideTip(); };
  sh.buckets.forEach((b, i) => {
    const hit = sv("rect", { class: "hit", x: f.l + band * i, y: f.t, width: band, height: f.h - f.t - f.b });
    hit.addEventListener("pointermove", (ev) => show(i, false, ev));
    hit.addEventListener("pointerleave", reset);
    svg.append(hit);
  });
  if (!sh.any) {
    const t = sv("text", { class: "nodata", x: width / 2, y: height / 2, "text-anchor": "middle" });
    t.textContent = "No model calls in this range";
    svg.append(t);
  }
  box.append(svg);
  keyNav(box, n, (i) => show(i, true));
  // Legend: always present for two or more series; rect keys mirror the bars.
  const legend = $("calls-legend");
  // Only the models with calls in this range; their colours never change with the range.
  const present = stackKeys.filter((k) => sh.buckets.some((b) => valueOf(b, k) > 0));
  legend.replaceChildren(...present.map((k) => el("li", {}, el("i", { class: "key", style: `background:${colour(k)}` }), k)));
  legend.hidden = present.length < 2;
}

// ---- lines: two series, one axis -----------------------------------------

function drawLines(box, sh, series, { max, format, label, legendId }) {
  box.replaceChildren();
  const width = Math.max(300, box.clientWidth), height = 210;
  const f = frame(width, height);
  const svg = sv("svg", { viewBox: `0 0 ${width} ${height}`, role: "img", "aria-label": label });
  const n = sh.buckets.length;
  const values = series.map((s) => sh.buckets.map(s.value));
  const top = max ?? niceMax(Math.max(0, ...values.flat().filter((v) => v != null)));
  drawYAxis(svg, f, top, 4, format);
  const step = (f.w - f.l - f.r) / Math.max(1, n - 1);
  const x = (i) => (n === 1 ? (f.l + f.w - f.r) / 2 : f.l + step * i);
  const y = (v) => f.t + (f.h - f.t - f.b) * (1 - v / top);
  series.forEach((s, si) => {
    let d = "", pen = false, last = -1;
    values[si].forEach((v, i) => {
      if (v == null) { pen = false; return; }
      d += `${pen ? "L" : "M"}${x(i).toFixed(1)},${y(v).toFixed(1)} `;
      pen = true; last = i;
    });
    if (d) svg.append(sv("path", { class: "series-line", d, stroke: s.color }));
    // Lone points (gaps on both sides) and the latest point get a dot with a surface ring.
    values[si].forEach((v, i) => {
      const lone = v != null && values[si][i - 1] == null && values[si][i + 1] == null;
      if (v != null && (lone || i === last)) svg.append(sv("circle", { class: "dot", cx: x(i), cy: y(v), r: 4, fill: s.color }));
    });
  });
  drawXAxis(svg, f, sh.buckets, x, sh.daily);
  const cross = sv("line", { class: "crosshair", y1: f.t, y2: f.h - f.b, visibility: "hidden" });
  svg.append(cross);
  const show = (i, fromKey, ev) => {
    cross.setAttribute("x1", x(i)); cross.setAttribute("x2", x(i)); cross.setAttribute("visibility", "visible");
    const rows = series.map((s, si) => ({ color: s.color, value: values[si][i] == null ? "n/a" : s.text(values[si][i], sh.buckets[i]), label: s.name }));
    const rect = box.getBoundingClientRect();
    showTip(fromKey ? rect.left + (x(i) / width) * rect.width : ev.clientX, fromKey ? rect.top + 20 : ev.clientY, longLabel(sh.buckets[i].t, sh.daily), rows);
  };
  const hit = sv("rect", { class: "hit", x: f.l, y: f.t, width: f.w - f.l - f.r, height: f.h - f.t - f.b });
  hit.addEventListener("pointermove", (ev) => {
    const r = svg.getBoundingClientRect();
    const px = ((ev.clientX - r.left) / r.width) * width;
    show(Math.max(0, Math.min(n - 1, Math.round(n === 1 ? 0 : (px - f.l) / step))), false, ev);
  });
  hit.addEventListener("pointerleave", () => { cross.setAttribute("visibility", "hidden"); hideTip(); });
  svg.append(hit);
  box.append(svg);
  keyNav(box, n, (i) => show(i, true));
  $(legendId).replaceChildren(...series.map((s) => el("li", {}, el("i", { class: "key line", style: `background:${s.color}` }), s.name)));
}

// ---- tables ---------------------------------------------------------------

function table(head, rows, numeric = []) {
  const t = el("table");
  t.append(el("thead", {}, el("tr", {}, ...head.map((h, i) => el("th", numeric.includes(i) ? { class: "n" } : {}, h)))));
  const body = el("tbody");
  for (const r of rows) body.append(el("tr", {}, ...r.map((c, i) => (c instanceof Node && c.tagName === "TD" ? c : el("td", numeric.includes(i) ? { class: "n" } : {}, c)))));
  t.append(body);
  return el("div", { class: "table-scroll" }, t);
}

// ---- tiles ----------------------------------------------------------------

function tile(k, v, d, title) {
  return el("div", { class: "tile", ...(title ? { title } : {}) }, el("span", { class: "k" }, k), el("span", { class: "v" }, v), el("span", { class: "d" }, d));
}
function drawTiles(sh) {
  const t = sh.tot;
  const g = pct(t.guarded_calls, t.llm_calls), p = pct(t.pii_spans_redacted, t.pii_spans);
  $("tiles").replaceChildren(
    tile("Model calls", compact.format(t.llm_calls), `${sh.models.length} model${sh.models.length === 1 ? "" : "s"} and regions`, fmt.format(t.llm_calls)),
    tile("Tokens", compact.format(t.input + t.output), `${compact.format(t.input)} in · ${compact.format(t.output)} out`, fmt.format(t.input + t.output)),
    tile("Guardrail coverage", pctText(g), `${fmt.format(t.guarded_calls)} of ${fmt.format(t.llm_calls)} calls · ${fmt.format(t.blocked_calls)} blocked`),
    tile("Personal data redacted", pctText(p), `${fmt.format(t.pii_spans_redacted)} of ${fmt.format(t.pii_spans)} spans`),
    tile("Content dropped", compact.format(t.content_present_spans), "spans arrived with content; none kept", fmt.format(t.content_present_spans)),
    tile("Tool calls", compact.format(t.tool_calls), `${fmt.format(t.tool_errors)} failed`, fmt.format(t.tool_calls)),
    tile("Traces committed", compact.format(t.traces_committed), "salted Merkle roots", fmt.format(t.traces_committed)),
  );
}

// ---- timeline -------------------------------------------------------------

function when(s) {
  const d = new Date(s);
  return d.toLocaleString(undefined, { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
}
function drawTimeline(sh) {
  const items = [
    ...sh.transitions.map((x) => ({ t: x.at, kind: "change", ico: "↻", text: [`${x.kind === "model" ? "Model" : "Prompt"} changed `, el("code", {}, x.from), " → ", el("code", {}, x.to)] })),
    ...sh.anomalies.map((x) => ({ t: x.start, kind: "anomaly", ico: "!", text: [x.kind === "error_rate" ? "Unusual error rate" : "Unusual guardrail block rate", ` until ${when(x.end)}`] })),
    ...sh.evals.map((x) => ({ t: x.ran_at, kind: "eval", ico: "✓", text: ["Evaluation ", el("code", {}, x.suite), ` on ${x.model}, prompt ${x.prompt_version}: pass rate ${(x.pass_rate * 100).toFixed(1)}%`] })),
  ].sort((a, b) => b.t.localeCompare(a.t));
  const ol = $("timeline");
  if (!items.length) { ol.replaceChildren(el("li", { class: "none" }, "Nothing changed in this range.")); return; }
  ol.replaceChildren(...items.map((i) => el("li", {},
    el("span", { class: `ico ${i.kind}`, "aria-hidden": "true" }, i.ico),
    el("div", {}, el("div", { class: "what" }, ...i.text), el("div", { class: "when" }, when(i.t))))));
}

// ---- the whole board -------------------------------------------------------

function drawStatus(api) {
  const box = $("status");
  const log = api.log;
  const kids = [];
  if (log) {
    kids.push(log.verified
      ? el("span", { class: "pill pass" }, el("span", { "aria-hidden": "true" }, "✓"), " Verified")
      : el("span", { class: "pill fail" }, el("span", { "aria-hidden": "true" }, "✕"), " Failed"));
    kids.push(el("span", {}, `${fmt.format(log.batches)} batch${log.batches === 1 ? "" : "es"}`));
    kids.push(el("span", { class: "agent", title: "Agent ID" }, log.agent_id));
  }
  kids.push(el("span", {}, `Updated ${new Date(api.generated_at).toLocaleTimeString()}`));
  if (api.version) kids.push(el("span", {}, `agent ${api.version}`));
  box.replaceChildren(...kids);
}

function render() {
  const api = state.data;
  if (!api) return;
  drawStatus(api);
  $("cta-link").href = api.early_access;
  const log = api.log;
  const has = log && log.series.length > 0;
  $("empty").hidden = has;
  $("board").hidden = !has;
  if (!has) { $("empty-detail").textContent = api.error || "No batches yet."; return; }
  if (state.firstLoad) {
    state.firstLoad = false;
    const lastT = Date.parse(log.series[log.series.length - 1].start);
    if (Date.now() - lastT > RANGES[state.range]) state.range = "all"; // nothing recent: show everything
  }
  for (const b of $("range").querySelectorAll("button")) b.setAttribute("aria-pressed", String(b.dataset.range === state.range));
  const sh = shape(log, state.range, Date.now());
  const ids = assignSlots(log);
  const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
  $("meta").textContent = `${sh.daily ? "Daily" : "Hourly"} totals in your time zone (${tz}). Refreshes every 30 seconds.`;
  $("calls-sub").textContent = `Calls per ${sh.daily ? "day" : "hour"}, by model`;

  drawTiles(sh);
  drawCalls($("chart-calls"), sh, ids);
  const bucketWord = sh.daily ? "Day" : "Hour";
  $("calls-table").replaceChildren(table([bucketWord, "Model calls", "Input tokens", "Output tokens"],
    sh.buckets.filter((b) => b.llm_calls || b.calls.size).map((b) => [longLabel(b.t, sh.daily), fmt.format([...b.calls.values()].reduce((s, v) => s + v, 0)), fmt.format(b.input), fmt.format(b.output)]), [1, 2, 3]));

  drawLines($("chart-coverage"), sh, [
    { name: "Guardrail decision", color: "var(--line-a)", value: (b) => pct(b.guarded_calls, b.llm_calls), text: (v, b) => `${pctText(v)} (${fmt.format(b.guarded_calls)} of ${fmt.format(b.llm_calls)})` },
    { name: "Personal data redacted", color: "var(--line-b)", value: (b) => pct(b.pii_spans_redacted, b.pii_spans), text: (v, b) => `${pctText(v)} (${fmt.format(b.pii_spans_redacted)} of ${fmt.format(b.pii_spans)})` },
  ], { max: 100, format: (v) => `${v}%`, label: "Guardrail coverage and personal-data redaction per time bucket", legendId: "coverage-legend" });
  $("coverage-table").replaceChildren(table([bucketWord, "Guardrail coverage", "Calls", "Redacted", "Personal-data spans"],
    sh.buckets.filter((b) => b.llm_calls || b.pii_spans).map((b) => [longLabel(b.t, sh.daily), pctText(pct(b.guarded_calls, b.llm_calls)), fmt.format(b.llm_calls), pctText(pct(b.pii_spans_redacted, b.pii_spans)), fmt.format(b.pii_spans)]), [1, 2, 3, 4]));

  drawLines($("chart-tokens"), sh, [
    { name: "Input tokens", color: "var(--line-a)", value: (b) => b.input, text: (v) => fmt.format(v) },
    { name: "Output tokens", color: "var(--line-b)", value: (b) => b.output, text: (v) => fmt.format(v) },
  ], { format: (v) => compact.format(v), label: "Input and output tokens per time bucket", legendId: "tokens-legend" });
  $("tokens-table").replaceChildren(table([bucketWord, "Input tokens", "Output tokens"],
    sh.buckets.filter((b) => b.input || b.output).map((b) => [longLabel(b.t, sh.daily), fmt.format(b.input), fmt.format(b.output)]), [1, 2]));

  const total = sh.tot.llm_calls || 1;
  $("models-table").replaceChildren(table(["Model", "Calls", "Share", "Tokens in", "Tokens out"],
    sh.models.map((m) => [
      el("td", {}, el("i", { class: "sw", style: `background:${ids.slot.get(modelKey(m)) || "var(--series-other)"}` }), m.model,
        el("span", { class: "sub2" }, `${m.provider} · ${m.region || "region unknown"}`)),
      fmt.format(m.calls), `${((100 * m.calls) / total).toFixed(1)}%`, compact.format(m.input), compact.format(m.output)]), [1, 2, 3, 4]));

  drawTimeline(sh);

  $("chain-sub").replaceChildren(log.verified
    ? `Every batch is signed by this agent's key and names the previous batch's digest. Verified from batch 1 to ${fmt.format(log.last_seq)}.`
    : el("span", { class: "notice fail" }, `✕ Failed: ${log.problem}. Figures cover only the batches before it.`));
  $("chain-table").replaceChildren(table(["Batch", "Hour (UTC)", "Model calls", "Traces", "Digest"],
    log.recent_batches.map((b) => [fmt.format(b.seq), b.window_start.replace("T", " ").replace(":00:00Z", ":00"), fmt.format(b.llm_calls), fmt.format(b.traces), el("td", { class: "digest" }, b.digest)]), [0, 2, 3]));
}

async function refresh() {
  for (const c of document.querySelectorAll(".chart")) c.classList.add("loading");
  try {
    const r = await fetch("api/log", { cache: "no-store" });
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    state.data = await r.json();
    render();
  } catch (e) {
    $("status").replaceChildren(el("span", { class: "pill fail" }, el("span", { "aria-hidden": "true" }, "✕"), " Agent not reachable"), el("span", {}, String(e.message || e)));
  } finally {
    for (const c of document.querySelectorAll(".chart")) c.classList.remove("loading");
  }
}

$("range").addEventListener("click", (e) => {
  const b = e.target.closest("button[data-range]");
  if (!b) return;
  state.range = b.dataset.range;
  save("range", state.range);
  render();
});
let resizeTimer;
addEventListener("resize", () => { clearTimeout(resizeTimer); resizeTimer = setTimeout(render, 120); });
refresh();
setInterval(refresh, REFRESH_MS);
