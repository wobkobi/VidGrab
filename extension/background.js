// Background script: the only HTTP client of the backend. Routes runtime messages, polls
// the job queue, broadcasts jobsUpdated, drives the badge and desktop notifications.
const {constants, api} = YTDL;

const NATIVE_HOST = "yt_dl_backend";
const POLL_ACTIVE_MS = 1000;
const POLL_IDLE_MS = 30000;
const HEALTH_REFRESH_MS = 30000;
const START_RETRY_MS = 5000;
const START_TIMEOUT_MS = 30000;
const INFO_TTL_MS = 10 * 60 * 1000;
const INFO_CACHE_MAX = 50;
// Minimum gap between native-host token requests triggered by a rejected token
const TOKEN_RETRY_MS = 10000;
const NOTIFICATION_PREFIX = "ytdl-job:";
const GROUP_NOTIFICATION_PREFIX = "ytdl-group:";
const NO_TOKEN_ERROR =
  "Could not get the backend key from the native host. Run python backend/install_native_host.py once and restart Firefox.";
const FOREIGN_ERROR = "Another program is using port 18956 and is not the yt-dl backend.";
const JOB_ACTIONS = new Set(["pause", "resume", "cancel", "retry", "open", "reveal"]);
const ACTIVE = new Set(constants.ACTIVE_STATUSES);

const state = {jobs: [], backend: {reachable: false, health: null, error: null}};
// True while ensureBackend runs (and before the first attempt), so the UI can say
// "Starting backend..." instead of "Backend not running" during a normal start.
let backendStarting = true;
let settings = constants.mergeSettings(undefined);
let lastBroadcast = "";
let lastBadge = null;
// Status per job id from the previous poll; null until the first poll so jobs that
// finished before the browser started do not raise notifications.
let lastStatuses = null;
let lastHealthAt = 0;
let pollTimer = null;
let polling = false;
let pollQueued = false;
let ensurePromise = null;
let tokenPromise = null;
let lastTokenAt = 0;
let autoUpdateTried = false;
let autoUpdateChecking = false;
const infoCache = new Map();

/**
 * Wait for a number of milliseconds.
 * @param {number} ms - Delay in milliseconds.
 * @returns {Promise<void>} Resolves after the delay.
 */
function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Count jobs that are queued, running or paused.
 * @param {Array<{status: string}>} jobs - Jobs to count.
 * @returns {number} Number of active jobs.
 */
function activeCount(jobs) {
  return jobs.filter((job) => ACTIVE.has(job.status)).length;
}

/**
 * Snapshot the backend state in the shape the runtime messages use. `reachable` is null
 * while a start attempt is still running, and `error` explains a known cause of failure.
 * @returns {{reachable: boolean|null, health: object|null, error: string|null}} Copy of the backend state.
 */
function backendSnapshot() {
  const reachable = state.backend.reachable ? true : backendStarting ? null : false;
  return {reachable, health: state.backend.health, error: state.backend.error};
}

/**
 * Ask the native host for the install token. Any command other than "ping" also makes the
 * host start the backend when it is down. Concurrent callers share one request.
 * @returns {Promise<boolean>} True when the host supplied a token.
 */
function requestToken() {
  if (!tokenPromise) {
    tokenPromise = callNativeHost().finally(() => {
      tokenPromise = null;
    });
  }
  return tokenPromise;
}

/**
 * Body of {@link requestToken}.
 * @returns {Promise<boolean>} True when the host supplied a token.
 */
async function callNativeHost() {
  lastTokenAt = Date.now();
  try {
    const reply = await browser.runtime.sendNativeMessage(NATIVE_HOST, {command: "start"});
    if (reply && reply.status === "error") console.warn("[yt-dl] Native host:", reply.detail);
    if (!reply || typeof reply.token !== "string" || !reply.token) return false;
    api.setToken(reply.token);
    return true;
  } catch (err) {
    // Usually the native host is not installed
    console.warn("[yt-dl] Native host unavailable:", err && err.message);
    return false;
  }
}

