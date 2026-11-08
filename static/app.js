// ai-usage dashboard: polls /api/usage (hourly rows from collect.py) and renders SVG charts.
// Colors follow the entity (tool:profile) in a fixed, CVD-validated order; text never wears series color.

const SVG = "http://www.w3.org/2000/svg";
const POLL_MS = 10_000;
const HOUR = 3_600_000;

// Fixed categorical order (dataviz reference palette, dark steps). Validated adjacent-pair safe.
const GROUPS = [
  { key: "claude-code:personal", label: "Claude · personal", color: "#3987e5" },
  { key: "claude-code:work-subscription", label: "Claude · work", color: "#d95926" },
  { key: "claude-code:work-foundry", label: "Claude · Foundry", color: "#199e70" },
  { key: "codex:personal-chatgpt", label: "Codex · personal", color: "#c98500" },
  { key: "codex:work-foundry", label: "Codex · Foundry", color: "#d55181" },
  { key: "copilot:github-copilot", label: "Copilot", color: "#008300" },
  { key: "claude-code:legacy", label: "Claude · legacy", color: "#9085e9" },
  { key: "claude-code:no-profile", label: "Claude · no profile", color: "#e66767" },
];
const OTHER = { key: "other", label: "Other", color: "#8f8e86" };
const GROUP_BY_KEY = new Map(GROUPS.map((g) => [g.key, g]));

const TABS = [
  { id: "all", label: "All", match: (r) => r.tool !== "foundry-proxy" },
  { id: "claude", label: "Claude", match: (r) => r.tool === "claude-code" },
  { id: "codex", label: "Codex", match: (r) => r.tool === "codex" },
  { id: "copilot", label: "Copilot", match: (r) => r.tool === "copilot" },
  { id: "foundry", label: "Foundry", match: (r) => r.profile === "work-foundry" && r.tool !== "foundry-proxy" },
];
const METRICS = [
  { id: "total", label: "Total tokens", unit: "tokens", get: (r) => r.total },
  { id: "uncached", label: "Uncached", unit: "uncached tokens", get: (r) => Math.max(r.input - r.cached, 0) + r.output },
  { id: "output", label: "Output", unit: "output tokens", get: (r) => r.output },
  { id: "requests", label: "Requests", unit: "requests", get: (r) => r.requests },
];
const BURNS = [1, 3, 6].map((h) => ({ id: String(h), label: `${h}h`, hours: h }));
const RANGES = [
  { id: "24h", label: "24h", hours: 24, bucket: 1 },
  { id: "7d", label: "7d", hours: 24 * 7, bucket: 4 },
  { id: "30d", label: "30d", hours: 24 * 30, bucket: 24 },
];

const state = Object.assign(
  { tab: "all", metric: "total", burn: "3", range: "7d", sessionSort: "recent" },
  JSON.parse(localStorage.getItem("ai-usage-ui") || "{}"),
  // Bookmarkable overrides, e.g. /?tab=foundry&range=30d
  Object.fromEntries([...new URLSearchParams(location.search)].filter(([k]) => ["tab", "metric", "burn", "range"].includes(k))),
);
let data = null;
let etag = null;
let lastOk = 0;

const $ = (id) => document.getElementById(id);
const compact = new Intl.NumberFormat("en", { notation: "compact", maximumFractionDigits: 1 });
const exact = new Intl.NumberFormat("en");
const fmt = (n) => (Math.abs(n) < 1000 ? exact.format(Math.round(n)) : compact.format(n));
const dayFmt = new Intl.DateTimeFormat("en", { weekday: "short", month: "short", day: "numeric" });
const hourFmt = new Intl.DateTimeFormat("en", { weekday: "short", month: "short", day: "numeric", hour: "numeric" });
const shortDay = new Intl.DateTimeFormat("en", { month: "short", day: "numeric" });
const hourOnly = new Intl.DateTimeFormat("en", { hour: "numeric" });
const whenFmt = new Intl.DateTimeFormat("en", { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });

function el(tag, attrs = {}, ...kids) {
  const node = tag.startsWith("svg:") ? document.createElementNS(SVG, tag.slice(4)) : document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "text") node.textContent = v;
    else if (k === "bg") node.style.background = v;
    else if (k === "width%") node.style.width = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat()) if (kid !== null && kid !== undefined) node.append(kid);
  return node;
}
const swatch = (color) => el("span", { class: "swatch", bg: color, "aria-hidden": "true" });
const groupOf = (r) => GROUP_BY_KEY.get(`${r.tool}:${r.profile}`) || OTHER;
const save = () => localStorage.setItem("ai-usage-ui", JSON.stringify(state));
const metric = () => METRICS.find((m) => m.id === state.metric) || METRICS[0];
const range = () => RANGES.find((r) => r.id === state.range) || RANGES[1];
const burnHours = () => (BURNS.find((b) => b.id === state.burn) || BURNS[1]).hours;
const tab = () => TABS.find((t) => t.id === state.tab) || TABS[0];
const floorHour = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate(), d.getHours());
const startOfDay = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate());

// ---------- data ----------

function parse(snapshot) {
  const cols = snapshot.columns;
  const idx = Object.fromEntries(cols.map((c, i) => [c, i]));
  const rows = snapshot.rows.map((a) => {
    const [y, mo, d, h] = a[idx.hour].split(/[-T:]/).map(Number);
    const r = { t: new Date(y, mo - 1, d, h) };
    for (const c of cols) if (c !== "hour") r[c] = a[idx[c]];
    return r;
  });
  return { ...snapshot, rows, generated: new Date(snapshot.meta.generated) };
}

function periodBounds(now, period = "month", resetDay = 1, resetWeekday = 0) {
  if (period === "day") {
    const start = startOfDay(now);
    return [start, new Date(start.getFullYear(), start.getMonth(), start.getDate() + 1)];
  }
  if (period === "week") {
    const start = startOfDay(now);
    const back = (start.getDay() + 6 - resetWeekday) % 7; // getDay: 0=Sun; resetWeekday: 0=Mon
    start.setDate(start.getDate() - back);
    return [start, new Date(start.getFullYear(), start.getMonth(), start.getDate() + 7)];
  }
  let start = new Date(now.getFullYear(), now.getMonth(), resetDay);
  if (start > now) start = new Date(now.getFullYear(), now.getMonth() - 1, resetDay);
  return [start, new Date(start.getFullYear(), start.getMonth() + 1, resetDay)];
}

function sumWhere(rows, get, from, to = Infinity) {
  let s = 0;
  for (const r of rows) if (r.t >= from && r.t < to) s += get(r);
  return s;
}

// Hourly rate over the trailing window (current partial hour counts toward the window).
function rate(rows, get, now, hours) {
  return sumWhere(rows, get, new Date(floorHour(now) - (hours - 1) * HOUR)) / hours;
}

// ---------- tooltip ----------

