// Toolbar popup: backend status and the live job list, fed by the background's jobsUpdated broadcasts.
const {constants, format} = YTDL;

const FINISHED = new Set(["complete", "error", "cancelled"]);
const ACTION_LABELS = {
  pause: "Pause",
  resume: "Resume",
  cancel: "Cancel",
  retry: "Retry",
  open: "Open",
  reveal: "Show in folder",
};
const ACTIONS_BY_STATUS = {
  queued: ["cancel"],
  downloading: ["pause", "cancel"],
  processing: ["cancel"],
  paused: ["resume", "cancel"],
  complete: ["open", "reveal"],
  error: ["retry"],
  cancelled: ["retry"],
};

const els = {
  dot: document.getElementById("status-dot"),
  title: document.getElementById("status-title"),
  meta: document.getElementById("status-meta"),
  start: document.getElementById("start-btn"),
  update: document.getElementById("update-btn"),
  options: document.getElementById("options-btn"),
  message: document.getElementById("message"),
  jobs: document.getElementById("jobs"),
  empty: document.getElementById("empty"),
  summary: document.getElementById("summary"),
  clear: document.getElementById("clear-btn"),
};

// Rendered blocks keyed by group id (or "job:<id>" for single jobs) and rows keyed by job id.
// Rows are updated in place so a button under the cursor survives the once-a-second refresh.
const blocks = new Map();
const rows = new Map();
const busy = {start: false, update: false};
let messageTimer = null;

/**
 * Send a runtime message to the background script.
 * @param {object} message - Message with a `type` field.
 * @returns {Promise<{ok: boolean, data?: any, error?: string}>} The background's response.
 */
async function send(message) {
  try {
    const res = await browser.runtime.sendMessage(message);
    return res || {ok: false, error: "No response from the extension"};
  } catch (err) {
    return {ok: false, error: (err && err.message) || "Extension error"};
  }
}

/**
 * Show a short message above the job list.
 * @param {string} text - Message text; empty hides it.
 * @param {"error"|"info"} [kind] - Colour of the side marker.
 */
function showMessage(text, kind = "error") {
  clearTimeout(messageTimer);
  els.message.textContent = text || "";
  els.message.className = kind === "info" ? "message info" : "message";
  els.message.hidden = !text;
  if (text) messageTimer = setTimeout(() => (els.message.hidden = true), 8000);
}

/**
 * Create an element with an optional class and text.
 * @param {string} tag - Tag name.
 * @param {string} [className] - Class attribute.
 * @param {string} [text] - Text content.
 * @returns {HTMLElement} The new element.
 */
function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

/**
 * Describe a job's output format, for example "MP4 1080p", "MP4 best ≤20 MB" or "MP3 192k".
 * A max size always produces MP4, whatever container the job asked for.
 * @param {object} options - Job options.
 * @returns {string} Short format tag.
 */
function formatTag(options) {
  if (!options) return "";
  if (options.kind === "audio") {
    const fmt = String(options.audio_format || "").toUpperCase();
    return options.audio_format === "mp3" ? `${fmt} ${options.audio_bitrate}k` : fmt;
  }
  const quality = options.quality === "best" ? "best" : `${options.quality}p`;
  const maxSize = Number(options.max_size_mb) > 0 ? Number(options.max_size_mb) : 0;
  const container = maxSize ? "MP4" : String(options.container || "").toUpperCase();
  return maxSize ? `${container} ${quality} ≤${maxSize} MB` : `${container} ${quality}`;
}

/**
 * Build the status/progress line for a job.
 * @param {object} job - Job from the backend.
 * @returns {string} Meta text.
 */
