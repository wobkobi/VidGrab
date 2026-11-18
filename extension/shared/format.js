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

  return {bytes, eta, percent, parseRange, statusLabel};
})();

if (typeof module !== "undefined") module.exports = YTDL.format;
