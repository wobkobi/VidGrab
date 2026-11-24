// Background script: the only HTTP client of the backend. Routes runtime messages, polls
// the job queue, broadcasts jobsUpdated, drives the badge and desktop notifications.
const {constants, api, format} = YTDL;

const NATIVE_HOST = "vidgrab";
const POLL_ACTIVE_MS = 1000;
// Poll (and refresh /health) this often while the helper downloads yt-dlp and ffmpeg
const POLL_SETUP_MS = 2000;
const POLL_IDLE_MS = 30000;
// While the helper isn't installed, look for it this often, so running the installer is picked
// up without a Firefox restart or a click. A missing native host fails on a registry lookup,
// so the check is cheap.
const POLL_MISSING_MS = 3000;
const HEALTH_REFRESH_MS = 30000;
const START_RETRY_MS = 5000;
const START_TIMEOUT_MS = 30000;
const INFO_TTL_MS = 10 * 60 * 1000;
const INFO_CACHE_MAX = 50;
// Minimum gap between native-host token requests triggered by a rejected token
const TOKEN_RETRY_MS = 10000;
const NOTIFICATION_PREFIX = "ytdl-job:";
const GROUP_NOTIFICATION_PREFIX = "ytdl-group:";
const NO_TOKEN_ERROR = "Could not get the backend key from the VidGrab helper. Run the helper once and restart Firefox.";
const FOREIGN_ERROR = "Another program is using the helper's port and is not the VidGrab helper.";
const JOB_ACTIONS = new Set(["pause", "resume", "cancel", "retry", "open", "reveal"]);
const ACTIVE = new Set(constants.ACTIVE_STATUSES);

// `code` names a known failure the UI handles specially (constants.HELPER_MISSING), with
// `detail` as the user-facing next step.
const state = {jobs: [], backend: {reachable: false, health: null, error: null, code: null, detail: null}};
// True while ensureBackend runs (and before the first attempt), so the UI can say
// "Starting backend..." instead of "Backend not running" during a normal start.
let backendStarting = true;
let settings = constants.mergeSettings(undefined);
let lastBroadcast = "";
let lastBadge = null;
// Status per job id from the previous poll; null until the first poll so jobs that
// finished before the browser started do not raise notifications.
let lastStatuses = null;
// Jobs already notified for their current finish, so no code path can notify one twice
const notified = new Set();
const NOTIFIED_MAX = 1000;
let lastHealthAt = 0;
let pollTimer = null;
let polling = false;
let pollQueued = false;
let ensurePromise = null;
let tokenPromise = null;
let lastTokenAt = 0;
// Code from the last failed native host call (constants.nativeErrorCode), null after a reply
let nativeError = null;
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
 * while a start attempt is still running, `error` explains a known cause of failure and
 * `code`/`detail` mark the helper_missing case.
 * @returns {{reachable: boolean|null, health: object|null, error: string|null, code: string|null, detail: string|null}} Copy of the backend state.
 */
function backendSnapshot() {
  const reachable = state.backend.reachable ? true : backendStarting ? null : false;
  const {health, error} = state.backend;
  return {reachable, health, error, code: state.backend.code || null, detail: state.backend.detail || null};
}

/**
 * Whether the backend is still downloading yt-dlp and ffmpeg on first run.
 * @returns {boolean} True while /health.tools.state is "installing".
 */
function toolsInstalling() {
  const setup = state.backend.reachable ? format.setupStatus(state.backend.health) : null;
  return Boolean(setup && setup.state === "installing");
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
    nativeError = null;
    if (reply && reply.status === "error") console.warn("[yt-dl] Native host:", reply.detail);
    if (!reply || typeof reply.token !== "string" || !reply.token) return false;
    api.setToken(reply.token);
    api.setPort(reply.port);
    return true;
  } catch (err) {
    nativeError = constants.nativeErrorCode(err);
    console.warn("[yt-dl] Native host unavailable:", err && err.message);
    return false;
  }
}

/**
 * Let the window of a file the helper opens next come to the front. The helper's server has
 * no right to take the foreground on Windows, so a player or Explorer window it opens would
 * stay behind Firefox; the native host, freshly started by Firefox, can grant that right.
 * Failures are ignored: the file still opens, just in the background.
 * @returns {Promise<void>} Resolves once the host has answered or failed.
 */
