// ai-usage dashboard enhancements: tooltips, the cumulative crosshair, live refresh, session
// filtering, and the theme toggle. The page is server-rendered and works without this script.
// @ts-check

const POLL_MS = 10_000;
const HOUR = 3_600_000;
const $ = (/** @type {string} */ id) => document.getElementById(id);

// ---------- theme ----------
const THEMES = ["auto", "light", "dark"];
function applyTheme(/** @type {string} */ t) {
  if (t === "auto") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", t);
}
function storedTheme() {
  try { return localStorage.getItem("ai-usage-theme") || "auto"; } catch { return "auto"; }
}
applyTheme(storedTheme());
document.addEventListener("click", (e) => {
  const btn = /** @type {HTMLElement} */ (e.target)?.closest?.("#theme");
  if (!btn) return;
  const next = THEMES[(THEMES.indexOf(storedTheme()) + 1) % THEMES.length];
  try { localStorage.setItem("ai-usage-theme", next); } catch { /* private mode */ }
  applyTheme(next);
  btn.setAttribute("title", `Theme: ${next}`);
});

// ---------- tooltip (textContent only; data comes from data-tip JSON) ----------
const tipEl = $("tip");
const fmt = new Intl.NumberFormat("en");
const hourFmt = new Intl.DateTimeFormat("en", { weekday: "short", month: "short", day: "numeric", hour: "numeric" });

/** @param {MouseEvent} evt @param {string} head @param {Array<[string, string, number]>} rows */
function showTip(evt, head, rows) {
  if (!tipEl) return;
  const h = document.createElement("div");
  h.className = "t-head";
  h.textContent = head;
  const kids = [h];
  for (const [label, value, slot] of rows) {
    const r = document.createElement("div");
    r.className = "t-row";
    const l = document.createElement("span");
    if (slot >= 0) {
      const sw = document.createElement("span");
      sw.className = `swatch s${slot}`;
      l.append(sw);
    }
    l.append(label);
    const v = document.createElement("b");
    v.textContent = value;
    r.append(l, v);
    kids.push(r);
  }
  tipEl.replaceChildren(...kids);
  tipEl.hidden = false;
  const pad = 14, rect = tipEl.getBoundingClientRect();
  tipEl.style.left = `${Math.min(evt.clientX + pad, innerWidth - rect.width - 8)}px`;
  tipEl.style.top = `${Math.min(evt.clientY + pad, innerHeight - rect.height - 8)}px`;
}
const hideTip = () => { if (tipEl) tipEl.hidden = true; };

document.addEventListener("mousemove", (evt) => {
  const target = /** @type {Element} */ (evt.target);
  const cum = target.closest?.("svg[data-cum]");
  if (cum && target.classList.contains("cum-hit")) return cumHover(/** @type {SVGSVGElement} */ (cum), evt);
  const t = target.closest?.("[data-tip]");
  if (!t) return hideTip();
  try {
    const d = JSON.parse(t.getAttribute("data-tip") || "{}");
    showTip(evt, String(d.h ?? ""), Array.isArray(d.r) ? d.r : []);
  } catch { hideTip(); }
});
document.addEventListener("mouseleave", hideTip);

// ---------- cumulative crosshair ----------
/** @param {SVGSVGElement} svg @param {MouseEvent} evt */
function cumHover(svg, evt) {
  const d = JSON.parse(svg.getAttribute("data-cum") || "{}");
  const box = svg.getBoundingClientRect();
  const scale = d.W / box.width;
  const plotW = d.W - d.L - d.R;
  const i = Math.min(Math.max(Math.round((((evt.clientX - box.left) * scale - d.L) / plotW) * d.hours), 0), d.hours);
  const at = new Date(d.start + i * HOUR);
  const past = i <= d.cum.length;
  const v = past ? (i === 0 ? 0 : d.cum[i - 1]) : d.run + d.rate * ((+at - d.now) / HOUR);
  const x = d.L + (i / d.hours) * plotW;
  const y = d.H - d.B - (v / d.ymax) * (d.H - d.T - d.B);
  const xh = svg.querySelector(".xh"), dot = svg.querySelector(".xh-dot");
  xh?.setAttribute("x1", String(x)); xh?.setAttribute("x2", String(x)); xh?.setAttribute("visibility", "visible");
  dot?.setAttribute("cx", String(x)); dot?.setAttribute("cy", String(y)); dot?.setAttribute("visibility", "visible");
  /** @type {Array<[string, string, number]>} */
  const rows = [[past ? "Recorded" : "Projected", fmt.format(Math.round(v)), -1]];
  if (past && i > 0) rows.push(["That hour", fmt.format(Math.round(d.by[i - 1] || 0)), -1]);
  if (d.budget > 0) rows.push(["Budget", `${Math.round((v / d.budget) * 100)}%`, -1]);
  showTip(evt, hourFmt.format(at), rows);
}
document.addEventListener("mouseout", (evt) => {
  const t = /** @type {Element} */ (evt.target);
  if (!t.classList?.contains("cum-hit")) return;
  const svg = t.closest("svg");
  svg?.querySelector(".xh")?.setAttribute("visibility", "hidden");
  svg?.querySelector(".xh-dot")?.setAttribute("visibility", "hidden");
  hideTip();
});