/**
 * Run an API call and, when the backend rejects the token, fetch it again from the native
 * host and retry once. Token refetches are spaced at least {@link TOKEN_RETRY_MS} apart.
 * @param {() => Promise<object>} call - API call returning {ok, data} or {ok: false, error, status}.
 * @returns {Promise<object>} The API result.
 */
async function authed(call) {
  const res = await call();
  if (res.status !== 401 || Date.now() - lastTokenAt < TOKEN_RETRY_MS) return res;
  return (await requestToken()) ? call() : res;
}

/**
 * Read settings from storage.local and merge them over the defaults.
 * @returns {Promise<object>} The full settings object.
 */
async function loadSettings() {
  try {
    const stored = await browser.storage.local.get("settings");
    settings = constants.mergeSettings(stored.settings);
  } catch (err) {
    console.warn("[yt-dl] Could not read settings:", err);
  }
  return settings;
}

/**
 * Send the backend-relevant settings to PUT /settings.
 * @returns {Promise<boolean>} True when the backend accepted them.
 */
async function pushSettings() {
  const res = await authed(() =>
    api.putSettings({
      concurrency: settings.concurrency,
      fragments: settings.fragments,
      cookies: settings.cookies,
      auto_update: settings.auto_update,
    }),
  );
  if (!res.ok) console.warn("[yt-dl] Could not push settings:", res.error);
  return res.ok;
}

/**
 * Run the yt-dlp update once per browser session when the backend reports one, the
 * setting allows it and nothing is downloading. Jobs are re-fetched first because the
 * cached list can be stale when the backend has only just come up.
 */
async function maybeAutoUpdate() {
  const health = state.backend.health;
  if (autoUpdateTried || autoUpdateChecking || !settings.auto_update || !health || !health.update_available) return;
  autoUpdateChecking = true;
  try {
    const jobs = await authed(() => api.listJobs());
    if (!jobs.ok || activeCount((jobs.data && jobs.data.jobs) || []) > 0) return;
    autoUpdateTried = true;
    const res = await authed(() => api.update());
    if (!res.ok) console.warn("[yt-dl] yt-dlp auto-update failed:", res.error);
  } finally {
    autoUpdateChecking = false;
  }
  await refreshHealth();
  publish();
}

/**
 * Fetch /health and update the backend state. The answer only counts when its proof
 * shows it knows the install token, so nothing (the token included) is sent to a program
 * squatting on the port. A down > up transition pushes settings and checks for a yt-dlp
 * update (without blocking the caller on the update itself).
 * @returns {Promise<{reachable: boolean|null, health: object|null, error: string|null}>} The new backend state.
 */
async function refreshHealth() {
  const wasReachable = state.backend.reachable;
  lastHealthAt = Date.now();
  if (!api.hasToken()) {
    state.backend = {reachable: false, health: null, error: NO_TOKEN_ERROR};
    return backendSnapshot();
  }
  const nonce = api.newNonce();
  const res = await api.health(nonce);
  if (!res.ok || !res.data) {
    state.backend = {reachable: false, health: null, error: null};
    return backendSnapshot();
  }
  if (!(await api.verifyProof(nonce, res.data.proof))) {
    state.backend = {reachable: false, health: null, error: FOREIGN_ERROR};
    return backendSnapshot();
  }
  const health = {...res.data};
  delete health.proof;
  state.backend = {reachable: true, health, error: null};
  if (!wasReachable) await pushSettings();
  maybeAutoUpdate().catch((err) => console.warn("[yt-dl] Auto-update check failed:", err));
  return backendSnapshot();
}

/**
 * Ask the native host for the token (which also starts the backend), then retry /health
 * every 5s for up to 30s. Concurrent callers share one attempt.
 * @returns {Promise<{reachable: boolean|null, health: object|null, error: string|null}>} The backend state afterwards.
 */
