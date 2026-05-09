// Tunnelsmith UI: fetches /api/scoreboard and renders the three tables.
// Plain DOM, no framework, so the embedded asset stays small.

const $ = (sel) => document.querySelector(sel);

function fmtTime(s) {
  if (!s) return "";
  const t = new Date(s);
  if (isNaN(t.getTime()) || t.getTime() === 0) return "";
  return t.toISOString().replace("T", " ").replace(/\.\d+Z$/, "Z");
}

function relTime(s) {
  if (!s) return "";
  const t = new Date(s);
  if (isNaN(t.getTime()) || t.getTime() === 0) return "";
  const diff = (t.getTime() - Date.now()) / 1000;
  if (Math.abs(diff) < 60) return diff > 0 ? `in ${Math.round(diff)}s` : `${Math.round(-diff)}s ago`;
  if (Math.abs(diff) < 3600) return diff > 0 ? `in ${Math.round(diff/60)}m` : `${Math.round(-diff/60)}m ago`;
  return diff > 0 ? `in ${Math.round(diff/3600)}h` : `${Math.round(-diff/3600)}h ago`;
}

function scoreClass(score) {
  if (score > 0.5) return "score-pos";
  if (score < -0.5) return "score-neg";
  return "";
}

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (k === "class") node.className = v;
      else if (k.startsWith("on") && typeof v === "function") node.addEventListener(k.slice(2), v);
      else node.setAttribute(k, v);
    }
  }
  for (const c of children) {
    if (c == null) continue;
    node.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
  }
  return node;
}

async function fetchJSON(url, opts) {
  const r = await fetch(url, opts);
  const text = await r.text();
  if (!r.ok) throw new Error(text || `${r.status} ${r.statusText}`);
  return text ? JSON.parse(text) : null;
}

function renderPool(state) {
  const tbody = $("#pool tbody");
  tbody.replaceChildren();
  const cooled = state.cooled_by_upstream || {};
  for (const id of state.pool_ids || []) {
    const n = cooled[id] || 0;
    tbody.appendChild(el("tr", null,
      el("td", null, id),
      el("td", null, String(n))));
  }
  $("#cascade").textContent = String(state.cascade_active || 0);
}

function renderScoreboard(state) {
  const tbody = $("#scoreboard tbody");
  tbody.replaceChildren();
  const entries = (state.entries || []).slice();
  entries.sort((a, b) => {
    if (a.host !== b.host) return a.host.localeCompare(b.host);
    return b.score - a.score;
  });
  for (const e of entries) {
    const cls = scoreClass(e.score);
    const cooledNow = e.cooldown_until && new Date(e.cooldown_until).getTime() > Date.now();
    tbody.appendChild(el("tr", null,
      el("td", null, e.host),
      el("td", null, e.upstream_id),
      el("td", { class: cls }, e.score.toFixed(2)),
      el("td", { class: cooledNow ? "cool" : "" }, cooledNow ? `${fmtTime(e.cooldown_until)} (${relTime(e.cooldown_until)})` : ""),
      el("td", null, fmtTime(e.last_seen)),
      el("td", null, String(e.global_success)),
      el("td", null, String(e.global_failure)),
      el("td", null, el("button", { onclick: () => onForget(e.host) }, "Forget"))));
  }
}

function renderForces(state) {
  const tbody = $("#forces tbody");
  tbody.replaceChildren();
  for (const f of state.forces || []) {
    tbody.appendChild(el("tr", null,
      el("td", null, f.host),
      el("td", null, f.upstream_id),
      el("td", null, `${fmtTime(f.until)} (${relTime(f.until)})`),
      el("td", null, el("button", { onclick: () => onUnforce(f.host) }, "Clear"))));
  }
}

// refreshSeq is bumped on every refresh() call. Any in-flight refresh
// whose token does not match the latest at resolution time skips its
// DOM updates so an older response cannot repaint stale state on top
// of a newer one (the 5s poll, the manual button, and post-action
// refreshes can all race).
let refreshSeq = 0;

async function refresh() {
  const seq = ++refreshSeq;
  try {
    const state = await fetchJSON("/api/scoreboard");
    if (seq !== refreshSeq) return;
    $("#generated").textContent = `Updated ${fmtTime(state.generated_at)}`;
    renderPool(state);
    renderScoreboard(state);
    renderForces(state);
  } catch (err) {
    if (seq !== refreshSeq) return;
    $("#generated").textContent = `Error: ${err.message}`;
  }
}

// runAction wraps an admin POST so failures land in the status line
// instead of the console. Without this, a network blip or 5xx on
// Forget/Unforce/Reset just looks like the click did nothing.
async function runAction(request) {
  try {
    await request();
    await refresh();
  } catch (err) {
    $("#generated").textContent = `Error: ${err.message}`;
  }
}

async function onForget(host) {
  if (!confirm(`Forget all scoreboard state for ${host}?`)) return;
  await runAction(() => fetchJSON("/api/forget", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ host }),
  }));
}

async function onUnforce(host) {
  await runAction(() => fetchJSON("/api/force/clear", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ host }),
  }));
}

async function onReset() {
  if (!confirm("Reset every entry, every cascade, every force pin? This cannot be undone.")) return;
  await runAction(() => fetchJSON("/api/reset", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: "{}",
  }));
}

async function onForceSubmit(e) {
  e.preventDefault();
  const form = e.target;
  const data = {
    host: form.host.value.trim(),
    upstream_id: form.upstream_id.value.trim(),
    duration: form.duration.value.trim(),
  };
  const msg = $("#force-msg");
  msg.className = "msg";
  msg.textContent = "Pinning...";
  try {
    await fetchJSON("/api/force", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(data),
    });
    msg.className = "msg ok";
    msg.textContent = `Pinned ${data.host} -> ${data.upstream_id} for ${data.duration}.`;
    form.host.value = "";
    refresh();
  } catch (err) {
    msg.className = "msg err";
    msg.textContent = err.message;
  }
}

document.addEventListener("DOMContentLoaded", () => {
  $("#refresh").addEventListener("click", refresh);
  $("#reset").addEventListener("click", onReset);
  $("#force-form").addEventListener("submit", onForceSubmit);
  refresh();
  setInterval(refresh, 5000);
});