function metaText(job) {
  const label = format.statusLabel(job.status);
  switch (job.status) {
    case "downloading": {
      const parts = [format.percent(job.percent)];
      if (job.speed) parts.push(job.speed);
      const eta = format.eta(job.eta);
      if (eta) parts.push(`ETA ${eta}`);
      return parts.join(" · ");
    }
    case "processing":
      if (format.isConverting(job)) return format.convertingMeta(job);
      return job.kind === "audio" ? "Converting audio..." : "Merging video and audio...";
    case "paused":
      return `${label} at ${format.percent(job.percent)}`;
    case "complete":
      return job.filename || label;
    case "error":
      return job.error || "Download failed";
    default:
      return label;
  }
}

/**
 * Run a job action through the background and report failures.
 * @param {string} id - Job id.
 * @param {string} action - pause|resume|cancel|retry|open|reveal.
 * @param {HTMLButtonElement} button - Button to disable while the request runs.
 */
async function runAction(id, action, button) {
  button.disabled = true;
  const res = await send({type: "jobAction", id, action});
  button.disabled = false;
  if (!res.ok) showMessage(res.error);
}

/**
 * Create the DOM for one job row.
 * @param {string} id - Job id.
 * @returns {object} Row record with references to the parts that change.
 */
function createRow(id) {
  const row = {el: el("div", "job"), status: null};
  row.title = el("span", "job-title");
  row.tag = el("span", "tag");
  const head = el("div", "job-head");
  head.append(row.title, row.tag);
  const bar = el("div", "bar");
  row.fill = el("span");
  bar.append(row.fill);
  row.meta = el("span", "job-meta muted small");
  row.actions = el("div", "actions");
  const foot = el("div", "job-foot");
  foot.append(row.meta, row.actions);
  row.el.append(head, bar, foot);
  rows.set(id, row);
  return row;
}

/**
 * Update a job row in place; action buttons are rebuilt only when the status changes.
 * @param {object} row - Row record from {@link createRow}.
 * @param {object} job - Job from the backend.
 */
function updateRow(row, job) {
  const title = job.title || job.filename || job.url;
  row.title.textContent = title;
  row.title.title = title;
  row.tag.textContent = formatTag(job.options);
  row.el.dataset.status = job.status;
  const pct = job.status === "complete" ? 100 : Math.min(100, Math.max(0, Number(job.percent) || 0));
  row.fill.style.width = `${pct}%`;
  const meta = metaText(job);
  row.meta.textContent = meta;
  row.meta.title = job.status === "error" && job.detail ? job.detail : meta;

  if (row.status === job.status) return;
  row.status = job.status;
  row.actions.replaceChildren();
  for (const action of ACTIONS_BY_STATUS[job.status] || []) {
    const button = el("button", "btn", ACTION_LABELS[action]);
    button.type = "button";
    button.addEventListener("click", () => runAction(job.id, action, button));
    row.actions.append(button);
  }
}

/**
 * Get or create the DOM block for a group (or a single ungrouped job).
 * @param {string} key - Block key.
 * @param {boolean} grouped - True for playlist/channel groups.
 * @returns {object} Block record.
 */
function getBlock(key, grouped) {
  let block = blocks.get(key);
  if (block) return block;
  if (grouped) {
    block = {el: el("section", "group"), grouped: true};
    const head = el("div", "group-head");
    block.title = el("span", "group-title");
    block.count = el("span", "group-count muted small");
    head.append(block.title, block.count);
    block.list = el("div");
    block.el.append(head, block.list);
  } else {
    block = {el: null, grouped: false};
  }
  blocks.set(key, block);
  return block;
}

/**
 * Place a child at a position, moving it only when it is not already there.
 * @param {HTMLElement} parent - Container.
 * @param {HTMLElement} child - Element to place.
 * @param {number} index - Target index.
 */
function placeAt(parent, child, index) {
  const current = parent.children[index];
  if (current !== child) parent.insertBefore(child, current || null);
}

/**
 * Group jobs by group_id, newest block first, jobs inside a group in playlist order.
 * @param {Array<object>} jobs - Jobs from the backend.
 * @returns {Array<{key: string, grouped: boolean, title: string, jobs: Array<object>}>} Ordered blocks.
 */
