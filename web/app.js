// pixyd UI: plain JavaScript, no build step. State arrives over the
// /api/events stream; changes go through the JSON API.

// ---------------------------------------------------------------------
// Helpers

const $ = (id) => document.getElementById(id);

/** Creates an element: h("p", { class: "x", text: "hi" }, child…) */
function h(tag, props = {}, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (value === undefined || value === null || value === false) continue;
    if (key === "class") el.className = value;
    else if (key === "text") el.textContent = value;
    else if (key.startsWith("on")) el.addEventListener(key.slice(2), value);
    else if (key in el && typeof value !== "string") el[key] = value;
    else el.setAttribute(key, value === true ? "" : value);
  }
  el.append(...children.filter((c) => c !== null && c !== undefined));
  return el;
}

function icon(name, cls = "icon") {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("class", cls);
  svg.setAttribute("aria-hidden", "true");
  const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
  use.setAttribute("href", `icons.svg#${name}`);
  svg.append(use);
  return svg;
}

function setIcon(svg, name) {
  svg.querySelector("use").setAttribute("href", `icons.svg#${name}`);
}

async function api(method, path, body) {
  const init = { method, headers: {} };
  if (body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  try {
    const res = await fetch(path, init);
    let data = null;
    try {
      data = await res.json();
    } catch {
      // not JSON
    }
    return { ok: res.ok, status: res.status, data };
  } catch (err) {
    return { ok: false, status: 0, data: { error: "offline", message: `pixyd is not reachable (${err.message})` } };
  }
}

function toast(message) {
  const el = h("div", { class: "toast", role: "status", text: message });
  $("toasts").append(el);
  setTimeout(() => el.remove(), 6000);
}

const capitalize = (s) => s.charAt(0).toUpperCase() + s.slice(1);

const storage = {
  get(key) {
    try {
      return localStorage.getItem(key);
    } catch {
      return null;
    }
  },
  set(key, value) {
    try {
      localStorage.setItem(key, value);
    } catch {
      // private window or blocked storage: not worth bothering anyone
    }
  },
};

// ---------------------------------------------------------------------
// State from the daemon

const app = {
  state: null, // last "state" event
  online: false, // event stream connected
  modeBusy: false,
};

function connectEvents() {
  const events = new EventSource("/api/events");
  events.addEventListener("open", () => {
    app.online = true;
    renderState();
  });
  events.addEventListener("error", () => {
    app.online = false;
    renderState();
  });
  events.addEventListener("state", (e) => {
    app.online = true;
    app.state = JSON.parse(e.data);
    renderState();
  });
  events.addEventListener("controls", (e) => renderControls(JSON.parse(e.data)));
}

function renderState() {
  const s = app.state;
  const conn = $("conn");
  const banner = $("banner");
  banner.hidden = true;
  banner.className = "banner";

  if (!app.online) {
    conn.className = "chip danger";
    conn.textContent = "pixyd offline";
    banner.hidden = false;
    banner.textContent = "Lost the connection to pixyd. Reconnecting…";
  } else if (!s || !s.connected) {
    conn.className = "chip danger";
    conn.textContent = "Disconnected";
    banner.hidden = false;
    banner.className = "banner warning";
    banner.textContent = "Waiting for the EMEET PIXY. Plug it in; pixyd picks it up on its own.";
  } else if (s.tracking.error === "permission") {
    conn.className = "chip warning";
    conn.textContent = "No HID access";
    banner.hidden = false;
    banner.textContent = `${s.tracking.message}. Image and PTZ controls still work.`;
  } else {
    conn.className = "chip success";
    conn.textContent = "Connected";
  }

  const cam = s && s.camera;
  $("device").textContent = cam
    ? [cam.name, cam.usb && `USB ${cam.usb}`, cam.speed, cam.video].filter(Boolean).join(" · ")
    : "";

  renderMode();
  preview.sync();
}

// ---------------------------------------------------------------------
// Mode

const MODE_HINTS = {
  privacy: "Privacy mode: the lens is closed.",
};

function renderMode() {
  const t = app.state && app.online ? app.state.tracking : null;
  const failed = !t || "error" in t && !("mode" in t);
  for (const b of $("modes").querySelectorAll("button")) {
    const isMode = !failed && t.mode === b.dataset.mode;
    b.classList.toggle("active", isMode && t.confirmed);
    b.classList.toggle("guess", isMode && !t.confirmed);
    b.setAttribute("aria-pressed", String(isMode));
    b.disabled = failed || app.modeBusy;
  }

  const note = $("mode-note");
  note.className = "note";
  let text = "";
  if (app.modeBusy) {
    text = "Switching…";
  } else if (failed) {
    text = t ? t.message : "";
    if (t) note.className = "note danger";
  } else if (t.pending) {
    text =
      `${capitalize(t.pending)} is applied as soon as an app streams video: ` +
      "an idle camera can't switch between standard and tracking.";
    note.className = "note warning";
  } else if (t.idle) {
    text =
      "No app is streaming video, so the camera can't tell standard from tracking" +
      (t.mode ? ` (last known: ${t.mode}).` : ".");
  } else {
    text = MODE_HINTS[t.mode] || "";
  }
  note.textContent = text;
  note.hidden = !text;
}

async function setMode(mode) {
  app.modeBusy = true;
  renderMode();
  const res = await api("PUT", "/api/mode", { mode });
  app.modeBusy = false;
  if (res.data && "mode" in res.data && app.state) {
    const { error, message, ...tracking } = res.data;
    app.state.tracking = tracking;
  }
  renderMode();
  if (!res.ok) toast(res.data?.message || `Switching to ${mode} failed (HTTP ${res.status})`);
}

for (const b of $("modes").querySelectorAll("button")) {
  b.addEventListener("click", () => setMode(b.dataset.mode));
}

// ---------------------------------------------------------------------
// Preview: the page streams the camera itself with getUserMedia, and lets
// go of it whenever it is hidden so other apps can have it.

const preview = {
  stream: null,
  starting: false,
  generation: 0,
  status: "idle", // idle | starting | live | paused | hidden | busy | denied | missing | unsupported | error | disconnected
  detail: "",
  paused: storage.get("pixyd.preview") === "paused",
  lastRetry: 0,

  wanted() {
    return !this.paused && !document.hidden && app.online && app.state?.connected;
  },

  // stuck states wait for the user: retrying a denied or missing camera
  // on every state change would only repeat the failure (or the prompt).
  stuck() {
    return ["denied", "missing", "error", "unsupported"].includes(this.status);
  },

  // sync starts or stops the preview to match the page and the camera.
  sync() {
    clearTimeout(this.retryTimer);
    if (!this.wanted()) {
      this.stop(this.paused ? "paused" : document.hidden ? "hidden" : "disconnected");
    } else if (!this.stream && !this.starting && !this.stuck()) {
      // Don't hammer a camera another app holds; retry every few seconds.
      const wait = this.lastRetry + 3000 - Date.now();
      if (this.status === "busy" && wait > 0) {
        this.retryTimer = setTimeout(() => this.sync(), wait);
      } else {
        this.start();
        return;
      }
    }
    this.render();
  },

  async start() {
    if (!navigator.mediaDevices?.getUserMedia) {
      this.status = "unsupported";
      this.render();
      return;
    }
    const gen = ++this.generation;
    this.starting = true;
    this.status = "starting";
    this.lastRetry = Date.now();
    this.render();
    try {
      let device = await findPixy();
      if (!device) {
        // Labels stay empty until the page may use a camera; asking once
        // reveals them. The browser may open its default camera briefly.
        const probe = await navigator.mediaDevices.getUserMedia({ video: true });
        probe.getTracks().forEach((t) => t.stop());
        device = await findPixy();
      }
      if (gen !== this.generation) return;
      if (!device) {
        this.status = "missing";
        return;
      }
      const stream = await navigator.mediaDevices.getUserMedia({
        audio: false,
        video: {
          deviceId: { exact: device.deviceId },
          width: { ideal: 1280 },
          height: { ideal: 720 },
          frameRate: { ideal: 30 },
        },
      });
      if (gen !== this.generation || !this.wanted()) {
        stream.getTracks().forEach((t) => t.stop());
        return;
      }
      this.stream = stream;
      for (const track of stream.getVideoTracks()) {
        track.addEventListener("ended", () => {
          if (this.stream !== stream) return;
          this.stop("error");
          this.detail = "The camera stopped sending video.";
          this.render();
        });
      }
      $("video").srcObject = stream;
      this.status = "live";
    } catch (err) {
      if (gen !== this.generation) return;
      this.detail = err.message || String(err);
      this.status =
        {
          NotReadableError: "busy",
          TrackStartError: "busy",
          AbortError: "busy",
          NotAllowedError: "denied",
          SecurityError: "denied",
          NotFoundError: "missing",
          OverconstrainedError: "missing",
        }[err.name] || "error";
    } finally {
      if (gen === this.generation) {
        this.starting = false;
        if (this.status === "busy") this.sync(); // schedules the retry
        this.render();
      }
    }
  },

  stop(status) {
    this.generation++;
    this.starting = false;
    if (this.stream) {
      this.stream.getTracks().forEach((t) => t.stop());
      this.stream = null;
    }
    $("video").srcObject = null;
    this.status = status;
  },

  render() {
    const overlay = $("overlay");
    const text = $("overlay-text");
    const ico = $("overlay-icon");
    const users = (app.state?.videoUsers || []).map((u) => `${u.name} (pid ${u.pid})`);
    const privacy = app.state?.tracking?.mode === "privacy" && app.state.tracking.confirmed;

    let msg = "";
    let tone = "";
    let iconName = "camera";
    switch (this.status) {
      case "starting":
        msg = "Starting preview…";
        break;
      case "live":
        if (privacy) {
          msg = "Privacy mode: the lens is closed.";
          iconName = "eye-off";
        }
        break;
      case "paused":
        msg = "Preview paused. The camera is free for other apps.";
        iconName = "pause";
        break;
      case "hidden":
        msg = "Preview stopped while the page is hidden.";
        iconName = "pause";
        break;
      case "disconnected":
        msg = app.online ? "The EMEET PIXY is not connected." : "pixyd is not reachable.";
        iconName = "unplug";
        tone = "warning";
        break;
      case "busy":
        msg = users.length
          ? `The camera is in use by ${users.join(", ")}. The preview starts once it is free.`
          : "The camera is in use by another app. The preview starts once it is free.";
        iconName = "circle-alert";
        tone = "warning";
        break;
      case "denied":
        msg = "The browser doesn't let this page use the camera. Allow camera access for this site and reload.";
        iconName = "circle-alert";
        tone = "danger";
        break;
      case "missing":
        msg = "The browser doesn't list the EMEET PIXY among its cameras.";
        iconName = "circle-alert";
        tone = "danger";
        break;
      case "unsupported":
        msg = "This browser can't show camera video here.";
        iconName = "circle-alert";
        tone = "danger";
        break;
      case "error":
        msg = `The preview failed: ${this.detail}`;
        iconName = "circle-alert";
        tone = "danger";
        break;
    }
    overlay.hidden = !msg;
    overlay.className = `overlay ${tone}`;
    text.textContent = msg;
    setIcon(ico, iconName);

    const toggle = $("preview-toggle");
    const retry = this.stuck() && !this.paused;
    setIcon(toggle.querySelector("svg"), this.paused || retry ? "play" : "pause");
    toggle.querySelector("span").textContent = retry ? "Retry preview" : this.paused ? "Start preview" : "Pause preview";
  },
};

async function findPixy() {
  const devices = await navigator.mediaDevices.enumerateDevices();
  const cam = app.state?.camera;
  const names = [cam?.card, cam?.name].filter(Boolean).map((n) => n.toLowerCase());
  return devices.find((d) => {
    if (d.kind !== "videoinput" || !d.label) return false;
    const label = d.label.toLowerCase();
    // Chrome appends "(328f:00c0)"; Firefox uses the V4L2 card name.
    return label.includes("328f:00c0") || names.some((n) => label.includes(n)) || label.includes("pixy");
  });
}

$("preview-toggle").addEventListener("click", () => {
  if (preview.stuck() && !preview.paused) {
    preview.status = "idle"; // retry
  } else {
    preview.paused = !preview.paused;
    storage.set("pixyd.preview", preview.paused ? "paused" : "on");
    if (!preview.paused) preview.status = "idle";
  }
  preview.sync();
});
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && preview.stuck()) preview.status = "idle"; // coming back retries
  preview.sync();
});
window.addEventListener("pagehide", () => preview.stop("hidden"));

