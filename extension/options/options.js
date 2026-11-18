// Options page: edits the "settings" object in storage.local and tells the background to apply it.
const {constants} = YTDL;

const form = document.getElementById("form");
const saveStatus = document.getElementById("save-status");
const testStatus = document.getElementById("test-status");
const testButton = document.getElementById("test");
const ffmpegHint = document.getElementById("ffmpeg-hint");

const DEFAULT_FIELDS = ["kind", "quality", "codec", "container", "audio_format", "audio_bitrate"];
const SELECTOR_FIELDS = ["watchContainer", "watchInsertion", "shortsBar"];
const SELECT_OPTIONS = {
  kind: constants.KINDS,
  quality: constants.QUALITIES,
  codec: constants.CODECS,
  container: constants.CONTAINERS,
  audio_format: constants.AUDIO_FORMATS,
  audio_bitrate: constants.AUDIO_BITRATES,
  watchInsertion: constants.WATCH_INSERTIONS,
};

// ffmpeg availability from the last health check; null until known.
let hasFfmpeg = null;

/**
 * Look up a form control by id.
 * @param {string} id - Element id.
 * @returns {HTMLInputElement|HTMLSelectElement} The control.
 */
function field(id) {
  return document.getElementById(id);
}

/**
 * Set a status line's text and colour.
 * @param {HTMLElement} node - Status element.
 * @param {string} text - Message.
 * @param {"ok"|"bad"|""} [tone] - Colour class.
 */
function setStatus(node, text, tone = "") {
  node.textContent = text;
  node.className = tone ? `status ${tone}` : "status";
}

/**
 * Fill every select from its enum list in {@link constants}.
 */
function populateSelects() {
  for (const [id, list] of Object.entries(SELECT_OPTIONS)) {
    const select = field(id);
    for (const item of list) {
      const option = document.createElement("option");
      option.value = item.value;
      option.textContent = item.label;
      select.append(option);
    }
  }
}

/**
 * Dim the fields that do not apply to the chosen default type and warn when the chosen
 * default needs ffmpeg but the backend reported none.
 */
function updateDependentFields() {
  const kind = field("kind").value;
  for (const label of form.querySelectorAll("label[data-for]")) {
    label.classList.toggle("dimmed", label.dataset.for !== kind);
  }
  field("audio_bitrate").disabled = kind !== "audio" || field("audio_format").value !== "mp3";
  const needs = constants.NEEDS_FFMPEG({
    kind,
    quality: field("quality").value,
    audio_format: field("audio_format").value,
  });
  ffmpegHint.hidden = !(needs && hasFfmpeg === false);
  ffmpegHint.textContent = "ffmpeg was not found. This default needs it (MP3, or video above 720p); install ffmpeg or pick 720p or M4A.";
}

/**
 * Write a settings object into the form.
 * @param {object} settings - Full settings object.
 */
function fillForm(settings) {
  for (const key of DEFAULT_FIELDS) field(key).value = settings.defaults[key];
  for (const key of SELECTOR_FIELDS) field(key).value = settings.selectors[key];
  field("concurrency").value = settings.concurrency;
  field("fragments").value = settings.fragments;
  field("cookies").checked = settings.cookies;
  field("notifications").checked = settings.notifications;
  field("auto_update").checked = settings.auto_update;
  updateDependentFields();
}

/**
 * Read the form into a settings object, or report the first invalid number.
 * @returns {{settings?: object, error?: string}} The validated settings or an error message.
 */