function ensureBackend() {
  if (!ensurePromise) {
    backendStarting = true;
    publish();
    ensurePromise = startBackend()
      .finally(() => {
        ensurePromise = null;
        backendStarting = false;
        publish();
      })
      .then(() => backendSnapshot());
  }
  return ensurePromise;
}

/**
 * Body of {@link ensureBackend}.
 */
async function startBackend() {
  if (api.hasToken() && (await refreshHealth()).reachable) return;
  await requestToken();
  const deadline = Date.now() + START_TIMEOUT_MS;
  while (Date.now() < deadline) {
    const backend = await refreshHealth();
    // Without a token, or with the port taken by something else, waiting cannot help
    if (backend.reachable || backend.error) break;
    await delay(START_RETRY_MS);
  }
  poll();
}

/**
 * Raise a desktop notification for a job that just completed or failed.
 * @param {object} job - Job from GET /jobs.
 */
function notifyJob(job) {
  if (!settings.notifications) return;
  const name = job.title || job.filename || job.url;
  const done = job.status === "complete";
  browser.notifications
    .create(NOTIFICATION_PREFIX + job.id, {
      type: "basic",
      iconUrl: browser.runtime.getURL("icons/icon-96.png"),
      title: done ? "Download complete" : "Download failed",
      message: done ? `${name}\nClick to show in folder.` : `${name}\n${job.error || "Unknown error"}`,
    })
    .catch((err) => console.warn("[yt-dl] Notification failed:", err));
}

/**
 * Raise one summary notification for a playlist or channel group that has just finished.
 * @param {{id: string, title: string, jobs: Array<object>}} group - The group and all its jobs.
 */
function notifyGroup(group) {
  if (!settings.notifications) return;
  const done = group.jobs.filter((job) => job.status === "complete").length;
  const failed = group.jobs.filter((job) => job.status === "error").length;
  const summary = `Downloaded ${done} of ${group.jobs.length}${failed ? `, ${failed} failed` : ""}.`;
  browser.notifications
    .create(GROUP_NOTIFICATION_PREFIX + group.id, {
      type: "basic",
      iconUrl: browser.runtime.getURL("icons/icon-96.png"),
      title: failed ? "Downloads finished with errors" : "Downloads complete",
      message: done ? `${group.title}\n${summary} Click to show in folder.` : `${group.title}\n${summary}`,
    })
    .catch((err) => console.warn("[yt-dl] Notification failed:", err));
}

/**
 * Notify on transitions into complete or error since the previous poll, including jobs
 * that appeared and finished between two polls. Single videos notify per job; a playlist
 * or channel group notifies once, when its last active item finishes, so a long list does
 * not flood the notification centre.
 * @param {Array<object>} jobs - Jobs from the current poll.
 */
function handleTransitions(jobs) {
  const next = new Map(jobs.map((job) => [job.id, job.status]));
  if (lastStatuses) {
    const groups = new Map();
    for (const job of jobs) {
      const prev = lastStatuses.get(job.id);
      const finishedNow = prev !== job.status && (job.status === "complete" || job.status === "error");
      if (!job.group_id) {
        if (finishedNow) notifyJob(job);
        continue;
      }
      let group = groups.get(job.group_id);
      if (!group) {
        group = {id: job.group_id, title: job.group_title || "Playlist", jobs: [], finishedNow: false};
        groups.set(job.group_id, group);
      }
      group.jobs.push(job);
      if (finishedNow) group.finishedNow = true;
    }
    for (const group of groups.values()) {
      if (group.finishedNow && activeCount(group.jobs) === 0) notifyGroup(group);
    }
  }
  lastStatuses = next;
}

/**
 * Send a message to the popup and every YouTube tab, ignoring "no receiver" errors.
 * @param {object} message - Message to broadcast.
 */
async function broadcast(message) {
  browser.runtime.sendMessage(message).catch(() => {});
  let tabs = [];
  try {
    tabs = await browser.tabs.query({url: ["*://*.youtube.com/*"]});
  } catch (err) {
    console.warn("[yt-dl] Could not list tabs:", err);
  }
  for (const tab of tabs) browser.tabs.sendMessage(tab.id, message).catch(() => {});
}