// ---------------------------------------------------------------------
// Controls, generated from the V4L2 metadata

// Where each section goes: "center" is the strip under the preview.
const SECTIONS = [
  { id: "ptz", column: "center", title: "Pan, tilt & zoom", icon: "move", reset: "Center", resetIcon: "locate-fixed" },
  { id: "user", column: "left", title: "Image", icon: "sun-medium", reset: "Reset", resetIcon: "rotate-ccw" },
  { id: "camera", column: "right", title: "Exposure & focus", icon: "aperture", reset: "Reset", resetIcon: "rotate-ccw" },
  { id: "other", column: "left", title: "Other", icon: "aperture", reset: "Reset", resetIcon: "rotate-ccw" },
];

// The driver files these under image (user) controls, but they are about
// exposure; moving them also balances the two columns.
const SECTION_OF = {
  gain: "camera",
  backlight_compensation: "camera",
  power_line_frequency: "camera",
};

const LABELS = {
  pan_absolute: "Pan",
  tilt_absolute: "Tilt",
  zoom_absolute: "Zoom",
  white_balance_automatic: "Automatic white balance",
  white_balance_temperature: "White balance",
  auto_exposure: "Exposure mode",
  exposure_time_absolute: "Exposure time",
  focus_automatic_continuous: "Autofocus",
  focus_absolute: "Focus",
  power_line_frequency: "Anti-flicker",
};

