// Shared enums, labels and default settings for the background, popup, options and content scripts.
var YTDL = globalThis.YTDL || (globalThis.YTDL = {});

YTDL.constants = (function () {
  // Bump when a stored value needs migrating; see mergeSettings
  const SETTINGS_VERSION = 2;

  const DEFAULT_SETTINGS = Object.freeze({
    version: SETTINGS_VERSION,
    defaults: Object.freeze({
      kind: "video",
      quality: "best",
      codec: "any",
      container: "mp4",
      audio_format: "mp3",
      audio_bitrate: "192",
      // Target file size in MB for video (0 = no limit); see MAX_SIZES
      max_size_mb: 0,
    }),
    concurrency: 2,
    fragments: 4,
    cookies: false,
    auto_update: true,
    notifications: true,
    selectors: Object.freeze({
      watchContainer: "#top-level-buttons-computed",
      // After YouTube's own buttons, so the like/dislike pill stays the row's first child and
      // never moves once the page has rendered (Return YouTube Dislike anchors its ratio bar
      // to it once, at load)
      watchInsertion: "last",
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

  // Codec choices while a max size is set: the output is always re-encoded or picked as one of
  // these, so a preference like VP9 has no meaning there
  const SIZED_CODECS = [
    {value: "any", label: "H.264 (plays everywhere)"},
    {value: "av1", label: "AV1 (sharper, newer devices)"},
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

  // Max size presets for common upload limits. Email is 18 MB because attachments grow by about a
  // third when encoded, so a 25 MB limit (Gmail) holds about 18 MB of file. A saved size that is
  // no longer a preset shows as Custom. "custom" takes a typed MB value.
  const MAX_SIZE_CUSTOM = "custom";
  const MAX_SIZES = [
    {value: "0", label: "Off"},
    {value: "10", label: "10 MB (Discord)"},
    {value: "16", label: "16 MB (WhatsApp)"},
    {value: "18", label: "18 MB (Email)"},
    {value: "50", label: "50 MB (Discord Nitro Basic)"},
    {value: "500", label: "500 MB (Discord Nitro)"},
    {value: MAX_SIZE_CUSTOM, label: "Custom"},
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

  const LIMITS = {concurrency: {min: 1, max: 5}, fragments: {min: 1, max: 8}, max_size_mb: {min: 1, max: 4000}};

  // Download page for the native helper: the latest GitHub release carries every platform's
  // build next to the signed add-on. Empty would show the instruction as plain text instead.
  const HELPER_URL = "https://github.com/wobkobi/VidGrab/releases/latest";

  // Backend state code for "the native host is not registered with Firefox"
  const HELPER_MISSING = "helper_missing";
  const HELPER_MISSING_ERROR = "VidGrab helper not installed";
  const HELPER_MISSING_DETAIL = "Download the VidGrab helper and run it once";

  // Oldest helper this add-on fully works with (2.6.0 added the channel name on each job). Raise
  // it when the add-on starts relying on something a newer helper adds. It isn't the add-on's own
  // version: every release rebuilds the helper, so that would ask for a reinstall on every
  // add-on-only patch.
  const MIN_HELPER_VERSION = "2.6.0";

  /**
   * Report whether a running helper is older than {@link MIN_HELPER_VERSION}. A version that isn't
   * dotted numbers, such as "dev" from a local go build, counts as current.
   * @param {unknown} version - The helper's version from /health.
   * @returns {boolean} True when the helper should be updated.
   */
  function helperOutdated(version) {
    if (typeof version !== "string" || !/^\d+(\.\d+)*$/.test(version)) return false;
    const have = version.split(".").map(Number);
    const need = MIN_HELPER_VERSION.split(".").map(Number);
    for (let i = 0; i < Math.max(have.length, need.length); i++) {
      if ((have[i] ?? 0) !== (need[i] ?? 0)) return (have[i] ?? 0) < (need[i] ?? 0);
    }
    return false;
  }

  /**
   * Classify a sendNativeMessage rejection. Firefox rejects with "No such native
   * application <name>" when no manifest is registered for the host; anything else (a
   * crashed host, "Attempt to postMessage on disconnected port") is a generic failure.
   * @param {unknown} err - The rejection value.
   * @returns {string|null} {@link HELPER_MISSING} when the host is not installed, otherwise null.
   */
  function nativeErrorCode(err) {
    let message = "";
    if (typeof err === "string") message = err;
    else if (err && typeof err.message === "string") message = err.message;
    return /no such native application/i.test(message) ? HELPER_MISSING : null;
  }

  /**
   * Report whether a download needs ffmpeg: MP3 conversion, video above 720p (or "best"),
   * which arrives as separate streams that must be merged, any video max size, since
   * fitting a size may re-encode, and any clip, which ffmpeg cuts.
   * @param {{kind?: string, quality?: string, audio_format?: string, max_size_mb?: number, clip_start?: number, clip_end?: number}} options - Job options.
   * @returns {boolean} True when the backend must have ffmpeg to run the job.
   */
  function NEEDS_FFMPEG(options) {
    if (!options) return false;
    if (Number(options.clip_start) > 0 || Number(options.clip_end) > 0) return true;
    if (options.kind === "audio") return options.audio_format === "mp3";
    if (Number(options.max_size_mb) > 0) return true;
    if (options.quality === "best") return true;
    return Number(options.quality) > 720;
  }

  /**
   * Parse a typed custom max size. Only whole numbers within {@link LIMITS} count, so
   * "12.5", "0" and "5000" are rejected rather than rounded or clamped behind the user's back.
   * @param {unknown} raw - Input value (string or number).
   * @returns {number|null} Size in MB, or null when invalid.
   */
  function parseMaxSize(raw) {
    const text = typeof raw === "number" ? String(raw) : typeof raw === "string" ? raw.trim() : "";
    if (!/^\d+$/.test(text)) return null;
    const n = Number(text);
    const {min, max} = LIMITS.max_size_mb;
    return n >= min && n <= max ? n : null;
  }

  /**
   * Map a stored max size onto its {@link MAX_SIZES} choice.
   * @param {unknown} mb - Size in MB (0 = off).
   * @returns {string} The preset's value, "0" for off, or {@link MAX_SIZE_CUSTOM}.
   */
  function maxSizeChoice(mb) {
    const n = Number(mb);
    if (!Number.isFinite(n) || n <= 0) return "0";
    const preset = MAX_SIZES.find((item) => item.value !== MAX_SIZE_CUSTOM && Number(item.value) === n);
    return preset ? preset.value : MAX_SIZE_CUSTOM;
  }

  /**
   * Resolve a {@link MAX_SIZES} choice plus the custom field into a size.
   * @param {string} choice - Select value.
   * @param {unknown} custom - Custom field value, used only for {@link MAX_SIZE_CUSTOM}.
   * @returns {number|null} Size in MB (0 = off), or null when the custom value is invalid.
   */
  function maxSizeValue(choice, custom) {
    if (choice === MAX_SIZE_CUSTOM) return parseMaxSize(custom);
    const n = Number(choice);
    return Number.isInteger(n) && n > 0 ? n : 0;
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
   * Validate a stored max size: whole MB within {@link LIMITS}, clamped down to the max.
   * Anything non-numeric, zero or negative turns the limit off.
   * @param {unknown} value - Stored value.
   * @returns {number} Size in MB, or 0 for off.
   */
  function storedMaxSize(value) {
    if (typeof value !== "number" && typeof value !== "string") return 0;
    const n = Math.round(Number(value));
    if (!Number.isFinite(n) || n < LIMITS.max_size_mb.min) return 0;
    return Math.min(LIMITS.max_size_mb.max, n);
  }

  /**
   * Merge a stored settings object over {@link DEFAULT_SETTINGS}. `defaults` and
   * `selectors` merge key by key; unknown or invalid values fall back to the default
   * so a corrupted store never reaches the backend.
   *
   * Migration to version 2: the watch button used to go before the like/dislike pill by
   * default, which left Return YouTube Dislike's ratio bar under it. Settings saved before
   * version 2 that still say "first" move to "last"; a "first" saved from version 2 on is a
   * deliberate choice and stays.
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
    const storedVersion = Number.isInteger(src.version) ? src.version : 1;
    const insertion = storedVersion < 2 && ss.watchInsertion === "first" ? "last" : ss.watchInsertion;

    return {
      version: SETTINGS_VERSION,
      defaults: {
        kind: pickEnum(sd.kind, KINDS, d.defaults.kind),
        quality: pickEnum(sd.quality, QUALITIES, d.defaults.quality),
        codec: pickEnum(sd.codec, CODECS, d.defaults.codec),
        container: pickEnum(sd.container, CONTAINERS, d.defaults.container),
        audio_format: pickEnum(sd.audio_format, AUDIO_FORMATS, d.defaults.audio_format),
        audio_bitrate: pickEnum(sd.audio_bitrate, AUDIO_BITRATES, d.defaults.audio_bitrate),
        max_size_mb: storedMaxSize(sd.max_size_mb),
      },
      concurrency: clampInt(src.concurrency, LIMITS.concurrency, d.concurrency),
      fragments: clampInt(src.fragments, LIMITS.fragments, d.fragments),
      cookies: bool(src.cookies, d.cookies),
      auto_update: bool(src.auto_update, d.auto_update),
      notifications: bool(src.notifications, d.notifications),
      selectors: {
        watchContainer: text(ss.watchContainer, d.selectors.watchContainer),
        watchInsertion: pickEnum(insertion, WATCH_INSERTIONS, d.selectors.watchInsertion),
        shortsBar: text(ss.shortsBar, d.selectors.shortsBar),
      },
    };
  }

  return {
    SETTINGS_VERSION,
    DEFAULT_SETTINGS,
    KINDS,
    QUALITIES,
    CODECS,
    SIZED_CODECS,
    CONTAINERS,
    AUDIO_FORMATS,
    AUDIO_BITRATES,
    MAX_SIZES,
    MAX_SIZE_CUSTOM,
    WATCH_INSERTIONS,
    ACTIVE_STATUSES,
    STATUS_LABELS,
    LIMITS,
    HELPER_URL,
    HELPER_MISSING,
    HELPER_MISSING_ERROR,
    HELPER_MISSING_DETAIL,
    MIN_HELPER_VERSION,
    helperOutdated,
    NEEDS_FFMPEG,
    parseMaxSize,
    maxSizeChoice,
    maxSizeValue,
    nativeErrorCode,
    mergeSettings,
  };
})();

if (typeof module !== "undefined") module.exports = YTDL.constants;