// ---------- session filter (client-side over data-search) ----------
function applyFilter() {
  const input = /** @type {HTMLInputElement|null} */ ($("session-filter"));
  if (!input) return;
  const q = input.value.trim().toLowerCase();
  for (const tr of document.querySelectorAll("#sessions-table tbody tr[data-search]")) {
    tr.classList.toggle("hidden", !!q && !(tr.getAttribute("data-search") || "").includes(q));
  }
}
document.addEventListener("input", (e) => { if (/** @type {Element} */ (e.target).id === "session-filter") applyFilter(); });

// ---------- live refresh: poll the ETag, swap <main> when the snapshot changes ----------
let etag = document.body.dataset.etag || "";
let lastOk = Date.now();
let generated = Number(document.body.dataset.generated || Date.now());
let interval = Number(document.body.dataset.interval || 30);

async function swap() {
  const filter = /** @type {HTMLInputElement|null} */ ($("session-filter"));
  const q = filter?.value ?? "", focused = document.activeElement === filter;
  const res = await fetch(location.href, { headers: { "X-Partial": "1" }, cache: "no-store" });
  if (!res.ok) return;
  const main = $("main");
  if (!main) return;
  main.innerHTML = await res.text(); // same-origin, server-escaped HTML
  const f = /** @type {HTMLInputElement|null} */ ($("session-filter"));
  if (f) { f.value = q; if (focused) f.focus(); }
  applyFilter();
}

async function poll() {
  if (!$("main")) return;
  try {
    const res = await fetch("/api/etag", { cache: "no-store" });
    const d = await res.json();
    lastOk = Date.now();
    if (d.generated) generated = d.generated;
    if (d.interval) interval = d.interval;
    if (d.etag && d.etag !== etag) { etag = d.etag; await swap(); }
    const n = $("notice");
    if (n) n.hidden = true;
  } catch {
    const n = $("notice");
    if (n) { n.hidden = false; n.textContent = "Can't reach the dashboard server. Is 'ai-usage serve' still running?"; }
  }
  tick();
}

function tick() {
  const live = $("live"), text = $("live-text");
  if (!live || !text) return;
  const age = Math.round((Date.now() - generated) / 1000);
  const stale = Date.now() - lastOk > POLL_MS * 3 || age > interval * 3;
  live.className = `live ${stale ? "stale" : "ok"}`;
  text.textContent = `${stale ? "stale" : "live"} · updated ${age}s ago`;
}

document.addEventListener("click", async (e) => {
  const btn = /** @type {HTMLButtonElement|null} */ (/** @type {Element} */ (e.target).closest?.("#refresh"));
  if (!btn) return;
  btn.disabled = true;
  try {
    await fetch("/api/refresh", { method: "POST", headers: { "X-Requested-With": "ai-usage" } });
    const before = etag;
    for (let i = 0; i < 15 && etag === before; i++) {
      await new Promise((r) => setTimeout(r, 1000));
      await poll();
    }
  } finally {
    const b = /** @type {HTMLButtonElement|null} */ ($("refresh"));
    if (b) b.disabled = false;
  }
});

// ---------- sidebar highlights the section in view ----------
const navObserver = new IntersectionObserver((entries) => {
  for (const e of entries) {
    if (!e.isIntersecting) continue;
    document.querySelectorAll(".nav-item").forEach((a) => a.classList.toggle("active", a.getAttribute("href") === `#${e.target.id}`));
  }
}, { rootMargin: "-40% 0px -55% 0px" });
function observe() {
  for (const a of document.querySelectorAll(".nav-item")) {
    const el = document.querySelector(a.getAttribute("href") || "");
    if (el) navObserver.observe(el);
  }
}

if ($("main")) {
  observe();
  applyFilter();
  setInterval(poll, POLL_MS);
  setInterval(tick, 1000);
  document.addEventListener("visibilitychange", () => { if (!document.hidden) poll(); });
  tick();
}