// Manual controls and the automatic switches that make them inactive.
const MASTERS = {
  white_balance_temperature: ["white_balance_automatic", "white_balance_temperature_auto"],
  exposure_time_absolute: ["auto_exposure", "exposure_auto"],
  exposure_absolute: ["exposure_auto"],
  focus_absolute: ["focus_automatic_continuous", "focus_auto"],
};

const FORMATS = {
  pan_absolute: (v) => `${Math.round(v / 3600)}°`,
  tilt_absolute: (v) => `${Math.round(v / 3600)}°`,
  zoom_absolute: (v) => `${(v / 100).toFixed(2)}×`,
  white_balance_temperature: (v) => `${v} K`,
  exposure_time_absolute: (v) => `${+(v / 10).toFixed(1)} ms`, // 100 µs units
};

const SUPPORTED = new Set(["int", "bool", "menu", "intmenu", "button"]);

function label(c) {
  if (LABELS[c.name]) return LABELS[c.name];
  // "Power Line Frequency" → "Power line frequency"
  const words = c.label.replace(/, Absolute$/, "").split(" ");
  return words.map((w, i) => (i === 0 || /^[A-Z0-9]+$/.test(w) ? w : w.toLowerCase())).join(" ");
}

const format = (c, v) => (v === null || v === undefined ? "–" : FORMATS[c.name] ? FORMATS[c.name](v) : String(v));
const menuLabel = (item) => item.label.replace(/ Mode$/, "");
const settable = (c) => !c.flags.includes("read-only") && SUPPORTED.has(c.type);
function sectionOf(c) {
  if (/^(pan|tilt|zoom)_/.test(c.name)) return "ptz";
  if (SECTION_OF[c.name]) return SECTION_OF[c.name];
  return ["user", "camera"].includes(c.class) ? c.class : "other";
}