/**
 * Update the badge and broadcast jobsUpdated when the state differs from the last broadcast.
 */
function publish() {
  const count = activeCount(state.jobs);
  const badge = count > 0 ? String(count) : "";
  if (badge !== lastBadge) {
    lastBadge = badge;
    browser.browserAction.setBadgeText({text: badge});
  }
  const message = {type: "jobsUpdated", jobs: state.jobs, backend: backendSnapshot()};
  const serialised = JSON.stringify(message);
  if (serialised === lastBroadcast) return;
  lastBroadcast = serialised;
  broadcast(message);
}

/**
 * Schedule the next poll: every second while anything is active, otherwise every 30s
 * (the heartbeat that keeps the idle-timeout backend alive).
 * @param {number} [ms] - Explicit delay; defaults to the active/idle interval.
 */
function schedulePoll(ms) {
  clearTimeout(pollTimer);
  const wait = ms !== undefined ? ms : activeCount(state.jobs) > 0 ? POLL_ACTIVE_MS : POLL_IDLE_MS;
  pollTimer = setTimeout(poll, wait);
}

/**
 * Fetch GET /jobs, refresh health when stale or after an outage, then publish. Calls
 * made while a poll is in flight collapse into one follow-up poll.
 */
async function poll() {
  if (polling) {
    pollQueued = true;
    return;
  }
  polling = true;
  clearTimeout(pollTimer);
  try {
    // Health first: job requests carry the token, so they only go to a verified backend
    if (!state.backend.reachable || Date.now() - lastHealthAt > HEALTH_REFRESH_MS) await refreshHealth();
    const res = state.backend.reachable ? await authed(() => api.listJobs()) : {ok: false};
    if (res.ok) {
      const jobs = res.data && Array.isArray(res.data.jobs) ? res.data.jobs : [];
      handleTransitions(jobs);
      state.jobs = jobs;
    } else if (!res.status) {
      // No HTTP status means nothing answered: the backend is gone and so is its in-memory queue.
      if (state.backend.reachable) state.backend = {reachable: false, health: null, error: null};
      state.jobs = [];
      lastStatuses = null;
    }
    publish();
  } catch (err) {
    console.warn("[yt-dl] Poll failed:", err);
  } finally {
    polling = false;
    const again = pollQueued;
    pollQueued = false;
    schedulePoll(again ? 0 : undefined);
  }
}

/**
 * Run a backend call against a verified backend, starting it first when it is not known
 * to be up; when nothing answers, start the backend once and retry.
 * @param {() => Promise<object>} call - API call returning {ok, data} or {ok: false, error, status}.
 * @returns {Promise<object>} The API result.
 */
async function withBackend(call) {
  if (!state.backend.reachable) {
    const backend = await ensureBackend();
    if (!backend.reachable) return {ok: false, error: backend.error || api.NOT_RUNNING};
  }
  const first = await authed(call);
  if (first.ok || first.status) return first;
  state.backend = {reachable: false, health: null, error: null};
  const backend = await ensureBackend();
  if (!backend.reachable) return {ok: false, error: backend.error || api.NOT_RUNNING};
  return authed(call);
}

/**
 * Validate that a value is a non-empty string.
 * @param {unknown} value - Value to check.
 * @returns {boolean} True for non-empty strings.
 */
function isText(value) {
  return typeof value === "string" && value.length > 0;
}

/**
 * Return cached /info data for a URL, dropping expired entries.
 * @param {string} url - Page URL.
 * @returns {object|null} Cached /info body, or null.
 */
function cachedInfo(url) {
  const hit = infoCache.get(url);
  if (!hit) return null;
  if (Date.now() - hit.at > INFO_TTL_MS) {
    infoCache.delete(url);
    return null;
  }
  return hit.data;
}

/**
 * Cache an /info body, evicting the oldest entry once the cache is full.
 * @param {string} url - Page URL.
 * @param {object} data - /info body.
 */
