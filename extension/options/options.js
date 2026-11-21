// Options page: edits the "settings" object in storage.local and tells the background to apply it.
const {constants} = YTDL;

const form = document.getElementById("form");
const saveStatus = document.getElementById("save-status");
const testStatus = document.getElementById("test-status");
const testButton = document.getElementById("test");
const ffmpegHint = document.getElementById("ffmpeg-hint");
const sizeHint = document.getElementById("size-hint");
const customSizeLabel = document.getElementById("max-size-custom");

const DEFAULT_FIELDS = ["kind", "quality", "codec", "container", "audio_format", "audio_bitrate"];
const SELECTOR_FIELDS = ["watchContainer", "watchInsertion", "shortsBar"];
const SELECT_OPTIONS = {
  kind: constants.KINDS,
  quality: constants.QUALITIES,
  codec: constants.CODECS,
  container: constants.CONTAINERS,
  audio_format: constants.AUDIO_FORMATS,
  audio_bitrate: constants.AUDIO_BITRATES,
  max_size: constants.MAX_SIZES,
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
 * Disable the fields the other choices make meaningless (bitrate for M4A, container under a max
 * size) and warn when the chosen default needs ffmpeg but the backend reported none. Video and
 * audio fields stay editable whatever the type, since the panel starts from all of them.
 */
function updateDependentFields() {
  const kind = field("kind").value;
  field("audio_bitrate").disabled = field("audio_format").value !== "mp3";

  // A max size always saves MP4: show it in the disabled container select and put the
  // earlier choice back when the size is turned off
  const choice = field("max_size").value;
  const sized = kind === "video" && choice !== "0";
  const container = field("container");
  if (sized && !container.disabled) {
    container.dataset.chosen = container.value;
    container.value = "mp4";
  } else if (!sized && container.disabled) {
    container.value = container.dataset.chosen || container.value;
  }
  container.disabled = sized;
  customSizeLabel.hidden = choice !== constants.MAX_SIZE_CUSTOM;
  sizeHint.hidden = !sized;

  const needs = constants.NEEDS_FFMPEG({
    kind,
    quality: field("quality").value,
    audio_format: field("audio_format").value,
    max_size_mb: choice === "0" ? 0 : 1,
  });
  ffmpegHint.hidden = !(needs && hasFfmpeg === false);
  ffmpegHint.textContent =
    "ffmpeg was not found. This default needs it (MP3, video above 720p, or a max size); install ffmpeg or pick 720p, M4A and no max size.";
}

/**
 * Write a settings object into the form.
 * @param {object} settings - Full settings object.
 */
function fillForm(settings) {
  field("container").disabled = false;
  for (const key of DEFAULT_FIELDS) field(key).value = settings.defaults[key];
  const size = settings.defaults.max_size_mb;
  field("max_size").value = constants.maxSizeChoice(size);
  field("max_size_custom").value = field("max_size").value === constants.MAX_SIZE_CUSTOM ? String(size) : "";
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
  const maxSize = constants.maxSizeValue(field("max_size").value, field("max_size_custom").value);
  if (maxSize === null) {
    const {min, max} = constants.LIMITS.max_size_mb;
    return {error: `Custom max size must be a whole number of MB from ${min} to ${max}.`};
  }
  defaults.max_size_mb = maxSize;
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
      // Built from the form, so it is current-version and a "first" placement is deliberate
      version: constants.SETTINGS_VERSION,
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
      res.data.error || "Helper not running. Download the VidGrab helper, run it once, then restart Firefox.",
      "bad",
    );
    return;
  }
  hasFfmpeg = Boolean(health.ffmpeg);
  updateDependentFields();
  const parts = [
    `Backend v${health.version} running`,
    `yt-dlp ${health.yt_dlp_version}${health.update_available ? ` (update to ${health.yt_dlp_latest} available)` : ""}`,
    health.ffmpeg ? "ffmpeg found" : "ffmpeg missing: MP3, video above 720p and max size are unavailable",
  ];
  setStatus(testStatus, parts.join(" · "), health.ffmpeg ? "ok" : "bad");
}

form.addEventListener("change", (event) => {
  if (["kind", "quality", "audio_format", "max_size"].includes(event.target.id)) updateDependentFields();
  if (event.target.id === "max_size" && event.target.value === constants.MAX_SIZE_CUSTOM) field("max_size_custom").focus();
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
document.getElementById("version").textContent = `v${browser.runtime.getManifest().version}`;
load().then(testBackend);