const columns = () => ({ left: $("controls-left"), right: $("controls-right"), center: $("ptz") });

function visible(c) {
  if (!SUPPORTED.has(c.type)) return false;
  if (c.type === "int" && c.min === c.max) return false; // e.g. zoom_continuous 0..0
  return true;
}

const controls = {
  rows: new Map(), // name → row
  byName: new Map(), // name → latest control from the daemon
  shape: "",
  error: null,
};

function renderControls(msg) {
  if (msg.error) {
    controls.error = msg;
    controls.shape = "";
    controls.rows.clear();
    const { left, right, center } = columns();
    left.replaceChildren(
      h("section", { class: "panel" }, h("p", { class: "muted", text: `Controls unavailable: ${msg.message}` })),
    );
    right.replaceChildren();
    center.replaceChildren();
    center.hidden = true;
    return;
  }
  controls.error = null;
  const list = msg.controls.filter(visible);
  controls.byName = new Map(msg.controls.map((c) => [c.name, c]));
  const shape = list.map((c) => `${c.name}:${c.type}:${c.min}:${c.max}:${c.step}`).join("|");
  if (shape !== controls.shape) {
    controls.shape = shape;
    build(list);
  }
  for (const c of list) controls.rows.get(c.name)?.update(c);
}

function build(list) {
  controls.rows.clear();
  const placed = { left: [], right: [], center: [] };
  for (const section of SECTIONS) {
    const items = order(list.filter((c) => sectionOf(c) === section.id));
    if (!items.length) continue;
    const rows = items.map((c) => {
      const row = makeRow(c);
      controls.rows.set(c.name, row);
      return row.el;
    });
    const resetNames = items.filter(settable).filter((c) => c.type !== "button").map((c) => c.name);
    const head = h(
      "div",
      { class: "panel-head" },
      icon(section.icon),
      h("h2", { text: section.title }),
      resetNames.length
        ? h(
            "button",
            {
              type: "button",
              class: "btn ghost",
              title: section.id === "ptz" ? "Back to the center at no zoom" : "Reset this section to the defaults",
              onclick: () => resetControls(resetNames),
            },
            icon(section.resetIcon),
            section.reset,
          )
        : null,
    );
    const body = h("div", { class: "rows" }, ...rows);
    // The center strip lives inside the preview panel and has no frame.
    placed[section.column].push(
      section.column === "center" ? [head, body] : [h("section", { class: "panel", "aria-label": section.title }, head, body)],
    );
  }
  const { left, right, center } = columns();
  left.replaceChildren(...placed.left.flat());
  right.replaceChildren(...placed.right.flat());
  center.replaceChildren(...placed.center.flat());
  center.hidden = placed.center.length === 0;
}