const tip = $("tip");
function showTip(evt, head, lines) {
  tip.replaceChildren(
    el("div", { class: "t-head", text: head }),
    ...lines.map(([label, value, color]) =>
      el("div", { class: "t-row" }, el("span", {}, color ? swatch(color) : null, label), el("b", { text: value }))),
  );
  tip.hidden = false;
  const pad = 14;
  const { innerWidth: w, innerHeight: h } = window;
  const rect = tip.getBoundingClientRect();
  tip.style.left = `${Math.min(evt.clientX + pad, w - rect.width - 8)}px`;
  tip.style.top = `${Math.min(evt.clientY + pad, h - rect.height - 8)}px`;
}
const hideTip = () => { tip.hidden = true; };

// ---------- chart helpers ----------

function niceMax(v) {
  if (v <= 0) return 1;
  const p = 10 ** Math.floor(Math.log10(v));
  for (const m of [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

function frame(svg, height) {
  const width = svg.parentElement.clientWidth || 900;
  svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
  svg.setAttribute("height", height);
  svg.replaceChildren();
  return { width, height, m: { top: 22, right: 18, bottom: 30, left: 56 } };
}

function yAxis(svg, f, yMax, y) {
  const ticks = 4;
  for (let i = 0; i <= ticks; i++) {
    const v = (yMax / ticks) * i;
    const yy = y(v);
    svg.append(el("svg:line", { class: i === 0 ? "axis-line" : "grid-line", x1: f.m.left, x2: f.width - f.m.right, y1: yy, y2: yy }));
    svg.append(el("svg:text", { x: f.m.left - 8, y: yy + 4, "text-anchor": "end", text: fmt(v) }));
  }
}

// ---------- renderers ----------

function renderTabs(rows) {
  $("tabs").replaceChildren(...TABS.map((t) => {
    const groups = new Set(rows.filter(t.match).map((r) => groupOf(r).key));
    return el("button", {
      class: "tab", role: "tab", "aria-selected": String(t.id === state.tab),
      onclick: () => { state.tab = t.id; save(); render(); },
    }, t.label, el("span", { class: "count", text: String(groups.size) }));
  }));
}

function renderSeg(id, options, key) {
  $(id).replaceChildren(...options.map((o) => el("button", {
    type: "button", "aria-pressed": String(o.id === state[key]), text: o.label,
    onclick: () => { state[key] = o.id; save(); render(); },
  })));
}

function tabBudget(groupKeys) {
  // Sum budgets only when every group in the tab has one with the same period/reset.
  const bs = groupKeys.map((k) => data.budgets[k]).filter(Boolean);
  if (!bs.length || bs.length !== groupKeys.length) return null;
  const sig = (b) => `${b.period}|${b.reset_day}|${b.reset_weekday}|${b.metric}`;
  if (new Set(bs.map(sig)).size !== 1) return null;
  return { ...bs[0], tokens: bs.reduce((s, b) => s + b.tokens, 0) };
}

function renderHero(rows, now, groupKeys) {
  const m = metric();
  const r = range();
  const burn = burnHours();
  const budget = tabBudget(groupKeys);
  const get = budget && state.metric !== "requests" ? METRICS.find((x) => x.id === budget.metric).get : m.get;
  const [pStart, pEnd] = budget
    ? periodBounds(now, budget.period, budget.reset_day, budget.reset_weekday)
    : periodBounds(now, "month");

  const rangeTotal = sumWhere(rows, m.get, new Date(floorHour(now) - (r.hours - 1) * HOUR));
  const today = sumWhere(rows, m.get, startOfDay(now));
  const burnRate = rate(rows, m.get, now, burn);
  const projRate = rate(rows, get, now, 24 * 7);
  const toDate = sumWhere(rows, get, pStart);
  const hoursLeft = Math.max((pEnd - now) / HOUR, 0);
  const projected = toDate + projRate * hoursLeft;

  const title = tab().label === "All" ? "All tools" : tab().label;
  $("hero-title").replaceChildren(document.createTextNode(`${title} · last ${r.label}`));
  $("hero-value").textContent = fmt(rangeTotal);
  $("hero-value").title = exact.format(rangeTotal);
  $("hero-unit").textContent = m.unit;

  const stats = [
    ["Today", `${fmt(today)}`],
    [`Burn, last ${burn}h`, `${fmt(burnRate)}/h`],
    [budget ? "Period to date" : "Month to date", fmt(toDate)],
    [`Projected by ${shortDay.format(new Date(pEnd - 1))}`, fmt(projected)],
  ];
  if (budget) {
    const pct = (toDate / budget.tokens) * 100;
    const emptyAt = projRate > 0 && toDate < budget.tokens ? new Date(+now + ((budget.tokens - toDate) / projRate) * HOUR) : null;
    stats.push(["Budget used", `${pct.toFixed(0)}% of ${fmt(budget.tokens)}`]);
    stats.push(["Budget empty", toDate >= budget.tokens ? "Now" : emptyAt && emptyAt < pEnd ? dayFmt.format(emptyAt) : "Not this period"]);
  }
  $("hero-stats").replaceChildren(...stats.map(([k, v]) => el("div", {}, el("dt", { text: k }), el("dd", { text: v }))));

  renderCumulative(rows, now, { get, pStart, pEnd, projRate, budget, unit: budget && state.metric !== "requests" ? budget.metric : m.unit });
}

function renderCumulative(rows, now, { get, pStart, pEnd, projRate, budget, unit }) {
  const svg = $("cum-chart");
  const f = frame(svg, 260);
  const nowH = floorHour(now);
  const hours = Math.ceil((pEnd - pStart) / HOUR);
  const byHour = new Float64Array(hours);
  for (const r of rows) {
    const i = Math.floor((r.t - pStart) / HOUR);
    if (i >= 0 && i < hours) byHour[i] += get(r);
  }
  const nowIdx = Math.min(Math.floor((nowH - pStart) / HOUR), hours - 1);
  const cum = [];
  let run = 0;
  for (let i = 0; i <= nowIdx; i++) { run += byHour[i]; cum.push(run); }
  const projEnd = run + projRate * Math.max((pEnd - now) / HOUR, 0);
  const yMax = niceMax(Math.max(projEnd, budget ? budget.tokens : 0, run) * 1.05);

  const x = (i) => f.m.left + (i / hours) * (f.width - f.m.left - f.m.right);
  const y = (v) => f.height - f.m.bottom - (v / yMax) * (f.height - f.m.top - f.m.bottom);
  yAxis(svg, f, yMax, y);

  // Day ticks
  const days = Math.round(hours / 24);
  const step = days > 20 ? 3 : days > 10 ? 2 : 1;
  for (let d = 0; d <= days; d += step) {
    const at = new Date(pStart.getFullYear(), pStart.getMonth(), pStart.getDate() + d);
    const i = (at - pStart) / HOUR;
    if (i > hours) break;
    svg.append(el("svg:text", { x: x(i), y: f.height - 10, "text-anchor": "middle", text: shortDay.format(at) }));
  }

  if (budget) {
    svg.append(el("svg:line", { class: "budget-line", x1: f.m.left, x2: f.width - f.m.right, y1: y(budget.tokens), y2: y(budget.tokens) }));
    svg.append(el("svg:text", { class: "budget-label", x: f.width - f.m.right, y: y(budget.tokens) - 6, "text-anchor": "end", text: `Budget ${fmt(budget.tokens)}` }));
  }

  if (cum.length) {
    const pts = cum.map((v, i) => `${x(i + 1).toFixed(1)},${y(v).toFixed(1)}`);
    const line = `M${x(0).toFixed(1)},${y(0).toFixed(1)} L${pts.join(" L")}`;
    const area = `${line} L${x(cum.length).toFixed(1)},${y(0).toFixed(1)} Z`;
    const grad = el("svg:linearGradient", { id: "cum-fill", x1: 0, x2: 0, y1: 0, y2: 1 },
      el("svg:stop", { offset: "0%", "stop-color": "#c3c2b7", "stop-opacity": 0.22 }),
      el("svg:stop", { offset: "100%", "stop-color": "#c3c2b7", "stop-opacity": 0 }));
    svg.append(el("svg:defs", {}, grad));
    svg.append(el("svg:path", { d: area, fill: "url(#cum-fill)" }));
    svg.append(el("svg:path", { d: line, fill: "none", stroke: "#c3c2b7", "stroke-width": 2, "stroke-linejoin": "round" }));
    const xn = x(cum.length);
    svg.append(el("svg:path", {
      d: `M${xn.toFixed(1)},${y(run).toFixed(1)} L${x(hours).toFixed(1)},${y(projEnd).toFixed(1)}`,
      fill: "none", stroke: "#c3c2b7", "stroke-width": 2, "stroke-dasharray": "5 5",
    }));
    svg.append(el("svg:line", { class: "now-line", x1: xn, x2: xn, y1: f.m.top - 6, y2: f.height - f.m.bottom }));
    svg.append(el("svg:text", { class: "now-label", x: xn, y: f.m.top - 10, "text-anchor": "middle", text: "Now" }));
    svg.append(el("svg:circle", { cx: xn, cy: y(run), r: 4.5, fill: "#c3c2b7", stroke: "#1a1a19", "stroke-width": 2 }));
    svg.append(el("svg:text", { x: x(hours) - 4, y: y(projEnd) - 8, "text-anchor": "end", fill: "#c3c2b7", text: fmt(projEnd) }));
  }

  // Crosshair + tooltip
  const cross = el("svg:line", { class: "crosshair", y1: f.m.top, y2: f.height - f.m.bottom, visibility: "hidden" });
  const dot = el("svg:circle", { r: 4, fill: "#f0efec", stroke: "#1a1a19", "stroke-width": 2, visibility: "hidden" });
  svg.append(cross, dot);
  const hit = el("svg:rect", { x: f.m.left, y: f.m.top, width: f.width - f.m.left - f.m.right, height: f.height - f.m.top - f.m.bottom, fill: "transparent" });
  hit.addEventListener("mousemove", (evt) => {
    const pt = svg.getBoundingClientRect();
    const scale = f.width / pt.width;
    const i = Math.round((((evt.clientX - pt.left) * scale - f.m.left) / (f.width - f.m.left - f.m.right)) * hours);
    const clamped = Math.min(Math.max(i, 0), hours);
    const at = new Date(+pStart + clamped * HOUR);
    const past = clamped <= cum.length;
    const v = past ? (clamped === 0 ? 0 : cum[clamped - 1]) : run + projRate * ((at - now) / HOUR);
    cross.setAttribute("x1", x(clamped)); cross.setAttribute("x2", x(clamped)); cross.setAttribute("visibility", "visible");
    dot.setAttribute("cx", x(clamped)); dot.setAttribute("cy", y(v)); dot.setAttribute("visibility", "visible");
    const lines = [[past ? "Recorded" : "Projected", exact.format(Math.round(v))]];
    if (past && clamped > 0) lines.push(["That hour", exact.format(Math.round(byHour[clamped - 1]))]);
    if (budget) lines.push(["Budget", `${((v / budget.tokens) * 100).toFixed(0)}%`]);
    showTip(evt, hourFmt.format(at), lines);
  });
  hit.addEventListener("mouseleave", () => { cross.setAttribute("visibility", "hidden"); dot.setAttribute("visibility", "hidden"); hideTip(); });
  svg.append(hit);

  $("cum-desc").textContent = `Cumulative ${unit} from ${dayFmt.format(pStart)}: ${exact.format(Math.round(run))} so far, projected ${exact.format(Math.round(projEnd))} by ${dayFmt.format(new Date(pEnd - 1))} at the 7-day average rate.`;
  $("cum-key").replaceChildren(...[
    el("span", {}, el("span", { class: "key-line" }), "Recorded"),
    el("span", {}, el("span", { class: "key-line dashed" }), "Projected (7-day average rate)"),
    budget ? el("span", {}, el("span", { class: "key-line dotted" }), "Budget") : null,
    el("span", { text: `Cumulative ${unit}, ${budget ? "budget period" : "this month"}` }),
  ].filter(Boolean));
}

function bucketed(rows, now) {
  const r = range();
  const m = metric();
  const n = Math.ceil(r.hours / r.bucket);
  const end = r.bucket === 24 ? new Date(+startOfDay(now) + 24 * HOUR) : new Date(+floorHour(now) + HOUR);
  // Align multi-hour buckets to local midnight so labels read naturally.
  const alignedEnd = r.bucket > 1 && r.bucket < 24
    ? new Date(+startOfDay(now) + Math.ceil((now - startOfDay(now)) / (r.bucket * HOUR)) * r.bucket * HOUR)
    : end;
  const start = new Date(+alignedEnd - n * r.bucket * HOUR);
  const groups = [...GROUPS, OTHER];
  const values = groups.map(() => new Float64Array(n));
  for (const row of rows) {
    const i = Math.floor((row.t - start) / (r.bucket * HOUR));
    if (i < 0 || i >= n) continue;
    const g = groups.indexOf(groupOf(row));
    values[g][i] += m.get(row);
  }
  const present = groups.map((g, gi) => ({ ...g, values: values[gi], total: values[gi].reduce((a, b) => a + b, 0) }))
    .filter((g) => g.total > 0);
  return { start, n, bucket: r.bucket, present };
}

function renderBars(rows, now) {
  const svg = $("bar-chart");
  const f = frame(svg, 240);
  const { start, n, bucket, present } = bucketed(rows, now);
  const totals = Array.from({ length: n }, (_, i) => present.reduce((s, g) => s + g.values[i], 0));
  const yMax = niceMax(Math.max(...totals, 0) * 1.05);
  const band = (f.width - f.m.left - f.m.right) / n;
  const barW = Math.max(Math.min(band * 0.7, 28), 2);
  const x = (i) => f.m.left + i * band + (band - barW) / 2;
  const y = (v) => f.height - f.m.bottom - (v / yMax) * (f.height - f.m.top - f.m.bottom);
  yAxis(svg, f, yMax, y);

  const labelEvery = Math.max(1, Math.ceil(n / 12));
  for (let i = 0; i < n; i += labelEvery) {
    const at = new Date(+start + i * bucket * HOUR);
    const text = bucket === 1 ? hourOnly.format(at) : bucket === 24 ? shortDay.format(at) : (at.getHours() === 0 ? shortDay.format(at) : hourOnly.format(at));
    svg.append(el("svg:text", { x: x(i) + barW / 2, y: f.height - 10, "text-anchor": "middle", text }));
  }

  const gap = 2;
  for (let i = 0; i < n; i++) {
    let base = 0;
    const segs = present.filter((g) => g.values[i] > 0);
    segs.forEach((g, si) => {
      const v = g.values[i];
      const y0 = y(base), y1 = y(base + v);
      const top = si === segs.length - 1;
      const h = Math.max(y0 - y1 - (si > 0 ? gap : 0), 1);
      const yTop = y0 - (si > 0 ? gap : 0) - h;
      const r = top ? Math.min(4, barW / 2, h) : 0;
      const d = r
        ? `M${x(i)},${yTop + h} V${yTop + r} Q${x(i)},${yTop} ${x(i) + r},${yTop} H${x(i) + barW - r} Q${x(i) + barW},${yTop} ${x(i) + barW},${yTop + r} V${yTop + h} Z`
        : `M${x(i)},${yTop + h} V${yTop} H${x(i) + barW} V${yTop + h} Z`;
      svg.append(el("svg:path", { d, fill: g.color }));
      base += v;
    });
  }
  // Hit targets per bucket, wider than the bars.
  for (let i = 0; i < n; i++) {
    const hit = el("svg:rect", { x: f.m.left + i * band, y: f.m.top, width: band, height: f.height - f.m.top - f.m.bottom, fill: "transparent" });
    hit.addEventListener("mousemove", (evt) => {
      const at = new Date(+start + i * bucket * HOUR);
      const head = bucket === 24 ? dayFmt.format(at) : `${hourFmt.format(at)} – ${hourOnly.format(new Date(+at + bucket * HOUR))}`;
      const lines = present.filter((g) => g.values[i] > 0).reverse().map((g) => [g.label, exact.format(Math.round(g.values[i])), g.color]);
      lines.push(["Total", exact.format(Math.round(totals[i]))]);
      showTip(evt, head, lines);
    });
    hit.addEventListener("mouseleave", hideTip);
    svg.append(hit);
  }

  const m = metric();
  $("bars-sub").textContent = `${m.unit} per ${bucket === 1 ? "hour" : bucket === 24 ? "day" : `${bucket} hours`}, last ${range().label}`;
  $("bars-legend").replaceChildren(...(present.length ? present : [{ label: "No usage in range", color: "#8f8e86", total: 0 }])
    .map((g) => el("span", {}, swatch(g.color), g.label, el("b", { text: fmt(g.total) }))));

  const head = el("tr", {}, el("th", { text: "Bucket start" }), ...present.map((g) => el("th", { text: g.label })), el("th", { text: "Total" }));
  const body = [];
  for (let i = n - 1; i >= 0; i--) {
    if (!totals[i]) continue;
    const at = new Date(+start + i * bucket * HOUR);
    body.push(el("tr", {}, el("td", { text: bucket === 24 ? dayFmt.format(at) : hourFmt.format(at) }),
      ...present.map((g) => el("td", { text: exact.format(Math.round(g.values[i])) })),
      el("td", { text: exact.format(Math.round(totals[i])) })));
  }
  $("bars-table").replaceChildren(el("thead", {}, head), el("tbody", {}, body));
}

function sparkline(values, color) {
  const w = 280, h = 42;
  const max = Math.max(...values, 1);
  const svg = el("svg:svg", { class: "spark", viewBox: `0 0 ${w} ${h}`, preserveAspectRatio: "none", "aria-hidden": "true" });
  const pts = Array.from(values, (v, i) => `${((i / Math.max(values.length - 1, 1)) * w).toFixed(1)},${(h - 2 - (v / max) * (h - 6)).toFixed(1)}`);
  svg.append(el("svg:polyline", { points: pts.join(" "), fill: "none", stroke: color, "stroke-width": 2, "stroke-linejoin": "round", "vector-effect": "non-scaling-stroke" }));
  return svg;
}

function renderGroups(rows, now) {
  const m = metric();
  const r = range();
  const from = new Date(floorHour(now) - (r.hours - 1) * HOUR);
  const { present } = bucketed(rows, now);
  const known = [...GROUPS, OTHER];
  const keys = [...new Set(rows.map((x) => groupOf(x).key))].sort((a, b) => known.findIndex((g) => g.key === a) - known.findIndex((g) => g.key === b));
  const cards = keys.map((key) => {
    const g = known.find((k) => k.key === key);
    const gr = rows.filter((x) => groupOf(x).key === key);
    const total = sumWhere(gr, m.get, from);
    const reqs = sumWhere(gr, (x) => x.requests, from);
    const burn = rate(gr, m.get, now, burnHours());
    const models = new Map();
    for (const x of gr) if (x.t >= from) models.set(x.model, (models.get(x.model) || 0) + m.get(x));
    const top = [...models.entries()].sort((a, b) => b[1] - a[1]).slice(0, 3);
    const series = present.find((p) => p.key === key);
    const kids = [
      el("div", { class: "name" }, swatch(g.color), g.label),
      el("div", { class: "big", title: exact.format(total), text: fmt(total) }),
      el("dl", { class: "facts" },
        el("div", {}, el("dt", { text: "Requests" }), el("dd", { text: fmt(reqs) })),
        el("div", {}, el("dt", { text: `Burn ${burnHours()}h` }), el("dd", { text: `${fmt(burn)}/h` })),
        el("div", {}, el("dt", { text: "Share" }), el("dd", { text: "" }))),
      series ? sparkline(series.values, g.color) : el("div", { class: "empty", text: `No usage in the last ${r.label}` }),
      el("div", { class: "models" }, ...top.flatMap(([name, v], i) => [i ? " · " : "", el("b", { text: name }), ` ${fmt(v)}`])),
    ];
    const budget = data.budgets[key];
    if (budget) kids.push(budgetBlock(gr, now, budget));
    return { key, total, node: el("article", { class: "card group-card" }, kids) };
  });
  const all = cards.reduce((s, c) => s + c.total, 0);
  for (const c of cards) {
    const share = all ? (c.total / all) * 100 : 0;
    c.node.querySelector(".facts div:nth-child(3) dd").textContent = !all ? "—" : share > 0 && share < 1 ? "<1%" : `${share.toFixed(0)}%`;
  }
  $("groups").replaceChildren(...(cards.length ? cards.map((c) => c.node) : [el("div", { class: "empty", text: "No usage for this tab yet." })]));
}

function budgetBlock(rows, now, b) {
  const get = METRICS.find((x) => x.id === b.metric).get;
  const [start, end] = periodBounds(now, b.period, b.reset_day, b.reset_weekday);
  const used = sumWhere(rows, get, start);
  const projRate = rate(rows, get, now, 24 * 7);
  const projected = used + projRate * Math.max((end - now) / HOUR, 0);
  const pct = (used / b.tokens) * 100;
  const level = used >= b.tokens ? ["critical", "✕", "Over budget"]
    : projected > b.tokens ? ["serious", "▲", "On track to exceed"]
      : pct > 75 ? ["warning", "!", "Above 75%"] : ["good", "✓", "Within budget"];
  return el("div", { class: "budget" },
    el("div", {}, `${b.period} budget · ${fmt(used)} of ${fmt(b.tokens)} ${b.metric} (${pct.toFixed(0)}%)`),
    el("div", { class: "budget-track" }, el("div", { class: "budget-fill", "width%": `${Math.min(pct, 100)}%` })),
    el("div", {}, el("span", { class: `status ${level[0]}` }, el("span", { "aria-hidden": "true", text: level[1] }), level[2]),
      ` · projected ${fmt(projected)} by ${shortDay.format(new Date(end - 1))}`));
}

function renderModels(rows, now) {
  const r = range();
  const from = new Date(floorHour(now) - (r.hours - 1) * HOUR);
  const agg = new Map();
  for (const x of rows) {
    if (x.t < from) continue;
    const g = groupOf(x);
    const k = `${g.key}|${x.model}`;
    const a = agg.get(k) || { g, model: x.model, requests: 0, input: 0, cached: 0, output: 0, reasoning: 0, total: 0 };
    for (const c of ["requests", "input", "cached", "output", "reasoning", "total"]) a[c] += x[c];
    agg.set(k, a);
  }
  const list = [...agg.values()].sort((a, b) => b.total - a.total);
  const cols = ["requests", "input", "cached", "output", "reasoning", "total"];
  $("models-sub").textContent = `last ${r.label} · ${list.length} profile/model pairs`;
  $("models-table").replaceChildren(
    el("thead", {}, el("tr", {}, el("th", { text: "Profile" }), el("th", { class: "left", text: "Model" }), ...cols.map((c) => el("th", { text: c[0].toUpperCase() + c.slice(1) })))),
    el("tbody", {}, list.length ? list.map((a) => el("tr", {},
      el("td", {}, el("span", { class: "legend" }, el("span", {}, swatch(a.g.color), a.g.label))),
      el("td", { class: "left", text: a.model }),
      ...cols.map((c) => el("td", { title: exact.format(a[c]), text: fmt(a[c]) })))) : el("tr", {}, el("td", { colspan: 8, class: "empty", text: "No usage in range." }))),
  );
}

function renderSources(allRows, now) {
  const r = range();
  const from = new Date(floorHour(now) - (r.hours - 1) * HOUR);
  const proxy = allRows.filter((x) => x.tool === "foundry-proxy");
  const codexFoundry = allRows.filter((x) => x.tool === "codex" && x.profile === "work-foundry");
  const proxyTotal = sumWhere(proxy, (x) => x.total, from);
  const codexTotal = sumWhere(codexFoundry, (x) => x.total, from);
  const statusOf = (s) => (s === "ok" ? ["good", "✓", "ok"] : s === "not found" ? ["warning", "!", "not found"] : ["critical", "✕", s]);
  const table = el("table", {},
    el("thead", {}, el("tr", {}, el("th", { text: "Source" }), el("th", { class: "left", text: "Status" }), el("th", { text: `Requests (${data.meta.days}d)` }), el("th", { class: "left", text: "Path" }))),
    el("tbody", {}, data.sources.map((s) => {
      const [cls, icon, label] = statusOf(s.status);
      return el("tr", {}, el("td", { text: s.source }),
        el("td", { class: "left" }, el("span", { class: `status ${cls}` }, el("span", { "aria-hidden": "true", text: icon }), label)),
        el("td", { text: exact.format(s.requests) }), el("td", { class: "left muted", text: s.path }));
    })));
  const note = el("p", { class: "muted" },
    `Foundry proxy cross-check (last ${r.label}): proxy logged ${fmt(proxyTotal)} tokens; Codex Foundry sessions recorded ${fmt(codexTotal)}. `,
    "The proxy is not added to totals because it overlaps Codex Foundry sessions.");
  const parts = [table, note];
  if (data.meta.config_error) parts.push(el("p", { class: "status warning" }, el("span", { "aria-hidden": "true", text: "!" }), `Budget config: ${data.meta.config_error}`));
  const m = data.meta;
  $("sources-sub").textContent = `collected in ${exact.format(m.collect_ms)} ms · ${exact.format(m.cache_misses)} files re-read, ${exact.format(m.cache_hits)} cached · every ${m.interval}s`;
  if (m.history) {
    parts.push(el("p", { class: "muted" },
      `History store ${m.history}: ${exact.format(m.history_rows)} hourly rows since ${m.history_since ? dayFmt.format(new Date(m.history_since)) : "—"}. `,
      m.history_only_rows ? `${exact.format(m.history_only_rows)} rows in this window come only from history (their transcripts are gone).` : "Every row in this window still has its source transcript."));
  }
  $("sources-body").replaceChildren(...parts);
  $("foot").replaceChildren(
    el("div", { text: `Timezone ${data.meta.tz}` }),
    el("div", { text: `History ${data.meta.days} days` }),
    el("div", { text: data.meta.config_exists ? `Budgets: ${data.meta.config}` : "No budgets set (see dashboard.example.json)" }),
  );
}

// ---------- work (sessions, projects, ai-work-report overlay) ----------

const SESSION_SORTS = [
  { id: "recent", label: "Recent" },
  { id: "tokens", label: "Tokens" },
  { id: "active", label: "Active time" },
];
const SESSION_LIMIT = 150;
const TITLE_SRC = { ai: "", summary: "", "custom-title": "", index: "", prompt: "first prompt" };
let sessionFilter = "";
const hours = (min) => (min >= 60 ? `${(min / 60).toFixed(1)}h` : `${Math.round(min)}m`);
const sessionMetric = (x) => (state.metric === "requests" ? x.requests : x[state.metric] ?? x.total);
const groupOfSession = (x) => GROUP_BY_KEY.get(`${x.tool}:${x.profile}`) || OTHER;

function sessionsInRange(now) {
  const from = new Date(floorHour(now) - (range().hours - 1) * HOUR);
  return (data.sessions || []).filter((x) => tab().match(x) && new Date(x.last) >= from);
}

// Map ai-work-report classified session ids (full ids or 8-char prefixes) onto live sessions.
function classification() {
  const c = data.work_report?.classified;
  if (!c) return null;
  const entries = Object.entries(c.sessions || {});
  const exactIds = new Map(entries);
  const themes = new Map((c.themes || []).map((t) => [t.id, t]));
  const lookup = (id) => exactIds.get(id) || entries.find(([k]) => k.length >= 6 && id.startsWith(k))?.[1] || null;
  return { themes, lookup };
}

function fillPlaceholders(text, ctx) {
  return String(text || "")
    .replace(/<[^>]*>/g, "")
    .replace(/\{(pct|tokens|claude_pct):([a-z0-9-]+)\}/g, (_, kind, id) => {
      const t = ctx.byTheme.get(id);
      if (!t) return "—";
      return kind === "tokens" ? fmt(t.total) : `${ctx.total ? Math.round((t.total / ctx.total) * 100) : 0}%`;
    })
    .replace(/\{total_tokens\}|\{claude_tokens\}/g, fmt(ctx.total))
    .replace(/\{top_n\}/g, String(ctx.count))
    .replace(/\{[^}]*\}/g, "");
}

function renderWorkstreams(sessions) {
  const box = $("workstreams");
  const wr = data.work_report;
  const cls = classification();
  const dir = data.meta.work_report_dir || "data/work-report";
  const runHelp = [
    el("p", { class: "muted" }, "Run the ai-work-report skill whenever you want themes, estimated hours saved, and a narrative. ",
      "Point its workdir and output here and this card picks them up on the next refresh:"),
    el("pre", { class: "cmd", text: `workdir: ${dir}\nbuild:   python3 scripts/build.py ${dir} --days 30 --out ${dir}/ai-work-report.html` }),
  ];
  if (!wr || (!cls && !wr.report_mtime)) {
    box.replaceChildren(...[el("div", { class: "card-head" }, el("h2", { text: "Workstreams" }), el("span", { class: "muted", text: "from the ai-work-report skill" })),
      ...runHelp, wr?.error ? el("p", { class: "status warning" }, el("span", { "aria-hidden": "true", text: "!" }), wr.error) : null].filter(Boolean));
    return;
  }
  const byTheme = new Map();
  let total = 0, matched = 0;
  for (const x of sessions) {
    const hit = cls?.lookup(x.id);
    if (!hit || hit.theme === "none") continue;
    matched++;
    const t = byTheme.get(hit.theme) || { total: 0, sessions: 0, active: 0, manual: 0, saved: 0 };
    const ai = Math.max((x.active_min / 60) * 1.5, 0.1);
    const manual = Math.min(Number(hit.manual_likely) || 0, 16);
    t.total += sessionMetric(x); t.sessions++; t.active += x.active_min; t.manual += manual; t.saved += Math.max(manual - ai, 0);
    byTheme.set(hit.theme, t);
    total += sessionMetric(x);
  }
  const n = wr.narrative || {};
  const ctx = { byTheme, total, count: matched };
  const rows = [...byTheme.entries()].sort((a, b) => b[1].total - a[1].total);
  const sum = (k) => rows.reduce((s, [, t]) => s + t[k], 0);
  const when = wr.classified_mtime || wr.report_mtime;
  box.replaceChildren(...[
    el("div", { class: "card-head" }, el("h2", { text: "Workstreams" }),
      el("span", { class: "muted", text: `ai-work-report · classified ${when ? dayFmt.format(new Date(when)) : "—"} · ${matched} of ${sessions.length} sessions in range matched` })),
    n.title_html ? el("div", { class: "narrative" }, el("h3", { text: fillPlaceholders(n.title_html, ctx) }),
      n.thesis_html ? el("p", { text: fillPlaceholders(n.thesis_html, ctx) }) : null) : null,
    rows.length ? el("div", { class: "table-scroll" }, el("table", {},
      el("thead", {}, el("tr", {}, el("th", { class: "left", text: "Workstream" }), el("th", { text: "Sessions" }),
        el("th", { text: metric().label }), el("th", { text: "Share" }), el("th", { text: "AI active" }),
        el("th", { text: "Est. manual" }), el("th", { text: "Est. saved" }))),
      el("tbody", {}, rows.map(([id, t]) => {
        const theme = cls.themes.get(id) || { name: id };
        return el("tr", {}, el("td", { class: "title" }, theme.name, theme.d ? el("span", { class: "basis", text: theme.d }) : null),
          el("td", { text: exact.format(t.sessions) }), el("td", { text: fmt(t.total) }),
          el("td", { text: total ? `${Math.round((t.total / total) * 100)}%` : "—" }), el("td", { text: hours(t.active) }),
          el("td", { text: `${t.manual.toFixed(1)}h` }), el("td", { text: `${t.saved.toFixed(1)}h` }));
      }), el("tr", {}, el("td", { class: "left", text: "Total" }), el("td", { text: exact.format(sum("sessions")) }),
        el("td", { text: fmt(total) }), el("td", { text: "" }), el("td", { text: hours(sum("active")) }),
        el("td", { text: `${sum("manual").toFixed(1)}h` }), el("td", { text: `${sum("saved").toFixed(1)}h` }))))) :
      el("p", { class: "muted", text: "No sessions in this tab and range match the last classification. Try the 30d range or rerun the skill." }),
    el("p", { class: "muted small", text: "Manual hours are the skill's estimates (capped at 16h per session); saved = manual − AI active time × 1.5, as in the skill's build.py. Estimates, not measurements." }),
    el("div", { class: "work-actions" },
      wr.report_mtime ? el("a", { class: "link-btn", href: "/report", target: "_blank", rel: "noopener", text: `Open full report ↗ (${hourFmt.format(new Date(wr.report_mtime))})` }) : null,
      el("details", {}, el("summary", { class: "muted small", text: "Refresh it with the skill" }), ...runHelp)),
    wr.error ? el("p", { class: "status warning" }, el("span", { "aria-hidden": "true", text: "!" }), wr.error) : null,
  ].filter(Boolean));
}

function renderProjects(sessions) {
  const svg = $("projects-chart");
  const byProject = new Map();
  for (const x of sessions) {
    const key = x.project || "(unknown)";
    const p = byProject.get(key) || { name: key, path: x.path, total: 0, active: 0, sessions: 0, groups: new Map() };
    const v = sessionMetric(x);
    p.total += v; p.active += x.active_min; p.sessions++;
    const g = groupOfSession(x);
    p.groups.set(g.key, (p.groups.get(g.key) || 0) + v);
    byProject.set(key, p);
  }
  let list = [...byProject.values()].sort((a, b) => b.total - a.total);
  const top = list.slice(0, 12);
  if (list.length > 12) {
    const rest = list.slice(12);
    const other = { name: `${rest.length} other projects`, total: 0, active: 0, sessions: 0, groups: new Map() };
    for (const p of rest) { other.total += p.total; other.active += p.active; other.sessions += p.sessions;
      for (const [k, v] of p.groups) other.groups.set(k, (other.groups.get(k) || 0) + v); }
    top.push(other);
  }
  list = top;
  const rowH = 30;
  const f = frame(svg, Math.max(list.length * rowH + 30, 80));
  f.m.left = 210; f.m.right = 150; f.m.top = 6; f.m.bottom = 20;
  const max = niceMax(Math.max(...list.map((p) => p.total), 1));
  const x = (v) => f.m.left + (v / max) * (f.width - f.m.left - f.m.right);
  const order = [...GROUPS, OTHER];
  const used = new Set();
  list.forEach((p, i) => {
    const y = f.m.top + i * rowH;
    const label = p.name.length > 30 ? `…${p.name.slice(-29)}` : p.name;
    svg.append(el("svg:text", { x: f.m.left - 10, y: y + rowH / 2 + 4, "text-anchor": "end", fill: "#c3c2b7", text: label }));
    let base = 0;
    const segs = order.filter((g) => p.groups.get(g.key) > 0);
    segs.forEach((g, si) => {
      const v = p.groups.get(g.key);
      used.add(g.key);
      const x0 = x(base) + (si ? 2 : 0), x1 = x(base + v);
      const w = Math.max(x1 - x0, 1);
      const last = si === segs.length - 1;
      const r = last ? Math.min(4, w, 9) : 0;
      const top0 = y + 6, h = rowH - 12;
      const d = r
        ? `M${x0},${top0} H${x0 + w - r} Q${x0 + w},${top0} ${x0 + w},${top0 + r} V${top0 + h - r} Q${x0 + w},${top0 + h} ${x0 + w - r},${top0 + h} H${x0} Z`
        : `M${x0},${top0} H${x0 + w} V${top0 + h} H${x0} Z`;
      svg.append(el("svg:path", { d, fill: g.color }));
      base += v;
    });
    svg.append(el("svg:text", { x: x(p.total) + 8, y: y + rowH / 2 + 4, fill: "#f0efec", text: `${fmt(p.total)} · ${hours(p.active)} · ${p.sessions} sess.` }));
    const hit = el("svg:rect", { x: 0, y, width: f.width, height: rowH, fill: "transparent" });
    hit.addEventListener("mousemove", (evt) => showTip(evt, p.path ? `${p.name} (${p.path})` : p.name, [
      ...order.filter((g) => p.groups.get(g.key) > 0).map((g) => [g.label, exact.format(Math.round(p.groups.get(g.key))), g.color]),
      ["Active time", hours(p.active)], ["Sessions", exact.format(p.sessions)]]));
    hit.addEventListener("mouseleave", hideTip);
    svg.append(hit);
  });
  if (!list.length) svg.append(el("svg:text", { x: 10, y: 30, text: "No sessions in this tab and range." }));
  $("projects-sub").textContent = `${metric().unit} by project · last ${range().label} · active time = 5-minute slots with activity`;
  $("projects-legend").replaceChildren(...order.filter((g) => used.has(g.key)).map((g) => el("span", {}, swatch(g.color), g.label)));
}

function renderSessions(sessions) {
  renderSeg("session-sort", SESSION_SORTS, "sessionSort");
  const cls = classification();
  const q = sessionFilter.trim().toLowerCase();
  let list = sessions.filter((x) => !q || [x.title, x.project, x.path, x.branch, x.repo, x.model, x.profile]
    .some((v) => v && String(v).toLowerCase().includes(q)));
  const sorters = { recent: (a, b) => (a.last < b.last ? 1 : -1), tokens: (a, b) => sessionMetric(b) - sessionMetric(a), active: (a, b) => b.active_min - a.active_min };
  list = list.sort(sorters[state.sessionSort] || sorters.recent);
  const shown = list.slice(0, SESSION_LIMIT);
  const head = el("tr", {}, el("th", { class: "left", text: "Last active" }), el("th", { class: "left", text: "Session" }),
    el("th", { class: "left", text: "Project" }), el("th", { class: "left", text: "Profile" }),
    cls ? el("th", { class: "left", text: "Workstream" }) : null,
    el("th", { text: "Active" }), el("th", { text: metric().label }));
  const body = shown.map((x) => {
    const g = groupOfSession(x);
    const hit = cls?.lookup(x.id);
    const theme = hit && hit.theme !== "none" ? cls.themes.get(hit.theme)?.name || hit.theme : null;
    const src = TITLE_SRC[x.title_src];
    const tr = el("tr", { class: "clickable", tabindex: "0", "aria-expanded": "false" },
      el("td", { class: "left", text: whenFmt.format(new Date(x.last)) }),
      el("td", { class: "title" }, x.title || el("span", { class: "muted", text: "(untitled)" }),
        src ? el("span", { class: "src", text: src }) : null,
        el("span", { class: "basis", text: [hit?.basis, x.branch && `⎇ ${x.branch}`, x.model].filter(Boolean).join(" · ") })),
      el("td", { class: "left wrap", title: x.path || "", text: x.project || "—" }),
      el("td", { class: "left" }, el("span", { class: "chip" }, swatch(g.color), g.label)),
      cls ? el("td", { class: "left wrap", text: theme || (hit ? "housekeeping" : "—") }) : null,
      el("td", { text: hours(x.active_min) }),
      el("td", { title: exact.format(sessionMetric(x)), text: fmt(sessionMetric(x)) }));
    const cols = cls ? 7 : 6;
    const open = () => toggleDetail(x, tr, (panel) => el("tr", { class: "detail-row" }, el("td", { colspan: cols }, panel)));
    tr.addEventListener("click", open);
    tr.addEventListener("keydown", (e) => { if (e.key === "Enter") open(); });
    return tr;
  });
  $("sessions-table").replaceChildren(el("thead", {}, head),
    el("tbody", {}, body.length ? body : el("tr", {}, el("td", { colspan: 8, class: "empty", text: "No sessions match." }))));
  shown.forEach((x, i) => { if (openRows.has(sessionKey(x))) { openRows.delete(sessionKey(x)); body[i].click(); } });
  $("sessions-sub").textContent = `${exact.format(list.length)} sessions in the last ${range().label}${list.length > SESSION_LIMIT ? `, showing ${SESSION_LIMIT}` : ""}. ` +
    "Titles come from each tool (Claude AI titles, Copilot summaries, Codex thread names), else the first prompt; secrets are redacted.";
}

// ---------- where the tokens went + session detail ----------

const details = new Map();
const openRows = new Set();
const sessionKey = (x) => `${x.tool}|${x.profile}|${x.id}`;

async function loadDetail(x) {
  const key = sessionKey(x);
  if (!details.has(key)) {
    const res = await fetch(`/api/session?key=${encodeURIComponent(key)}`, { cache: "no-store" });
    details.set(key, res.ok ? await res.json() : null);
  }
  return details.get(key);
}

function detailPanel(x, d) {
  const g = groupOfSession(x);
  const meta = el("div", { class: "meta" },
    [`${g.label}`, x.path, x.branch && `⎇ ${x.branch}`, x.repo,
      `${whenFmt.format(new Date(x.first))} → ${whenFmt.format(new Date(x.last))}`, `active ${hours(x.active_min)}`,
      `${exact.format(x.requests)} requests`].filter(Boolean).join(" · "));
  if (!d) return el("div", { class: "detail" }, meta, el("p", { class: "muted", text: "No detail recorded for this session." }));
  const block = (title, body) => el("div", {}, el("h4", { text: title }), body);
  const parts = [meta];
  if (d.prompts?.length) parts.push(block("What you asked", el("ol", {}, d.prompts.map((p) => el("li", { text: p })))));
  if (d.files?.length) {
    parts.push(block(`Files changed (${d.files_total})`, el("ul", { class: "files" },
      d.files.map(([f, n]) => el("li", { title: f }, x.path && f.startsWith(`${x.path}/`) ? f.slice(x.path.length + 1) : f,
        n > 1 ? el("span", { text: ` ×${n}` }) : null)),
      d.files_total > d.files.length ? el("li", { class: "muted", text: `+${d.files_total - d.files.length} more` }) : null)));
  }
  if (d.commands?.length) parts.push(block("Commands", el("ul", {}, d.commands.map((c) => el("li", { text: c })))));
  if (d.refs?.length) parts.push(block("References", el("ul", {}, d.refs.map((r) => el("li", { text: r })))));
  if (d.tools?.length) parts.push(block("Tools used", el("div", { class: "chips" }, d.tools.map(([t, n]) => el("span", { class: "chip", text: `${t} ${n}` })))));
  if (d.models?.length > 1) parts.push(block("Models", el("div", { class: "chips" }, d.models.map(([m, n]) => el("span", { class: "chip", text: `${m} · ${n} req` })))));
  return el("div", { class: "detail" }, parts);
}

async function toggleDetail(x, anchor, makeHost) {
  const key = sessionKey(x);
  const next = anchor.nextElementSibling;
  if (next?.dataset.detailFor === key) {
    next.remove(); openRows.delete(key); anchor.setAttribute("aria-expanded", "false");
    return;
  }
  openRows.add(key);
  anchor.setAttribute("aria-expanded", "true");
  const host = makeHost(el("div", { class: "detail" }, el("span", { class: "muted", text: "Loading…" })));
  host.dataset.detailFor = key;
  anchor.after(host);
  const d = await loadDetail(x);
  host.replaceChildren(makeHost(detailPanel(x, d)).firstChild || detailPanel(x, d));
}

function renderTopSessions(sessions) {
  const list = [...sessions].sort((a, b) => sessionMetric(b) - sessionMetric(a)).slice(0, 12);
  const total = sessions.reduce((s, x) => s + sessionMetric(x), 0);
  const topSum = list.reduce((s, x) => s + sessionMetric(x), 0);
  const max = Math.max(...list.map(sessionMetric), 1);
  $("top-sub").textContent = sessions.length
    ? `top ${list.length} of ${exact.format(sessions.length)} sessions = ${total ? Math.round((topSum / total) * 100) : 0}% of ${metric().unit}, last ${range().label} · click a row to see what happened`
    : "";
  const rows = [];
  list.forEach((x, i) => {
    const g = groupOfSession(x);
    const v = sessionMetric(x);
    const row = el("div", { class: "top-row", role: "button", tabindex: "0", "aria-expanded": "false" },
      el("div", { class: "rank", text: String(i + 1) }),
      el("div", {}, el("div", { class: "t", text: x.title || "(untitled)" }),
        el("div", { class: "sub" }, el("span", { class: "chip" }, swatch(g.color), g.label), el("span", { text: x.project || "—" }),
          x.branch ? el("span", { text: `⎇ ${x.branch}` }) : null, el("span", { text: `${hours(x.active_min)} active` }),
          el("span", { text: whenFmt.format(new Date(x.last)) }))),
      el("div", { class: "bar", "aria-hidden": "true" }, el("span", { bg: g.color, "width%": `${(v / max) * 100}%` })),
      el("div", { class: "val" }, el("b", { title: exact.format(v), text: fmt(v) }), el("span", { text: total ? `${((v / total) * 100).toFixed(1)}% of range` : "" })));
    const open = () => toggleDetail(x, row, (panel) => { const w = el("div", {}); w.append(panel); return w; });
    row.addEventListener("click", open);
    row.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); open(); } });
    rows.push(row);
  });
  $("top-list").replaceChildren(...(rows.length ? rows : [el("div", { class: "empty", text: "No sessions in this tab and range." })]));
  // Re-open panels that were open before a live refresh.
  list.forEach((x, i) => { if (openRows.has(sessionKey(x))) { openRows.delete(sessionKey(x)); rows[i].click(); } });
}

