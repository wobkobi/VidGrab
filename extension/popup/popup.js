// Toolbar popup: backend status and the live job list, fed by the background's jobsUpdated broadcasts.
const {constants, format} = YTDL;

const FINISHED = new Set(["complete", "error", "cancelled"]);
// Job actions sent at once by a card's Cancel all or Retry; see runGroupAction
const GROUP_ACTION_BATCH = 5;
// Set by renderJobs: the clear button clears failed cards, the only finished ones left
let clearFailed = false;
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

// Common yt-dlp failures, reworded with what to do about them. yt-dlp's messages run to
// several lines with FAQ links; the full text stays on the row's tooltip. Age checks come
// before the generic sign-in match, since YouTube words both as "Sign in to confirm...".
const KNOWN_ERRORS = [
  [/confirm your age|inappropriate for some users|age[- ]restricted/i, "Age-restricted: turn on Use Firefox cookies in settings"],
  [/not a bot|sign in to confirm/i, "YouTube wants a sign-in: turn on Use Firefox cookies in settings"],
  [/private video/i, "Private video: turn on Use Firefox cookies if your account has access"],
  [/members[- ]only|join this channel/i, "Members-only: turn on Use Firefox cookies if your account is a member"],
  [/premieres? in|live event will begin|will begin in/i, "Not out yet: an upcoming premiere or stream"],
  [/video unavailable|no longer available|has been removed/i, "Video unavailable"],
];

/**
 * Short form of a job's error message, for the row's meta line.
 * @param {string} [error] - Error from the backend.
 * @returns {string} A known error reworded, otherwise the error itself.
 */
function shortError(error) {
  if (!error) return "Download failed";
  const known = KNOWN_ERRORS.find(([pattern]) => pattern.test(error));
  return known ? known[1] : error;
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
      if (format.isPreparing(job)) return "Preparing...";
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
    // The title and full bar already say it finished; the filename is on the Open button's tooltip
    case "complete":
      return "";
    case "error":
      return shortError(job.error);
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
  // A link only for its keyboard focus and hover cue: a click goes through showVideo
  row.title = el("a", "job-title");
  row.title.addEventListener("click", (event) => {
    event.preventDefault();
    if (row.url) showVideo(row.url);
  });
  row.channel = el("div", "job-channel muted small");
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
  row.el.append(head, row.channel, bar, foot);
  rows.set(id, row);
  return row;
}

/**
 * Bring up a job's video: switch to a tab already on it (in any window), or open it in a new tab.
 * @param {string} url - The job's video link.
 */
async function showVideo(url) {
  try {
    const id = format.videoId(url);
    const tabs = id ? await browser.tabs.query({url: "*://*.youtube.com/*"}) : [];
    const tab = tabs.find((t) => format.videoId(t.url) === id);
    if (tab) {
      await browser.tabs.update(tab.id, {active: true});
      await browser.windows.update(tab.windowId, {focused: true});
    } else {
      await browser.tabs.create({url});
    }
    window.close();
  } catch (err) {
    showMessage(`Couldn't open the video: ${(err && err.message) || err}`);
  }
}

/**
 * Update a job row in place; action buttons are rebuilt only when the status changes.
 * @param {object} row - Row record from {@link createRow}.
 * @param {object} job - Job from the backend.
 */
function updateRow(row, job) {
  // A single video has no title until yt-dlp has read the page; the link stays on hover. One
  // that ended before that (members-only, private) never gets one, so it shows its link.
  const pending = FINISHED.has(job.status) ? (job.url || "").replace(/^https?:\/\/(www\.)?/, "") : "";
  const title = job.title || job.filename || pending || "Getting video details...";
  const channel = typeof job.channel === "string" ? job.channel.trim() : "";
  row.title.textContent = title;
  row.url = /^https?:\/\//.test(job.url || "") ? job.url : "";
  if (row.url) {
    row.title.href = row.url;
    row.title.title = `${title}\nGo to the video`;
  } else {
    row.title.removeAttribute("href");
    row.title.title = title;
  }
  row.channel.textContent = channel;
  row.channel.title = channel;
  row.tag.textContent = formatTag(job.options);
  row.el.dataset.status = job.status;
  const pct = job.status === "complete" ? 100 : Math.min(100, Math.max(0, Number(job.percent) || 0));
  row.fill.style.width = `${pct}%`;
  const meta = metaText(job);
  row.meta.textContent = meta;
  // A reworded error keeps yt-dlp's own text on hover
  row.meta.title = job.status === "error" ? job.detail || job.error || meta : meta;

  if (row.status === job.status) return;
  row.status = job.status;
  row.actions.replaceChildren();
  for (const action of ACTIONS_BY_STATUS[job.status] || []) {
    const button = el("button", "btn", ACTION_LABELS[action]);
    button.type = "button";
    if (action === "open" && job.filename) button.title = `Open ${job.filename}`;
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
    block = {el: el("section", "group"), grouped: true, collapsed: null, actionsKey: ""};
    block.toggle = el("button", "group-toggle");
    block.toggle.type = "button";
    block.toggle.append(el("span", "group-chevron"), folderIcon());
    block.toggle.addEventListener("click", () => setCollapsed(block, !block.collapsed));
    block.title = el("span", "group-title");
    block.sub = el("span", "group-sub muted small");
    const text = el("div", "group-text");
    text.append(block.title, block.sub);
    const head = el("div", "group-head");
    head.append(block.toggle, text);
    // The group buttons sit beside the overall bar, like a finished row's, so the summary line
    // above keeps the full width
    const bar = el("div", "bar group-bar");
    block.fill = el("span");
    bar.append(block.fill);
    block.actions = el("div", "actions");
    const foot = el("div", "group-foot");
    foot.append(bar, block.actions);
    block.list = el("div", "group-list");
    block.el.append(head, foot, block.list);
  } else {
    block = {el: null, grouped: false};
  }
  blocks.set(key, block);
  return block;
}