// order puts each manual control right after its automatic switch.
function order(items) {
  const out = items.filter((c) => !(MASTERS[c.name] || []).some((m) => items.some((i) => i.name === m)));
  for (const c of items) {
    const master = (MASTERS[c.name] || []).find((m) => out.some((o) => o.name === m));
    if (master) out.splice(out.findIndex((o) => o.name === master) + 1, 0, c);
  }
  return out;
}

function masterOf(c) {
  const name = (MASTERS[c.name] || []).find((m) => controls.byName.has(m));
  return name ? controls.byName.get(name) : null;
}

// manualValue is the master's value that turns the dependent manual.
function manualValue(master) {
  if (master.type === "bool") return 0;
  const item = (master.menu || []).find((m) => /manual/i.test(m.label));
  return item ? item.value : null;
}

function makeRow(c) {
  const id = `ctl-${c.name}`;
  const row = { control: c, hold: 0, dragging: false };
  const valueEl = h("span", { class: "ctl-value" });
  const reset = h(
    "button",
    { type: "button", class: "icon-btn", onclick: () => resetControls([row.control.name]) },
    icon("rotate-ccw"),
  );
  const hint = h("p", { class: "hint", hidden: true });
  let input;
  let head;

  const holdFor = (ms) => {
    row.hold = Date.now() + ms;
    clearTimeout(row.release);
    // Show the daemon's value again once the hold ends.
    row.release = setTimeout(() => row.update(controls.byName.get(row.control.name) || row.control), ms + 50);
  };

  if (c.type === "int") {
    input = h("input", { type: "range", id, min: c.min, max: c.max, step: Math.max(1, c.step) });
    const send = throttle((v) => setControl(row.control.name, v), 120);
    input.addEventListener("pointerdown", () => (row.dragging = true));
    input.addEventListener("input", () => {
      holdFor(1500);
      valueEl.textContent = format(row.control, +input.value);
      send(+input.value);
    });
    input.addEventListener("change", () => {
      row.dragging = false;
      holdFor(1500);
      send.flush(+input.value);
    });
    head = h("div", { class: "ctl-head" }, h("label", { class: "ctl-label", for: id, text: label(c) }), valueEl, reset);
  } else if (c.type === "bool") {
    input = h("input", { type: "checkbox", id });
    input.addEventListener("change", () => {
      holdFor(1000);
      setControl(row.control.name, input.checked ? 1 : 0);
    });
    head = h("div", { class: "ctl-head" }, input, h("label", { class: "ctl-label", for: id, text: label(c) }), reset);
  } else if (c.type === "button") {
    input = h("button", { type: "button", class: "btn", text: label(c), onclick: () => setControl(row.control.name, 1) });
    head = h("div", { class: "ctl-head" }, input);
  } else {
    const items = c.menu || [];
    const short = items.length <= 4 && items.reduce((n, m) => n + menuLabel(m).length, 0) <= 40;
    if (short) {
      input = h(
        "div",
        { class: "segmented compact", role: "group", "aria-label": label(c) },
        ...items.map((m) =>
          h("button", {
            type: "button",
            "data-value": String(m.value),
            text: menuLabel(m),
            onclick: () => {
              holdFor(1000);
              setControl(row.control.name, m.value);
            },
          }),
        ),
      );
    } else {
      input = h("select", { class: "field", id }, ...items.map((m) => h("option", { value: String(m.value), text: menuLabel(m) })));
      input.addEventListener("change", () => {
        holdFor(1000);
        setControl(row.control.name, +input.value);
      });
    }
    head = h("div", { class: "ctl-head" }, h("label", { class: "ctl-label", for: short ? null : id, text: label(c) }), reset);
  }

  row.el = h("div", { class: "ctl" }, head, c.type === "bool" || c.type === "button" ? null : input, hint);

  row.update = (next) => {
    row.control = next;
    const inactive = next.flags.includes("inactive");
    const disabled = !settable(next) || inactive;
    const held = row.dragging || Date.now() < row.hold;
    const v = next.value;

    row.el.classList.toggle("inactive", inactive);
    reset.disabled = disabled || v === next.default || next.type === "button";
    reset.title = `Reset to ${format(next, next.default)}`;

    if (next.type === "int") {
      input.disabled = disabled;
      if (!held && v !== null) {
        input.value = v;
        valueEl.textContent = format(next, v);
      }
    } else if (next.type === "bool") {
      input.disabled = disabled;
      if (!held && v !== null) input.checked = v !== 0;
    } else if (next.type === "button") {
      input.disabled = disabled;
    } else if (input.tagName === "SELECT") {
      input.disabled = disabled;
      if (!held && v !== null) input.value = String(v);
    } else {
      for (const b of input.querySelectorAll("button")) {
        b.disabled = disabled;
        if (!held) b.classList.toggle("active", +b.dataset.value === v);
      }
    }

    // Explain inactive controls and offer the way out.
    hint.replaceChildren();
    hint.hidden = true;
    if (inactive) {
      const master = masterOf(next);
      hint.hidden = false;
      if (master && manualValue(master) !== null && settable(master)) {
        hint.append(
          `Controlled by ${label(master).toLowerCase()}. `,
          h("button", {
            type: "button",
            class: "link",
            text: "Set manually",
            onclick: () => setControl(master.name, manualValue(master)),
          }),
        );
      } else {
        hint.append("Set automatically.");
      }
    } else if (next.error) {
      hint.hidden = false;
      hint.textContent = next.error;
    }
  };

  row.update(c);
  return row;
}

async function setControl(name, value) {
  const res = await api("PUT", `/api/controls/${encodeURIComponent(name)}`, { value });
  if (res.ok) renderControls(res.data);
  else toast(res.data?.message || `Setting ${name} failed (HTTP ${res.status})`);
}

async function resetControls(names) {
  for (const n of names) {
    const row = controls.rows.get(n);
    if (row) row.hold = 0;
  }
  const res = await api("POST", "/api/controls/reset", { names });
  if (res.ok) renderControls(res.data);
  else toast(res.data?.message || `Reset failed (HTTP ${res.status})`);
}

// throttle calls fn at most every ms with the latest argument, and once
// more at the end; flush sends now.
function throttle(fn, ms) {
  let last = 0;
  let timer = null;
  let pending;
  const run = () => {
    timer = null;
    last = Date.now();
    fn(pending);
  };
  const call = (v) => {
    pending = v;
    if (timer) return;
    const wait = Math.max(0, last + ms - Date.now());
    timer = setTimeout(run, wait);
  };
  call.flush = (v) => {
    pending = v;
    clearTimeout(timer);
    run();
  };
  return call;
}

// ---------------------------------------------------------------------

connectEvents();
preview.render();