function storeInfo(url, data) {
  infoCache.delete(url);
  if (infoCache.size >= INFO_CACHE_MAX) infoCache.delete(infoCache.keys().next().value);
  infoCache.set(url, {at: Date.now(), data});
}

const HANDLERS = {
  async health() {
    const backend = api.hasToken() ? await refreshHealth() : await ensureBackend();
    if (backend.reachable) poll();
    else publish();
    return {ok: true, data: backend};
  },

  async ensureBackend() {
    return {ok: true, data: await ensureBackend()};
  },

  async info(message) {
    if (!isText(message.url)) return {ok: false, error: "Missing URL", status: 400};
    const hit = cachedInfo(message.url);
    if (hit) return {ok: true, data: hit};
    const res = await withBackend(() => api.info(message.url));
    if (res.ok) storeInfo(message.url, res.data);
    return res;
  },

  async createJobs(message) {
    if (!isText(message.url)) return {ok: false, error: "Missing URL", status: 400};
    if (!message.options || typeof message.options !== "object") {
      return {ok: false, error: "Missing download options", status: 400};
    }
    const items = isText(message.items) ? message.items : undefined;
    const res = await withBackend(() => api.createJobs(message.url, message.options, items));
    if (!res.ok) return res;
    poll();
    return {ok: true, data: {jobs: (res.data && res.data.jobs) || []}};
  },

  async jobAction(message) {
    if (!isText(message.id)) return {ok: false, error: "Missing job id", status: 400};
    if (!JOB_ACTIONS.has(message.action)) return {ok: false, error: "Unknown action", status: 400};
    const res = await withBackend(() => api.jobAction(message.id, message.action));
    if (!res.ok) return res;
    poll();
    return {ok: true, data: {job: res.data && res.data.job}};
  },

  async clearFinished() {
    const res = await withBackend(() => api.clearFinished());
    if (!res.ok) return res;
    poll();
    return {ok: true, data: {removed: (res.data && res.data.removed) || 0}};
  },

  async getJobs() {
    return {ok: true, data: {jobs: state.jobs, backend: backendSnapshot()}};
  },

  async updateYtDlp() {
    const res = await withBackend(() => api.update());
    await refreshHealth();
    publish();
    return res;
  },

  async settingsChanged() {
    await loadSettings();
    const pushed = state.backend.reachable ? await pushSettings() : false;
    return {ok: true, data: {pushed}};
  },
};

browser.runtime.onMessage.addListener((message) => {
  if (!message || typeof message.type !== "string" || !Object.hasOwn(HANDLERS, message.type)) return undefined;
  return HANDLERS[message.type](message).catch((err) => {
    console.error(`[yt-dl] ${message.type} failed:`, err);
    return {ok: false, error: (err && err.message) || "Unexpected error"};
  });
});

browser.notifications.onClicked.addListener((notificationId) => {
  let job = null;
  if (notificationId.startsWith(NOTIFICATION_PREFIX)) {
    const id = notificationId.slice(NOTIFICATION_PREFIX.length);
    job = state.jobs.find((item) => item.id === id && item.status === "complete");
  } else if (notificationId.startsWith(GROUP_NOTIFICATION_PREFIX)) {
    const groupId = notificationId.slice(GROUP_NOTIFICATION_PREFIX.length);
    job = state.jobs.find((item) => item.group_id === groupId && item.status === "complete");
  } else {
    return;
  }
  browser.notifications.clear(notificationId);
  if (!job || !state.backend.reachable) return;
  authed(() => api.jobAction(job.id, "reveal")).then((res) => {
    if (!res.ok) console.warn("[yt-dl] Could not show file:", res.error);
  });
});

browser.browserAction.setBadgeBackgroundColor({color: "#cc0000"});

loadSettings()
  .then(() => ensureBackend())
  .catch((err) => console.warn("[yt-dl] Startup failed:", err))
  .finally(() => schedulePoll(0));