async function allowForeground() {
  try {
    await browser.runtime.sendNativeMessage(NATIVE_HOST, {command: "foreground"});
  } catch (err) {
    console.warn("[yt-dl] Could not allow the foreground:", err && err.message);
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
      // Helpers before 2.10.0 ignore these and keep 200, 1000 and 500
      list_limit: settings.list_limit,
      group_limit: settings.group_limit,
      keep_finished: settings.keep_finished,
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
    state.backend =
      nativeError === constants.HELPER_MISSING
        ? {
            reachable: false,
            health: null,
            error: constants.HELPER_MISSING_ERROR,
            code: constants.HELPER_MISSING,
            detail: constants.HELPER_MISSING_DETAIL,
          }
        : {reachable: false, health: null, error: NO_TOKEN_ERROR};
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
  // At most one notification per finish; handleTransitions forgets a job once it runs again
  if (notified.has(job.id)) return;
  if (notified.size >= NOTIFIED_MAX) notified.clear();
  notified.add(job.id);
  console.info(`[yt-dl] Notifying ${job.status} for job ${job.id}`);
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
      if (ACTIVE.has(job.status)) notified.delete(job.id);
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
 * Schedule the next poll: every second while anything is active, every 2s while the
 * helper sets up yt-dlp and ffmpeg, otherwise every 30s (the heartbeat that keeps the
 * idle-timeout backend alive).
 * @param {number} [ms] - Explicit delay; defaults to the active/setup/idle interval.
 */
function schedulePoll(ms) {
  clearTimeout(pollTimer);
  let wait = ms;
  if (wait === undefined) {
    if (activeCount(state.jobs) > 0) wait = POLL_ACTIVE_MS;
    else if (toolsInstalling()) wait = POLL_SETUP_MS;
    else if (state.backend.code === constants.HELPER_MISSING) wait = POLL_MISSING_MS;
    else wait = POLL_IDLE_MS;
  }
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
    // Health first: job requests carry the token, so they only go to a verified backend.
    // During first-run setup it is refreshed on every poll to track the progress.
    if (state.backend.code === constants.HELPER_MISSING && !ensurePromise) {
      // Asking the native host also starts the backend once the helper has been installed
      await requestToken();
    }
    if (!state.backend.reachable || toolsInstalling() || Date.now() - lastHealthAt > HEALTH_REFRESH_MS) {
      await refreshHealth();
    }
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
 * Turn a failed start into an API-style error, keeping the helper_missing code and detail.
 * @param {{error: string|null, code: string|null, detail: string|null}} backend - Snapshot from {@link ensureBackend}.
 * @returns {{ok: false, error: string, code?: string, detail?: string}} Error result.
 */
function startFailure(backend) {
  const res = {ok: false, error: backend.error || api.NOT_RUNNING};
  if (backend.code) res.code = backend.code;
  if (backend.detail) res.detail = backend.detail;
  return res;
}

/**
 * Replace the backend's generic 503 "Setting up yt-dlp and ffmpeg" with the current
 * progress, and refresh health so polling switches to the setup interval.
 * @param {object} res - API result with status 503.
 * @returns {Promise<object>} The result, with the progress text when setup is running.
 */
async function withSetupProgress(res) {
  await refreshHealth();
  poll();
  const setup = state.backend.reachable ? format.setupStatus(state.backend.health) : null;
  return setup ? {...res, error: setup.text} : res;
}

/**
 * Run a backend call against a verified backend, starting it first when it is not known
 * to be up; when nothing answers, start the backend once and retry. A 503 during first-run
 * setup comes back with the setup progress as its message.
 * @param {() => Promise<object>} call - API call returning {ok, data} or {ok: false, error, status}.
 * @returns {Promise<object>} The API result.
 */
async function withBackend(call) {
  if (!state.backend.reachable) {
    const backend = await ensureBackend();
    if (!backend.reachable) return startFailure(backend);
  }
  const first = await authed(call);
  if (first.status === 503) return withSetupProgress(first);
  if (first.ok || first.status) return first;
  state.backend = {reachable: false, health: null, error: null};
  const backend = await ensureBackend();
  if (!backend.reachable) return startFailure(backend);
  const second = await authed(call);
  return second.status === 503 ? withSetupProgress(second) : second;
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
    const res = await withBackend(() => api.info(message.url, settings.list_limit));
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
    const data = {jobs: (res.data && res.data.jobs) || []};
    // Set when a list had more videos than one download may queue; only the first were queued
    if (res.data && Number.isInteger(res.data.capped)) {
      data.capped = res.data.capped;
      if (Number.isInteger(res.data.next)) data.next = res.data.next;
    }
    return {ok: true, data};
  },

  async jobAction(message) {
    if (!isText(message.id)) return {ok: false, error: "Missing job id", status: 400};
    if (!JOB_ACTIONS.has(message.action)) return {ok: false, error: "Unknown action", status: 400};
    if (message.action === "open" || message.action === "reveal") await allowForeground();
    const res = await withBackend(() => api.jobAction(message.id, message.action));
    if (!res.ok) return res;
    poll();
    return {ok: true, data: {job: res.data && res.data.job}};
  },

  async clearFinished(message) {
    const res = await withBackend(() => api.clearFinished(message.failed === true));
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
    const listLimit = settings.list_limit;
    await loadSettings();
    // Cached listings were cut at the old limit
    if (settings.list_limit !== listLimit) infoCache.clear();
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
  allowForeground()
    .then(() => authed(() => api.jobAction(job.id, "reveal")))
    .then((res) => {
      if (!res.ok) console.warn("[yt-dl] Could not show file:", res.error);
    });
});

browser.browserAction.setBadgeBackgroundColor({color: "#cc0000"});

const startup = loadSettings()
  .then(() => ensureBackend())
  .catch((err) => {
    console.warn("[yt-dl] Startup failed:", err);
    return null;
  })
  .finally(() => schedulePoll(0));

// A fresh install with no helper opens the download page, since the add-on can do nothing
// without it. Updates and restarts don't, so a removed helper never reopens it.
browser.runtime.onInstalled.addListener(({reason}) => {
  if (reason !== "install" || !constants.HELPER_URL) return;
  startup.then((backend) => {
    if (backend && backend.code === constants.HELPER_MISSING) browser.tabs.create({url: constants.HELPER_URL});
  });
});