function readForm() {
  const numbers = {};
  for (const key of ["concurrency", "fragments"]) {
    const value = Number(field(key).value);
    const {min, max} = constants.LIMITS[key];
    if (!Number.isInteger(value) || value < min || value > max) {
      const name = key === "concurrency" ? "Parallel downloads" : "Download speed-up";
      return {error: `${name} must be a whole number from ${min} to ${max}.`};
    }
    numbers[key] = value;
  }
  const defaults = {};
  for (const key of DEFAULT_FIELDS) defaults[key] = field(key).value;
  const selectors = {};
  for (const key of SELECTOR_FIELDS) selectors[key] = field(key).value.trim();
  for (const key of ["watchContainer", "shortsBar"]) {
    try {
      document.querySelector(selectors[key]);
    } catch (err) {
      return {error: `"${selectors[key]}" is not a valid CSS selector.`};
    }
  }
  return {
    settings: constants.mergeSettings({
      defaults,
      selectors,
      ...numbers,
      cookies: field("cookies").checked,
      notifications: field("notifications").checked,
      auto_update: field("auto_update").checked,
    }),
  };
}

/**
 * Store settings and ask the background to push them to the backend.
 * @param {object} settings - Full settings object.
 * @param {string} doneText - Status text on success.
 */
async function persist(settings, doneText) {
  try {
    await browser.storage.local.set({settings});
  } catch (err) {
    setStatus(saveStatus, `Could not save: ${err.message}`, "bad");
    return;
  }
  fillForm(settings);
  let res;
  try {
    res = await browser.runtime.sendMessage({type: "settingsChanged"});
  } catch (err) {
    res = null;
  }
  if (res && res.ok && res.data && res.data.pushed) setStatus(saveStatus, doneText, "ok");
  else setStatus(saveStatus, `${doneText} The backend is not running; it gets these settings when it starts.`, "ok");
}

/**
 * Load stored settings into the form.
 */
async function load() {
  let stored;
  try {
    stored = (await browser.storage.local.get("settings")).settings;
  } catch (err) {
    setStatus(saveStatus, `Could not load settings: ${err.message}`, "bad");
  }
  fillForm(constants.mergeSettings(stored));
}

/**
 * Check the backend through the background script and describe the result.
 */
async function testBackend() {
  testButton.disabled = true;
  setStatus(testStatus, "Checking...");
  let res;
  try {
    res = await browser.runtime.sendMessage({type: "health"});
  } catch (err) {
    res = {ok: false, error: err.message};
  }
  testButton.disabled = false;
  if (!res || !res.ok) {
    setStatus(testStatus, (res && res.error) || "No response from the extension", "bad");
    return;
  }
  const health = res.data.health;
  if (!res.data.reachable || !health) {
    hasFfmpeg = null;
    updateDependentFields();
    setStatus(
      testStatus,
      res.data.error || "Backend not running. Run python backend/install_native_host.py once and restart Firefox.",
      "bad",
    );
    return;
  }
  hasFfmpeg = Boolean(health.ffmpeg);
  updateDependentFields();
  const parts = [
    `Backend v${health.version} running`,
    `yt-dlp ${health.yt_dlp_version}${health.update_available ? ` (update to ${health.yt_dlp_latest} available)` : ""}`,
    health.ffmpeg ? "ffmpeg found" : "ffmpeg missing: MP3 and video above 720p are unavailable",
  ];
  setStatus(testStatus, parts.join(" · "), health.ffmpeg ? "ok" : "bad");
}

form.addEventListener("change", (event) => {
  if (["kind", "quality", "audio_format"].includes(event.target.id)) updateDependentFields();
  setStatus(saveStatus, "Unsaved changes");
});

form.addEventListener("submit", (event) => {
  event.preventDefault();
  const {settings, error} = readForm();
  if (error) {
    setStatus(saveStatus, error, "bad");
    return;
  }
  persist(settings, "Saved.");
});

document.getElementById("reset").addEventListener("click", () => {
  persist(constants.mergeSettings(undefined), "Defaults restored.");
});

document.getElementById("reset-selectors").addEventListener("click", () => {
  for (const key of SELECTOR_FIELDS) field(key).value = constants.DEFAULT_SETTINGS.selectors[key];
  setStatus(saveStatus, "Selectors reset. Save to apply.");
});

testButton.addEventListener("click", testBackend);

populateSelects();
load().then(testBackend);