/**
 * Folder icon for playlist and channel groups, whose videos share a folder on disk.
 * @returns {SVGSVGElement} The icon.
 */
function folderIcon() {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("aria-hidden", "true");
  svg.classList.add("group-icon");
  const path = document.createElementNS(ns, "path");
  path.setAttribute("d", "M3 6.5A1.5 1.5 0 0 1 4.5 5h4.38a1.5 1.5 0 0 1 1.06.44L11.5 7h8A1.5 1.5 0 0 1 21 8.5v9a1.5 1.5 0 0 1-1.5 1.5h-15A1.5 1.5 0 0 1 3 17.5z");
  svg.append(path);
  return svg;
}

/**
 * Show or hide a group's video rows.
 * @param {object} block - Group block from {@link getBlock}.
 * @param {boolean} collapsed - True to hide the rows.
 */
function setCollapsed(block, collapsed) {
  block.collapsed = collapsed;
  block.el.classList.toggle("collapsed", collapsed);
  block.list.hidden = collapsed;
  block.toggle.setAttribute("aria-expanded", String(!collapsed));
  block.toggle.title = collapsed ? "Show videos" : "Hide videos";
  block.toggle.setAttribute("aria-label", block.toggle.title);
}

/**
 * Run one action on several jobs of a group, for Cancel all and Retry failed.
 * @param {Array<object>} jobs - Jobs to act on.
 * @param {string} action - Job action.
 * @param {HTMLButtonElement} button - Button to disable while the requests run.
 */
async function runGroupAction(jobs, action, button) {
  button.disabled = true;
  // A channel can hold 1000 jobs. All at once, the requests queue behind Firefox's few
  // connections per host and the later ones time out, so they go GROUP_ACTION_BATCH at a time.
  // Running jobs are cancelled last: cancelling one frees a slot, which would start a queued
  // job only for its own cancel to kill it moments later.
  const running = (job) => job.status === "downloading" || job.status === "processing";
  const order = action === "cancel" ? [...jobs.filter((job) => !running(job)), ...jobs.filter(running)] : jobs;
  const results = [];
  for (let i = 0; i < order.length; i += GROUP_ACTION_BATCH) {
    const batch = order.slice(i, i + GROUP_ACTION_BATCH);
    results.push(...(await Promise.all(batch.map((job) => send({type: "jobAction", id: job.id, action})))));
  }
  button.disabled = false;
  const failed = results.find((res) => !res.ok);
  if (failed) showMessage(failed.error);
}

/**
 * Update a group's header: title, summary line, overall progress and the group-wide actions.
 * Rows of a channel download skip the channel line, since it repeats the group title.
 * @param {object} block - Group block from {@link getBlock}.
 * @param {{title: string, jobs: Array<object>}} entry - Group from {@link groupJobs}.
 */