function groupJobs(jobs) {
  const byKey = new Map();
  for (const job of jobs) {
    const key = job.group_id ? `group:${job.group_id}` : `job:${job.id}`;
    let entry = byKey.get(key);
    if (!entry) {
      entry = {key, grouped: Boolean(job.group_id), title: job.group_title || "Playlist", jobs: [], newest: 0};
      byKey.set(key, entry);
    }
    entry.jobs.push(job);
    entry.newest = Math.max(entry.newest, Number(job.created_at) || 0);
  }
  const list = [...byKey.values()];
  for (const entry of list) entry.jobs.sort((a, b) => (a.created_at || 0) - (b.created_at || 0));
  return list.sort((a, b) => b.newest - a.newest);
}

/**
 * Render the job list, reusing existing rows and blocks.
 * @param {Array<object>} jobs - Jobs from the backend.
 */
function renderJobs(jobs) {
  const ordered = groupJobs(jobs);
  const seenBlocks = new Set();
  const seenRows = new Set();

  ordered.forEach((entry, index) => {
    seenBlocks.add(entry.key);
    const block = getBlock(entry.key, entry.grouped);
    let blockEl;
    if (entry.grouped) {
      const done = entry.jobs.filter((job) => job.status === "complete").length;
      const failed = entry.jobs.filter((job) => job.status === "error").length;
      block.title.textContent = entry.title;
      block.title.title = entry.title;
      block.count.textContent = `${done} of ${entry.jobs.length} done${failed ? ` · ${failed} failed` : ""}`;
      entry.jobs.forEach((job, i) => {
        seenRows.add(job.id);
        const row = rows.get(job.id) || createRow(job.id);
        updateRow(row, job);
        placeAt(block.list, row.el, i);
      });
      while (block.list.children.length > entry.jobs.length) block.list.lastElementChild.remove();
      blockEl = block.el;
    } else {
      const job = entry.jobs[0];
      seenRows.add(job.id);
      const row = rows.get(job.id) || createRow(job.id);
      updateRow(row, job);
      blockEl = row.el;
    }
    placeAt(els.jobs, blockEl, index);
  });

  while (els.jobs.children.length > ordered.length) els.jobs.lastElementChild.remove();
  for (const key of [...blocks.keys()]) if (!seenBlocks.has(key)) blocks.delete(key);
  for (const id of [...rows.keys()]) if (!seenRows.has(id)) rows.delete(id);

  const active = jobs.filter((job) => constants.ACTIVE_STATUSES.includes(job.status)).length;
  const finished = jobs.filter((job) => FINISHED.has(job.status)).length;
  els.empty.hidden = jobs.length > 0;
  els.clear.disabled = finished === 0;
  els.summary.textContent = jobs.length ? `${active} active · ${finished} finished` : "";
}

/**
 * Show the helper download instruction in the meta line, as a link when
 * {@link constants.HELPER_URL} is set and as plain text otherwise.
 * @param {string} text - Instruction text.
 */
function renderHelperLink(text) {
  if (!constants.HELPER_URL) {
    els.meta.textContent = text;
  } else {
    const link = el("a", "", text);
    link.href = constants.HELPER_URL;
    link.target = "_blank";
    link.rel = "noopener noreferrer";
    els.meta.replaceChildren(link);
  }
  els.meta.title = text;
}

/**
 * Render the backend status line.
 * @param {{reachable: boolean|null, health: object|null, error?: string|null, code?: string|null, detail?: string|null}} backend - Backend state; reachable is null while it starts.
 */
