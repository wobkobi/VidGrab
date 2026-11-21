// Display formatting for sizes, ETAs, percentages, item ranges and job statuses.
var YTDL = globalThis.YTDL || (globalThis.YTDL = {});

YTDL.format = (function () {
  const UNITS = ["B", "KB", "MB", "GB", "TB"];

  /**
   * Format a byte count with binary units ("12.3 MB").
   * @param {unknown} n - Byte count.
   * @returns {string} Formatted size, or "" when the value is missing or invalid.
   */
  function bytes(n) {
    const value = Number(n);
    if (n === null || n === undefined || n === "" || !Number.isFinite(value) || value < 0) return "";
    let size = value;
    let unit = 0;
    while (size >= 1024 && unit < UNITS.length - 1) {
      size /= 1024;
      unit += 1;
    }
    // Whole bytes never need a decimal; larger units keep one below 100.
    const digits = unit === 0 || size >= 100 ? 0 : 1;
    return `${size.toFixed(digits)} ${UNITS[unit]}`;
  }

  /**
   * Format an ETA. Numbers are seconds ("1:05", "1:02:03"); strings from yt-dlp pass through.
   * @param {unknown} v - Seconds or a preformatted string.
   * @returns {string} Formatted ETA, or "" when unknown.
   */
  function eta(v) {
    if (typeof v === "string") return v.trim();
    const total = Math.round(Number(v));
    if (v === null || v === undefined || !Number.isFinite(total) || total < 0) return "";
    const h = Math.floor(total / 3600);
    const m = Math.floor((total % 3600) / 60);
    const s = String(total % 60).padStart(2, "0");
    return h > 0 ? `${h}:${String(m).padStart(2, "0")}:${s}` : `${m}:${s}`;
  }

  /**
   * Format a 0-100 progress value as a whole percentage, clamped to the range.
   * @param {unknown} n - Progress value.
   * @returns {string} For example "45%".
   */
  function percent(n) {
    const value = Number(n);
    if (!Number.isFinite(value)) return "0%";
    return `${Math.min(100, Math.max(0, Math.floor(value)))}%`;
  }

  /**
   * Validate and normalise a playlist item range such as "1-10, 15". Each part is a
   * 1-based index or an ascending "start-end" pair; whitespace is dropped.
   * @param {unknown} str - User input.
   * @returns {string|null} Normalised range ("1-10,15"), or null when empty or invalid.
   */
  function parseRange(str) {
    if (typeof str !== "string") return null;
    const compact = str.replace(/\s+/g, "");
    if (!compact) return null;
    const parts = [];
    for (const part of compact.split(",")) {
      const match = /^(\d+)(?:-(\d+))?$/.exec(part);
      if (!match) return null;
      const start = Number(match[1]);
      const end = match[2] === undefined ? start : Number(match[2]);
      if (start < 1 || end < start) return null;
      parts.push(start === end ? String(start) : `${start}-${end}`);
    }
    return parts.join(",");
  }

  /**
   * Parse a clip time typed as seconds ("90"), "m:ss" ("1:30") or "h:mm:ss" ("1:02:05").
   * Fractions of a second are dropped, since the backend cuts on whole seconds.
   * @param {unknown} str - User input.
   * @returns {number|null} Whole seconds, null when empty or invalid.
   */
  function parseTime(str) {
    if (typeof str !== "string") return null;
    const compact = str.replace(/\s+/g, "");
    if (!compact) return null;
    // Only the first part may exceed 59, so "90" and "90:00" both read naturally
    const match = /^(\d+)(?::(\d{1,2}))?(?::(\d{1,2}))?(?:\.\d*)?$/.exec(compact);
    if (!match) return null;
    const parts = match.slice(1).filter((part) => part !== undefined).map(Number);
    if (parts.slice(1).some((n) => n > 59)) return null;
    return parts.reduce((total, n) => total * 60 + n, 0);
  }

  /**
   * Format whole seconds as "m:ss" or "h:mm:ss", the form {@link parseTime} reads back.
   * @param {number} seconds - Time in seconds; fractions are dropped.
   * @returns {string} Formatted time.
   */
  function clock(seconds) {
    const total = Math.max(0, Math.floor(Number(seconds) || 0));
    const h = Math.floor(total / 3600);
    const m = Math.floor(total / 60) % 60;
    const s = String(total % 60).padStart(2, "0");
    return h ? `${h}:${String(m).padStart(2, "0")}:${s}` : `${m}:${s}`;
  }

  /**
   * Map a job status to its display label.
   * @param {string} status - Backend job status.
   * @returns {string} Human-readable label.
   */
  function statusLabel(status) {
    const labels = YTDL.constants && YTDL.constants.STATUS_LABELS;
    if (labels && labels[status]) return labels[status];
    if (typeof status !== "string" || !status) return "";
    return status.charAt(0).toUpperCase() + status.slice(1);
  }

  /**
   * Whether a job is being re-encoded to fit its max size. The backend reports this as
   * status "processing" with stage "converting", and `percent` then tracks the conversion.
   * @param {unknown} job - Job from the backend.
   * @returns {boolean} True while converting.
   */
  function isConverting(job) {
    return Boolean(job) && job.status === "processing" && job.stage === "converting";
  }

  /**
   * Whether a downloading job has not received any data yet. yt-dlp first fetches the video's
   * details and picks formats, which takes a few seconds at 0% with no speed; showing that as
   * "0%" looks stuck.
   * @param {unknown} job - Job from the backend.
   * @returns {boolean} True until the first progress report.
   */
  function isPreparing(job) {
    return Boolean(job) && job.status === "downloading" && !(Number(job.percent) > 0) && !job.speed;
  }

  /**
   * Short label for a converting job, for example "Converting 45%".
   * @param {{percent?: unknown}} job - Converting job.
   * @returns {string} Label with the conversion percentage.
   */
  function convertingLabel(job) {
    return `Converting ${percent(job && job.percent)}`;
  }

  /**
   * Progress line for a converting job: percentage, then speed ("2.4x") and ETA when known.
   * @param {{percent?: unknown, speed?: unknown, eta?: unknown}} job - Converting job.
   * @returns {string} For example "Converting 45% · 2.4x · ETA 0:30".
   */
  function convertingMeta(job) {
    const parts = [convertingLabel(job)];
    if (job && typeof job.speed === "string" && job.speed.trim()) parts.push(job.speed.trim());
    const left = eta(job && job.eta);
    if (left) parts.push(`ETA ${left}`);
    return parts.join(" · ");
  }

  /**
   * Describe the first-run yt-dlp/ffmpeg setup reported in `/health.tools`. A missing or
   * malformed `tools` field (older backends) or an unknown state counts as ready.
   * @param {unknown} health - The /health body.
   * @returns {{state: "installing"|"error", text: string}|null} Status line, or null when ready.
   */
  function setupStatus(health) {
    const tools = health && typeof health === "object" ? health.tools : null;
    if (!tools || typeof tools !== "object") return null;
    if (tools.state === "error") {
      const error = typeof tools.error === "string" && tools.error.trim() ? tools.error.trim() : "Unknown error";
      return {state: "error", text: `Setting up yt-dlp and ffmpeg failed: ${error}`};
    }
    if (tools.state !== "installing") return null;
    // Drop trailing dots and ellipses so a step like "Downloading ffmpeg..." does not end up with six
    const step = typeof tools.step === "string" ? tools.step.trim().replace(/(\.|\u2026)+$/, "") : "";
    const label = step || "Setting up yt-dlp and ffmpeg";
    const pct = tools.percent === null || tools.percent === undefined || tools.percent === "" ? NaN : Number(tools.percent);
    return {state: "installing", text: Number.isFinite(pct) ? `${label}... ${percent(pct)}` : `${label}...`};
  }

  /**
   * The YouTube video id in a watch, Shorts, live or youtu.be link, so a job's link can be
   * matched to a tab open on the same video whatever its other parameters (time, playlist).
   * @param {unknown} url - Link to read.
   * @returns {string|null} The video id, or null for anything else (a playlist, a channel, not YouTube).
   */
  function videoId(url) {
    let parsed;
    try {
      parsed = new URL(String(url));
    } catch {
      return null;
    }
    const host = parsed.hostname.replace(/^(www|m|music)\./, "");
    if (host === "youtu.be") return parsed.pathname.split("/")[1] || null;
    if (host !== "youtube.com") return null;
    if (parsed.pathname === "/watch") return parsed.searchParams.get("v") || null;
    const match = /^\/(?:shorts|live)\/([^/]+)/.exec(parsed.pathname);
    return match ? match[1] : null;
  }

  return {bytes, eta, percent, parseRange, parseTime, clock, statusLabel, isConverting, isPreparing, convertingLabel, convertingMeta, setupStatus, videoId};
})();

if (typeof module !== "undefined") module.exports = YTDL.format;
