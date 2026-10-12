"use strict";

// Talks to the daemon through POST api/{cmd}, relative to this page's
// token URL.
async function api(cmd, body = {}) {
  const r = await fetch("api/" + cmd, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}

const $ = (id) => document.getElementById(id);
const form = $("settings");
let pairing = null; // {expires, known: Set of device ids}
let pollTimer = null;

function showError(err) {
  const el = $("error");
  el.textContent = err ? String(err.message || err) : "";
  el.hidden = !err;
}

function lastSeen(d) {
  if (d.connected) return "Connected" + (d.addr ? " from " + d.addr.replace(/:\d+$/, "") : "");
  const t = Date.parse(d.last_seen || "");
  if (!t) return "Never connected";
  const min = Math.round((Date.now() - t) / 60000);
  if (min < 1) return "Seen just now";
  if (min < 60) return "Seen " + min + " min ago";
  if (min < 1440) return "Seen " + Math.round(min / 60) + " h ago";
  return "Seen " + new Date(t).toLocaleDateString();
}

function renderDevices(devices) {
  const list = $("devices");
  list.replaceChildren();
  $("nodevices").hidden = devices.length > 0;
  for (const d of devices) {
    const li = document.createElement("li");
    li.className = "card device" + (d.connected ? " connected" : "");
    const dot = document.createElement("span");
    dot.className = "dot";
    const info = document.createElement("div");
    info.className = "info";
    const name = document.createElement("div");
    name.className = "name";
    name.textContent = d.name;
    const detail = document.createElement("div");
    detail.className = "detail";
    detail.textContent = lastSeen(d) + (d.pending ? " · " + d.pending + " waiting to be sent" : "");
    info.append(name, detail);

    const unpair = document.createElement("button");
    unpair.type = "button";
    unpair.className = "danger";
    unpair.textContent = "Unpair";
    let armed = null;
    unpair.addEventListener("click", async () => {
      if (!armed) { // ask once more, without a dialog
        unpair.textContent = "Really unpair?";
        unpair.classList.add("confirm");
        armed = setTimeout(() => {
          armed = null;
          unpair.textContent = "Unpair";
          unpair.classList.remove("confirm");
        }, 3000);
        return;
      }
      clearTimeout(armed);
      unpair.disabled = true;
      try {
        await api("unpair", { id: d.id });
        refresh();
      } catch (e) {
        showError(e);
        unpair.disabled = false;
      }
    });
    li.append(dot, info, unpair);
    list.append(li);
  }
}

function summary(st) {
  const n = st.devices.length;
  const c = st.devices.filter((d) => d.connected).length;
  if (n === 0) return st.name + " · no phone paired";
  return st.name + " · " + c + " of " + n + " connected";
}

async function refresh() {
  try {
    const st = await api("status");
    showError(null);
    $("summary").textContent = summary(st);
    renderDevices(st.devices || (st.devices = []));
    const host = (st.files || "").replace(/^http:\/\//, "");
    $("filesinfo").hidden = !host;
    $("davkde").textContent = "webdav://" + host;
    $("davgtk").textContent = "dav://" + host;
    if (pairing) {
      const fresh = st.devices.find((d) => !pairing.known.has(d.id));
      if (fresh) {
        stopPairing();
        $("summary").textContent = "Paired with " + fresh.name;
      }
    }
  } catch (e) {
    // The page stops when it isn't used; a new one has a new address.
    showError(e instanceof TypeError
      ? "This page has expired. Run pocketlink settings again."
      : "The pocketlink daemon isn't answering. " + e.message);
  }
}

function fill(reply) {
  const cfg = reply.config || {};
  const fixed = new Set(reply.overridden || []);
  for (const el of form.elements) {
    if (!el.name || !(el.name in cfg)) continue;
    if (document.activeElement === el) continue; // don't overwrite typing
    if (el.type === "checkbox") el.checked = !!cfg[el.name];
    else el.value = cfg[el.name] ?? "";
    el.disabled = fixed.has(el.name);
  }
  $("volumeout").textContent = form.elements.call_volume.value + "%";
  $("volumerow").hidden = cfg.ring_action !== "lower" && cfg.talk_action !== "lower";
  $("flags").hidden = fixed.size === 0;
  $("flags").textContent = "Set by daemon flags: " + [...fixed].join(", ");
}

async function save(el) {
  const value = el.type === "checkbox" ? String(el.checked) : el.value.trim();
  try {
    const reply = await api("set", { key: el.name, value });
    showError(null);
    fill(reply);
    if (reply.restart) $("restart").hidden = false;
    el.classList.add("saved");
    setTimeout(() => el.classList.remove("saved"), 1200);
  } catch (e) {
    showError(e);
    fill(await api("config"));
  }
}

form.addEventListener("change", (ev) => {
  if (ev.target.name) save(ev.target);
});
form.addEventListener("input", (ev) => {
  if (ev.target.name === "call_volume") $("volumeout").textContent = ev.target.value + "%";
});
form.addEventListener("submit", (ev) => ev.preventDefault());

async function startPairing() {
  try {
    const st = await api("status");
    const reply = await api("pair");
    pairing = { expires: Date.parse(reply.expires) || Date.now() + 300000, known: new Set(st.devices.map((d) => d.id)) };
    $("qr").src = reply.qr;
    $("pairuri").textContent = reply.uri;
    $("pairing").hidden = false;
    $("pair").disabled = true;
    tick();
    schedule();
  } catch (e) {
    showError(e);
  }
}

function stopPairing() {
  pairing = null;
  $("pairing").hidden = true;
  $("pair").disabled = false;
  schedule();
}

function tick() {
  if (!pairing) return;
  const left = Math.max(0, Math.round((pairing.expires - Date.now()) / 1000));
  if (left === 0) return stopPairing();
  $("countdown").textContent = Math.floor(left / 60) + ":" + String(left % 60).padStart(2, "0");
  setTimeout(tick, 1000);
}

// Faster while pairing, so a new phone shows up promptly.
function schedule() {
  clearInterval(pollTimer);
  pollTimer = setInterval(() => {
    if (!document.hidden) refresh();
  }, pairing ? 1500 : 5000);
}

$("pair").addEventListener("click", startPairing);
$("cancelpair").addEventListener("click", stopPairing);

// Closing the tab stops the page right away. keepalive lets the request
// outlive the page.
window.addEventListener("pagehide", () => {
  fetch("api/close", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}", keepalive: true });
});

refresh();
api("config").then(fill, showError);
schedule();
