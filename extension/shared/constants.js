// Shared enums, labels and default settings for the background, popup, options and content scripts.
var YTDL = globalThis.YTDL || (globalThis.YTDL = {});

YTDL.constants = (function () {
  const DEFAULT_SETTINGS = Object.freeze({
    defaults: Object.freeze({
      kind: "video",
      quality: "best",
      codec: "any",
      container: "mp4",
      audio_format: "mp3",
      audio_bitrate: "192",
    }),
    concurrency: 2,
    fragments: 4,
    cookies: false,
    auto_update: true,
    notifications: true,
    selectors: Object.freeze({
      watchContainer: "#top-level-buttons-computed",
      watchInsertion: "first",
      shortsBar: "#actions.ytd-reel-player-overlay-renderer, ytd-reel-player-overlay-renderer #actions",
    }),
  });

  const KINDS = [
    {value: "video", label: "Video"},
    {value: "audio", label: "Audio"},
  ];

  const QUALITIES = [
    {value: "best", label: "Best available"},
    {value: "2160", label: "2160p (4K)"},
    {value: "1440", label: "1440p"},
    {value: "1080", label: "1080p"},
    {value: "720", label: "720p"},
    {value: "480", label: "480p"},
    {value: "360", label: "360p"},
  ];

  const CODECS = [
    {value: "any", label: "Any codec"},
    {value: "h264", label: "H.264 (most compatible)"},
    {value: "vp9", label: "VP9"},
    {value: "av1", label: "AV1"},
  ];

  const CONTAINERS = [
    {value: "mp4", label: "MP4"},
    {value: "mkv", label: "MKV"},
  ];

  const AUDIO_FORMATS = [
    {value: "mp3", label: "MP3"},
    {value: "m4a", label: "M4A"},
  ];

  const AUDIO_BITRATES = [
    {value: "320", label: "320 kbps"},
    {value: "192", label: "192 kbps"},
    {value: "128", label: "128 kbps"},
  ];

  const WATCH_INSERTIONS = [
    {value: "first", label: "Before the other buttons"},
    {value: "last", label: "After the other buttons"},
  ];

  const ACTIVE_STATUSES = ["queued", "downloading", "processing", "paused"];

  const STATUS_LABELS = {
    queued: "Queued",
    downloading: "Downloading",
    processing: "Processing",
    paused: "Paused",
    complete: "Complete",
    error: "Failed",
    cancelled: "Cancelled",
  };

  const LIMITS = {concurrency: {min: 1, max: 5}, fragments: {min: 1, max: 8}};

  /**
   * Report whether a download needs ffmpeg: MP3 conversion, and video above 720p
   * (or "best"), which arrives as separate streams that must be merged.
   * @param {{kind?: string, quality?: string, audio_format?: string}} options - Job options.
   * @returns {boolean} True when the backend must have ffmpeg to run the job.
   */
  function NEEDS_FFMPEG(options) {
    if (!options) return false;
    if (options.kind === "audio") return options.audio_format === "mp3";
    if (options.quality === "best") return true;
    return Number(options.quality) > 720;
  }

  /**
   * Check that a value is a plain object (not an array or null).
   * @param {unknown} value - Value to test.
   * @returns {boolean} True for plain objects.
   */
  function isObject(value) {
    return value !== null && typeof value === "object" && !Array.isArray(value);
  }

  /**
   * Pick a value from an enum list, falling back when the stored value is unknown.
   * @param {unknown} value - Stored value.
   * @param {Array<{value: string}>} list - Allowed values.
   * @param {string} fallback - Default when the value is not in the list.
   * @returns {string} A valid enum value.
   */
  function pickEnum(value, list, fallback) {
    return list.some((item) => item.value === value) ? value : fallback;
  }

  /**
   * Clamp a stored number into its allowed range, falling back when it is not a number.
   * @param {unknown} value - Stored value.
   * @param {{min: number, max: number}} range - Inclusive bounds.
   * @param {number} fallback - Default when the value is not numeric.
   * @returns {number} An integer within the range.
   */
  function clampInt(value, range, fallback) {
    const n = Math.round(Number(value));
    if (!Number.isFinite(n)) return fallback;
    return Math.min(range.max, Math.max(range.min, n));
  }

  /**
   * Merge a stored settings object over {@link DEFAULT_SETTINGS}. `defaults` and
   * `selectors` merge key by key; unknown or invalid values fall back to the default
   * so a corrupted store never reaches the backend.
   * @param {unknown} stored - Value read from storage.local (may be undefined).
   * @returns {object} A complete, validated settings object.
   */
  function mergeSettings(stored) {
    const src = isObject(stored) ? stored : {};
    const d = DEFAULT_SETTINGS;
    const sd = isObject(src.defaults) ? src.defaults : {};
    const ss = isObject(src.selectors) ? src.selectors : {};
    const text = (value, fallback) => (typeof value === "string" && value.trim() ? value.trim() : fallback);
    const bool = (value, fallback) => (typeof value === "boolean" ? value : fallback);

    return {
      defaults: {
        kind: pickEnum(sd.kind, KINDS, d.defaults.kind),
        quality: pickEnum(sd.quality, QUALITIES, d.defaults.quality),
        codec: pickEnum(sd.codec, CODECS, d.defaults.codec),
        container: pickEnum(sd.container, CONTAINERS, d.defaults.container),
        audio_format: pickEnum(sd.audio_format, AUDIO_FORMATS, d.defaults.audio_format),
        audio_bitrate: pickEnum(sd.audio_bitrate, AUDIO_BITRATES, d.defaults.audio_bitrate),
      },
      concurrency: clampInt(src.concurrency, LIMITS.concurrency, d.concurrency),
      fragments: clampInt(src.fragments, LIMITS.fragments, d.fragments),
      cookies: bool(src.cookies, d.cookies),
      auto_update: bool(src.auto_update, d.auto_update),
      notifications: bool(src.notifications, d.notifications),
      selectors: {
        watchContainer: text(ss.watchContainer, d.selectors.watchContainer),
        watchInsertion: pickEnum(ss.watchInsertion, WATCH_INSERTIONS, d.selectors.watchInsertion),
        shortsBar: text(ss.shortsBar, d.selectors.shortsBar),
      },
    };
  }

  return {
    DEFAULT_SETTINGS,
    KINDS,
    QUALITIES,
    CODECS,
    CONTAINERS,
    AUDIO_FORMATS,
    AUDIO_BITRATES,
    WATCH_INSERTIONS,
    ACTIVE_STATUSES,
    STATUS_LABELS,
    LIMITS,
    NEEDS_FFMPEG,
    mergeSettings,
  };
})();

if (typeof module !== "undefined") module.exports = YTDL.constants;