function renderWork(now) {
  const sessions = sessionsInRange(now);
  renderTopSessions(sessions);
  renderWorkstreams(sessions);
  renderProjects(sessions);
  renderSessions(sessions);
}

function render() {
  if (!data) return;
  const now = new Date();
  const t = tab();
  const rows = data.rows.filter(t.match);
  renderTabs(data.rows);
  renderSeg("metric", METRICS, "metric");
  renderSeg("burn", BURNS, "burn");
  renderSeg("range", RANGES, "range");
  const groupKeys = [...new Set(rows.map((r) => groupOf(r).key))];
  renderHero(rows, now, groupKeys);
  renderBars(rows, now);
  renderGroups(rows, now);
  renderWork(now);
  renderModels(rows, now);
  renderSources(data.rows, now);
}

// ---------- live updates ----------

function tick() {
  const live = $("live");
  if (!data) return;
  const age = Math.round((Date.now() - data.generated) / 1000);
  const stale = Date.now() - lastOk > POLL_MS * 3 || age > data.meta.interval * 3;
  live.className = `live ${stale ? "stale" : "ok"}`;
  $("live-text").textContent = stale ? `stale · updated ${age}s ago` : `live · updated ${age}s ago`;
  $("subtitle").textContent = `${data.meta.tz} · since ${dayFmt.format(new Date(data.meta.since))} · data from ${hourFmt.format(data.generated)}`;
}