function renderBackend(backend) {
  const health = backend && backend.reachable ? backend.health : null;
  els.dot.className = health ? "dot ok" : "dot bad";
  if (!health) {
    const starting = busy.start || (backend && backend.reachable === null);
    const missing = !starting && backend && backend.code === constants.HELPER_MISSING;
    els.start.hidden = starting;
    els.start.disabled = busy.start;
    els.start.textContent = missing ? "Check again" : "Start";
    els.update.hidden = true;
    if (missing) {
      els.title.textContent = backend.error || constants.HELPER_MISSING_ERROR;
      renderHelperLink(backend.detail || constants.HELPER_MISSING_DETAIL);
      return;
    }
    els.title.textContent = starting ? "Starting backend..." : "Backend not running";
    els.meta.textContent = starting ? "" : (backend && backend.error) || "Run the VidGrab helper once and restart Firefox.";
    els.meta.title = els.meta.textContent;
    return;
  }
  els.start.hidden = true;
  // First-run setup: the background refreshes /health every 2s and rebroadcasts progress
  const setup = format.setupStatus(health);
  if (setup) {
    els.dot.className = setup.state === "error" ? "dot bad" : "dot busy";
    els.title.textContent = setup.state === "error" ? "Setting up yt-dlp and ffmpeg failed" : setup.text;
    els.meta.textContent = setup.state === "error" ? setup.text : "Downloads can start once setup finishes.";
    els.meta.title = els.meta.textContent;
    els.update.hidden = true;
    return;
  }
  els.title.textContent = `Backend running (v${health.version})`;
  const parts = [`yt-dlp ${health.yt_dlp_version}`];
  parts.push(health.ffmpeg ? "ffmpeg found" : "ffmpeg missing: MP3, above 720p and max size unavailable");
  els.meta.textContent = parts.join(" · ");
  els.meta.title = els.meta.textContent;
  els.update.hidden = !health.update_available && !busy.update;
  els.update.disabled = busy.update;
  els.update.textContent = busy.update ? "Updating..." : "Update";
  els.update.title = health.yt_dlp_latest ? `Update yt-dlp to ${health.yt_dlp_latest}` : "Update yt-dlp";
}

/**
 * Render a full state snapshot from the background.
 * @param {{jobs: Array<object>, backend: object}} data - Jobs and backend state.
 */
function render(data) {
  renderBackend(data.backend);
  renderJobs(Array.isArray(data.jobs) ? data.jobs : []);
}

els.options.addEventListener("click", () => {
  browser.runtime.openOptionsPage();
  window.close();
});

els.start.addEventListener("click", async () => {
  busy.start = true;
  renderBackend({reachable: false});
  const res = await send({type: "ensureBackend"});
  busy.start = false;
  if (!res.ok) showMessage(res.error);
  else if (res.data.code === constants.HELPER_MISSING) showMessage("Still not found. Run the helper, then check again.");
  else if (!res.data.reachable) showMessage(res.data.error || "Backend did not start. Check that the VidGrab helper is installed.");
  renderBackend(res.ok ? res.data : {reachable: false});
});

els.update.addEventListener("click", async () => {
  busy.update = true;
  els.update.disabled = true;
  els.update.textContent = "Updating...";
  const res = await send({type: "updateYtDlp"});
  busy.update = false;
  if (!res.ok) showMessage(res.error);
  else if (res.data && res.data.updated) showMessage(`yt-dlp updated to ${res.data.version}`, "info");
  else showMessage("yt-dlp is already up to date", "info");
  const latest = await send({type: "getJobs"});
  if (latest.ok) render(latest.data);
});

els.clear.addEventListener("click", async () => {
  els.clear.disabled = true;
  const res = await send({type: "clearFinished"});
  if (!res.ok) {
    showMessage(res.error);
    els.clear.disabled = false;
  }
});

browser.runtime.onMessage.addListener((message) => {
  if (message && message.type === "jobsUpdated") render(message);
});

send({type: "getJobs"}).then((res) => {
  if (res.ok) render(res.data);
  else showMessage(res.error);
  // Refresh health in the background; any change arrives as jobsUpdated.
  send({type: "health"});
});