function updateGroup(block, entry) {
  const jobs = entry.jobs;
  const done = jobs.filter((job) => job.status === "complete");
  const errored = jobs.filter((job) => job.status === "error").length;
  const cancelled = jobs.filter((job) => job.status === "cancelled").length;
  // Both kinds of stopped item are what Retry restarts
  const failed = jobs.filter((job) => job.status === "error" || job.status === "cancelled");
  const active = jobs.filter((job) => constants.ACTIVE_STATUSES.includes(job.status));
  const running = jobs.some((job) => job.status === "downloading" || job.status === "processing");
  // A channel listing gives every row the channel's own name, which is also the group title
  const isChannel = jobs.every((job) => typeof job.channel === "string" && job.channel.trim() === entry.title);
  const tags = new Set(jobs.map((job) => formatTag(job.options)));

  block.title.textContent = entry.title;
  block.title.title = entry.title;
  const parts = [isChannel ? "Channel" : "Playlist"];
  if (tags.size === 1) parts.push([...tags][0]);
  parts.push(`${done.length} of ${jobs.length} done`);
  if (errored) parts.push(`${errored} failed`);
  if (cancelled) parts.push(`${cancelled} cancelled`);
  block.sub.textContent = parts.join(" · ");
  block.el.classList.toggle("same-tag", tags.size === 1);
  block.el.classList.toggle("same-channel", isChannel);

  // Done items count as full and failed or cancelled ones as empty, so a finished card with
  // failures shows how much of the list actually arrived (2 of 3 > two-thirds)
  const total = jobs.reduce((sum, job) => {
    if (job.status === "complete") return sum + 100;
    if (FINISHED.has(job.status)) return sum;
    return sum + Math.min(100, Math.max(0, Number(job.percent) || 0));
  }, 0);
  block.fill.style.width = `${total / jobs.length}%`;
  let state = "active";
  if (!active.length) state = errored ? "error" : cancelled ? "cancelled" : "complete";
  else if (!running && active.every((job) => job.status === "paused" || job.status === "queued") && active.some((job) => job.status === "paused")) state = "paused";
  block.el.dataset.status = state;

  // Start finished groups folded so an old playlist doesn't fill the popup, but keep ones
  // with a failure open so the error shows
  if (block.collapsed === null) setCollapsed(block, !active.length && !errored);

  // Buttons are rebuilt only when the set changes, so one under the cursor survives the refresh;
  // clicks read block.jobs, so they act on the latest state
  block.jobs = jobs;
  const labels = [];
  if (active.length) labels.push("cancel");
  else if (failed.length) labels.push("retry");
  if (done.length) labels.push("reveal");
  // Retry's label names what it restarts, so the key covers that too
  const retryLabel = !errored ? "Retry cancelled" : cancelled ? "Retry all" : "Retry failed";
  const key = `${labels.join("|")}:${retryLabel}`;
  if (block.actionsKey === key) return;
  block.actionsKey = key;
  block.actions.replaceChildren(...labels.map((action) => groupButton(block, action, retryLabel)));
}

/**
 * Build one group-wide action button.
 * @param {object} block - Group block from {@link getBlock}.
 * @param {"cancel"|"retry"|"reveal"} action - Cancel every active item, retry every failed or
 *   cancelled one, or show the group's folder (through its first finished file).
 * @param {string} retryLabel - Label for the retry button: Retry failed, Retry cancelled or Retry all.
 * @returns {HTMLButtonElement} The button.
 */
function groupButton(block, action, retryLabel) {
  const label = {cancel: "Cancel all", retry: retryLabel, reveal: "Show folder"}[action];
  const button = el("button", "btn", label);
  button.type = "button";
  button.addEventListener("click", () => {
    const jobs = block.jobs || [];
    if (action === "reveal") {
      const done = jobs.find((job) => job.status === "complete");
      if (done) runAction(done.id, "reveal", button);
    } else if (action === "cancel") {
      runGroupAction(jobs.filter((job) => constants.ACTIVE_STATUSES.includes(job.status)), "cancel", button);
    } else {
      runGroupAction(jobs.filter((job) => job.status === "error" || job.status === "cancelled"), "retry", button);
    }
  });
  return button;
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
      updateGroup(block, entry);
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
  // The helper clears whole cards, and keeps ones with a failure until asked again, so an
  // error is not cleared away unread. With only those left, the button clears them.
  const done = ordered.filter((block) => block.jobs.every((job) => FINISHED.has(job.status)));
  const failed = done.filter((block) => block.jobs.some((job) => job.status === "error"));
  clearFailed = failed.length > 0 && failed.length === done.length;
  els.clear.textContent = clearFailed ? "Clear failed" : "Clear finished";
  els.clear.disabled = done.length === 0;
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
  els.update.hidden = !health.update_available && !busy.update;
  els.update.disabled = busy.update;
  els.update.textContent = busy.update ? "Updating..." : "Update";
  els.update.title = health.yt_dlp_latest ? `Update yt-dlp to ${health.yt_dlp_latest}` : "Update yt-dlp";
  // Downloads still work with an old helper, so this warns rather than blocks
  if (constants.helperOutdated(health.version)) {
    els.dot.className = "dot busy";
    els.title.textContent = `Helper out of date (v${health.version})`;
    renderHelperLink(`Download helper ${constants.MIN_HELPER_VERSION} or newer and run it once`);
    return;
  }
  els.title.textContent = `Backend running (v${health.version})`;
  const parts = [`yt-dlp ${health.yt_dlp_version}`];
  parts.push(health.ffmpeg ? "ffmpeg found" : "ffmpeg missing: MP3, above 720p and max size unavailable");
  els.meta.textContent = parts.join(" · ");
  els.meta.title = els.meta.textContent;
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
  const res = await send({type: "clearFinished", failed: clearFailed});
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