async function poll() {
  try {
    const res = await fetch("/api/usage", { headers: etag ? { "If-None-Match": etag } : {}, cache: "no-store" });
    lastOk = Date.now();
    if (res.status === 200) {
      const body = await res.json();
      if (!body.rows) {
        $("notice").hidden = false;
        $("notice").textContent = "Collecting usage for the first time… this page fills in automatically.";
        return;
      }
      etag = res.headers.get("ETag");
      data = parse(body);
      $("notice").hidden = true;
      render();
    }
  } catch {
    $("notice").hidden = false;
    $("notice").textContent = "Can't reach the dashboard server. Is the ai-usage LaunchAgent running?";
  }
  tick();
}

$("refresh").addEventListener("click", async () => {
  const btn = $("refresh");
  btn.disabled = true;
  try {
    await fetch("/api/refresh", { method: "POST", headers: { "X-Requested-With": "ai-usage" } });
    for (let i = 0; i < 12; i++) {
      await new Promise((r) => setTimeout(r, 1000));
      const before = etag;
      await poll();
      if (etag !== before) break;
    }
  } finally {
    btn.disabled = false;
  }
});

let resizeTimer;
window.addEventListener("resize", () => { clearTimeout(resizeTimer); resizeTimer = setTimeout(render, 150); });
document.addEventListener("visibilitychange", () => { if (!document.hidden) poll(); });

$("session-filter").addEventListener("input", (e) => { sessionFilter = e.target.value; if (data) renderSessions(sessionsInRange(new Date())); });

const sections = ["overview", "top-sessions", "breakdown", "profiles", "work", "models", "sources"];
const navObserver = new IntersectionObserver((entries) => {
  for (const e of entries) {
    if (!e.isIntersecting) continue;
    document.querySelectorAll(".nav-item").forEach((a) => a.classList.toggle("active", a.getAttribute("href") === `#${e.target.id}`));
  }
}, { rootMargin: "-40% 0px -55% 0px" });
sections.forEach((id) => navObserver.observe($(id)));

poll();
setInterval(poll, POLL_MS);
setInterval(tick, 1000);
