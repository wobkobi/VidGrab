// Content-script runtime shared by every page button: settings, messaging, DOM helpers,
// page detection, SPA navigation and the job-state feed from the background script.
var YTDL = globalThis.YTDL || (globalThis.YTDL = {});

(function () {
  "use strict";

  const content = YTDL.content || (YTDL.content = {});
  const LOG_PREFIX = "[yt-dl]";

  content.ready = Boolean(YTDL.constants && YTDL.format);
  if (!content.ready) {
    console.error(
      `${LOG_PREFIX} shared/constants.js or shared/format.js did not load before the content scripts; ` +
        "download buttons are disabled. Check the content_scripts order in manifest.json.",
    );
    return;
  }

  const { mergeSettings } = YTDL.constants;

  // Event bus: "navigate" (page), "settings" (settings), "jobs" ({jobs, backend}), "dom" ()
  const listeners = new Map();

  /**
   * Subscribe to a content-script event.
   * @param {string} name - Event name.
   * @param {(payload: any) => void} fn - Handler.
   * @returns {() => void} Unsubscribe function.
   */
  content.on = function (name, fn) {
    if (!listeners.has(name)) listeners.set(name, new Set());
    listeners.get(name).add(fn);
    return () => listeners.get(name).delete(fn);
  };

  /**
   * Call every handler for an event, isolating handler errors from each other.
   * @param {string} name - Event name.
   * @param {any} [payload] - Value passed to each handler.
   */
  content.emit = function (name, payload) {
    const set = listeners.get(name);
    if (!set) return;
    for (const fn of [...set]) {
      try {
        fn(payload);
      } catch (err) {
        console.error(`${LOG_PREFIX} "${name}" handler failed:`, err);
      }
    }
  };

  // Settings

  content.settings = mergeSettings(undefined);

  /**
   * Read settings from storage, merged over the defaults.
   * @returns {Promise<object>} Full settings object.
   */
  content.loadSettings = async function () {
    try {
      const stored = await browser.storage.local.get("settings");
      content.settings = mergeSettings(stored.settings);
    } catch (err) {
      console.error(`${LOG_PREFIX} could not read settings:`, err);
    }
    return content.settings;
  };

  /**
   * Store new download defaults, keeping every other stored setting.
   * @param {object} defaults - Download options to save as the defaults.
   * @returns {Promise<boolean>} Whether the write succeeded.
   */
  content.saveDefaults = async function (defaults) {
    try {
      const stored = await browser.storage.local.get("settings");
      const next = mergeSettings(stored.settings);
      next.defaults = { ...next.defaults, ...defaults };
      await browser.storage.local.set({ settings: next });
      content.settings = next;
      return true;
    } catch (err) {
      console.error(`${LOG_PREFIX} could not save defaults:`, err);
      return false;
    }
  };

  browser.storage.onChanged.addListener((changes, area) => {
    if (area !== "local" || !changes.settings) return;
    content.settings = mergeSettings(changes.settings.newValue);
    content.emit("settings", content.settings);
  });

  // Messaging

  /**
   * Send a runtime message to the background script. Never throws.
   * @param {object} message - Message with a `type` field.
   * @returns {Promise<{ok: boolean, data?: any, error?: string, status?: number}>} Background response.
   */
  content.send = async function (message) {
    try {
      const res = await browser.runtime.sendMessage(message);
      if (!res || typeof res !== "object") {
        return { ok: false, error: "No response from the extension" };
      }
      return res;
    } catch (err) {
      // A reloaded or updated extension leaves this content script orphaned
      console.error(`${LOG_PREFIX} message "${message.type}" failed:`, err);
      return { ok: false, error: "Extension was reloaded - refresh this page" };
    }
  };

  // Job state

  content.jobs = [];
  content.backend = null;

  /**
   * Store the latest job list and backend state and notify the button controllers.
   * @param {{jobs?: object[], backend?: {reachable: boolean, health: object|null}}} state - Snapshot from the background script.
   */
  function applyJobState(state) {
    if (!state) return;
    if (Array.isArray(state.jobs)) content.jobs = state.jobs;
    if (state.backend) content.backend = state.backend;
    content.emit("jobs", { jobs: content.jobs, backend: content.backend });
  }

  browser.runtime.onMessage.addListener((message) => {
    if (message && message.type === "jobsUpdated") applyJobState(message);
  });

  /**
   * Whether the backend is known to be down. Unknown state counts as up so buttons stay usable.
   * @returns {boolean} True only after the background script has reported it unreachable.
   */
  content.backendDown = function () {
    return Boolean(content.backend && content.backend.reachable === false);
  };

  /**
   * Whether the background script is still starting the backend.
   * @returns {boolean} True while the reported state is reachable: null.
   */
  content.backendStarting = function () {
    return Boolean(content.backend && content.backend.reachable === null);
  };

  /**
   * Whether the background script found no VidGrab helper registered with Firefox.
   * @returns {boolean} True when the backend state carries the helper_missing code.
   */
  content.helperMissing = function () {
    return Boolean(content.backend && content.backend.code === YTDL.constants.HELPER_MISSING);
  };

  /**
   * Build the one-line helper_missing message, with the download step inline so it is
   * visible without hovering.
   * @param {{error?: string|null, detail?: string|null}} [source] - Backend state or error result carrying the helper_missing code.
   * @returns {string} For example "VidGrab helper not installed. Download VidGrab-helper and run it once."
   */
  content.helperMissingText = function (source) {
    const error = (source && source.error) || YTDL.constants.HELPER_MISSING_ERROR;
    const detail = (source && source.detail) || YTDL.constants.HELPER_MISSING_DETAIL;
    return `${error}. ${detail}.`;
  };

  /**
   * First-run yt-dlp/ffmpeg setup state of a reachable backend.
   * @returns {{state: "installing"|"error", text: string}|null} Status from YTDL.format.setupStatus, or null when ready or unknown.
   */
  content.setupStatus = function () {
    if (!content.backend || !content.backend.reachable) return null;
    return YTDL.format.setupStatus(content.backend.health);
  };

  /**
   * Pull the video id out of any supported YouTube video URL.
   * @param {string} url - Watch, Shorts, embed or youtu.be URL.
   * @returns {string|null} The 11-character id, or null.
   */
  content.videoIdFromUrl = function (url) {
    if (!url) return null;
    const match = /(?:[?&]v=|\/shorts\/|\/embed\/|youtu\.be\/)([\w-]{11})/.exec(url);
    return match ? match[1] : null;
  };

  /**
   * Find the most recent job for a video, including playlist items of the same video.
   * @param {string} videoId - YouTube video id.
   * @returns {object|null} The newest matching job.
   */
  content.latestJobFor = function (videoId) {
    let best = null;
    for (const job of content.jobs) {
      if (content.videoIdFromUrl(job.url) !== videoId) continue;
      if (!best || (job.created_at || 0) > (best.created_at || 0)) best = job;
    }
    return best;
  };

  // DOM helpers

  /**
   * Create an element. Text goes through textContent, never HTML parsing.
   * @param {string} tag - Tag name.
   * @param {object} [props] - `className`, `text`, `on` (event map); anything else becomes an attribute (false/null skipped).
   * @param {Array<Node|string|null|false>} [children] - Child nodes or strings.
   * @returns {HTMLElement} The new element.
   */
  content.el = function (tag, props = {}, children = []) {
    const node = document.createElement(tag);
    for (const [key, value] of Object.entries(props)) {
      if (value === false || value === null || value === undefined) continue;
      if (key === "className") node.className = value;
      else if (key === "text") node.textContent = value;
      else if (key === "on") {
        for (const [evt, fn] of Object.entries(value)) node.addEventListener(evt, fn);
      } else node.setAttribute(key, value === true ? "" : String(value));
    }
    for (const child of children) {
      if (child === null || child === false || child === undefined) continue;
      node.append(child);
    }
    return node;
  };

  /**
   * Build the download arrow icon as inline SVG.
   * @returns {SVGSVGElement} 24x24 icon that inherits currentColor.
   */
  content.downloadIcon = function () {
    const ns = "http://www.w3.org/2000/svg";
    const svg = document.createElementNS(ns, "svg");
    svg.setAttribute("viewBox", "0 0 24 24");
    svg.setAttribute("aria-hidden", "true");
    svg.setAttribute("focusable", "false");
    svg.setAttribute("class", "yt-dl-icon");
    const path = document.createElementNS(ns, "path");
    path.setAttribute("d", "M17 18v1H6v-1h11zm-.5-6.6-.7-.7-3.8 3.7V4h-1v10.4l-3.8-3.8-.7.7 5 5 5-4.9z");
    path.setAttribute("fill", "currentColor");
    svg.append(path);
    return svg;
  };

  /**
   * Whether an element is rendered (YouTube keeps hidden copies of old pages in the DOM).
   * @param {Element} node - Element to test.
   * @returns {boolean} True when it has a layout box.
   */
  content.isVisible = function (node) {
    return Boolean(node && node.isConnected && node.getClientRects().length > 0);
  };

  /**
   * Return the first visible match for a list of selectors, or the first match of any kind
   * when nothing is visible yet.
   * @param {string[]} selectors - Selectors in priority order.
   * @param {ParentNode} [root] - Scope to search in.
   * @returns {Element|null} Best match.
   */
  content.findFirst = function (selectors, root = document) {
    let fallback = null;
    for (const selector of selectors) {
      if (!selector) continue;
      let nodes;
      try {
        nodes = root.querySelectorAll(selector);
      } catch {
        console.warn(`${LOG_PREFIX} invalid selector ignored: ${selector}`);
        continue;
      }
      for (const node of nodes) {
        if (content.isVisible(node)) return node;
        if (!fallback) fallback = node;
      }
    }
    return fallback;
  };

  /**
   * Wait until a selector matches, using a MutationObserver instead of polling.
   * @param {string} selector - CSS selector.
   * @param {number} [timeout] - Give up after this many milliseconds.
   * @param {ParentNode} [root] - Scope to search in.
   * @returns {Promise<Element|null>} The element, or null on timeout.
   */
  content.waitFor = function (selector, timeout = 10000, root = document) {
    const found = root.querySelector(selector);
    if (found) return Promise.resolve(found);
    return new Promise((resolve) => {
      const observer = new MutationObserver(() => {
        const node = root.querySelector(selector);
        if (node) finish(node);
      });
      const timer = setTimeout(() => finish(null), timeout);
      function finish(node) {
        observer.disconnect();
        clearTimeout(timer);
        resolve(node);
      }
      observer.observe(root === document ? document.documentElement : root, { childList: true, subtree: true });
    });
  };

  // Throttled DOM-change feed so controllers can re-inject after YouTube re-renders.
  // One observer for the whole page; handlers run at most every DOM_THROTTLE_MS.
  const DOM_THROTTLE_MS = 250;
  let domTimer = null;
  const domObserver = new MutationObserver(() => {
    if (domTimer) return;
    domTimer = setTimeout(() => {
      domTimer = null;
      content.emit("dom");
    }, DOM_THROTTLE_MS);
  });

  // Page detection

  const CHANNEL_RE = /^\/(@[^/]+|channel\/[\w-]+|c\/[^/]+|user\/[^/]+)(?:\/([^/]+))?\/?$/;
  // Tabs the backend lists as-is; any other channel tab maps to "videos" (matches backend CHANNEL_TABS)
  const CHANNEL_TABS = new Set(["videos", "shorts", "streams"]);

  /**
   * Classify a YouTube URL and derive the canonical URL sent to the backend.
   * Channel pages keep their Shorts or Live tab when viewing it, otherwise map to the Videos tab.
   * @param {string} [href] - URL to classify; defaults to the current page.
   * @returns {{type: "watch"|"shorts"|"playlist"|"channel"|"other", url: string|null, videoId: string|null, href: string}} Page description.
   */
  content.detectPage = function (href = location.href) {
    const page = { type: "other", url: null, videoId: null, href };
    let parsed;
    try {
      parsed = new URL(href);
    } catch {
      return page;
    }
    const path = parsed.pathname;
    const origin = "https://www.youtube.com";

    if (path === "/watch") {
      const id = parsed.searchParams.get("v");
      if (id && /^[\w-]{11}$/.test(id)) {
        return { ...page, type: "watch", videoId: id, url: `${origin}/watch?v=${id}` };
      }
      return page;
    }

    const shorts = /^\/shorts\/([\w-]{11})/.exec(path);
    if (shorts) {
      return { ...page, type: "shorts", videoId: shorts[1], url: `${origin}/shorts/${shorts[1]}` };
    }

    if (path === "/playlist") {
      const list = parsed.searchParams.get("list");
      if (list && /^[\w-]+$/.test(list)) {
        return { ...page, type: "playlist", url: `${origin}/playlist?list=${list}` };
      }
      return page;
    }

    const channel = CHANNEL_RE.exec(path);
    if (channel) {
      const tab = CHANNEL_TABS.has(channel[2]) ? channel[2] : "videos";
      return { ...page, type: "channel", url: `${origin}/${channel[1]}/${tab}` };
    }
    return page;
  };

  // SPA navigation: YouTube fires yt-navigate-finish, but Shorts scrolling and some history
  // updates change the URL without it, so a cheap href check backs it up.
  content.page = content.detectPage();
  const URL_CHECK_MS = 500;

  /**
   * Emit "navigate" when the URL has changed since the last check.
   */
  function checkNavigation() {
    if (location.href === content.page.href) return;
    content.page = content.detectPage();
    content.emit("navigate", content.page);
  }

  document.addEventListener("yt-navigate-finish", checkNavigation);
  window.addEventListener("popstate", checkNavigation);
  setInterval(checkNavigation, URL_CHECK_MS);

  /**
   * Start the runtime once every content script has registered its handlers.
   * Called from the last content script (lists.js).
   */
  content.start = async function () {
    await content.loadSettings();
    content.emit("settings", content.settings);
    content.emit("navigate", content.page);
    domObserver.observe(document.documentElement, { childList: true, subtree: true });

    const res = await content.send({ type: "getJobs" });
    if (res.ok) applyJobState(res.data);
  };
})();
