// Floating download-options panel shared by the watch, Shorts, playlist and channel buttons.
var YTDL = globalThis.YTDL || (globalThis.YTDL = {});

(function () {
  "use strict";

  const content = YTDL.content;
  if (!content || !content.ready) return;

  const { QUALITIES, CODECS, SIZED_CODECS, CONTAINERS, AUDIO_FORMATS, AUDIO_BITRATES, MAX_SIZES, MAX_SIZE_CUSTOM, LIMITS } = YTDL.constants;
  const { NEEDS_FFMPEG, HELPER_MISSING, maxSizeChoice, maxSizeValue } = YTDL.constants;
  const format = YTDL.format;
  const el = content.el;

  const VIEWPORT_MARGIN = 8;
  const ANCHOR_GAP = 6;
  const CLOSE_AFTER_SUCCESS_MS = 1200;
  const OPTION_KEYS = ["kind", "quality", "codec", "container", "audio_format", "audio_bitrate", "max_size_mb"];
  const SIZE_LIMITS = LIMITS.max_size_mb;

  // Map yt-dlp vcodec prefixes onto the codec preference values
  const CODEC_ALIASES = { avc1: "h264", avc: "h264", h264: "h264", vp09: "vp9", vp9: "vp9", av01: "av1", av1: "av1" };

  let state = null;
  let idCounter = 0;

  /**
   * Normalise an /info codec string (e.g. "avc1.640028") to a codec preference value.
   * @param {string} codec - Raw codec string.
   * @returns {string|null} "h264", "vp9", "av1" or null.
   */
  function normaliseCodec(codec) {
    const prefix = String(codec || "")
      .toLowerCase()
      .split(".")[0];
    return CODEC_ALIASES[prefix] || null;
  }

  /**
   * Build a labelled select control.
   * @param {string} label - Visible label.
   * @param {string} name - Option key it edits.
   * @returns {{field: HTMLElement, select: HTMLSelectElement}} Wrapper and select.
   */
  function selectField(label, name) {
    const id = `yt-dl-field-${name}-${++idCounter}`;
    const select = el("select", { className: "yt-dl-select", id, name });
    const field = el("div", { className: "yt-dl-field" }, [el("label", { className: "yt-dl-label", for: id, text: label }), select]);
    return { field, select };
  }

  /**
   * Replace a select's options.
   * @param {HTMLSelectElement} select - Target select.
   * @param {Array<{value: string, label: string, disabled?: boolean}>} options - New options.
   * @param {string} value - Value to select; falls back to the first enabled option.
   */
  function fillSelect(select, options, value) {
    select.replaceChildren(
      ...options.map((opt) => el("option", { value: opt.value, text: opt.label, disabled: Boolean(opt.disabled) })),
    );
    const wanted = options.find((opt) => opt.value === value && !opt.disabled);
    const firstEnabled = options.find((opt) => !opt.disabled);
    select.value = (wanted || firstEnabled || options[0] || { value: "" }).value;
  }

  /**
   * Build the panel DOM once per open.
   * @param {"video"|"list"} mode - Single video or playlist/channel.
   * @param {string} heading - Initial heading.
   * @returns {object} Element references.
   */
  function buildPanel(mode, heading) {
    const titleId = `yt-dl-panel-title-${++idCounter}`;
    const ui = {};

    ui.title = el("h2", { className: "yt-dl-panel-title", id: titleId, text: heading });
    ui.closeBtn = el("button", { className: "yt-dl-icon-btn", type: "button", "aria-label": "Close", text: "×" });
    ui.status = el("p", { className: "yt-dl-status", role: "status", "aria-live": "polite" });

    ui.videoTab = el("button", { className: "yt-dl-seg", type: "button", "aria-pressed": "true", text: "Video" });
    ui.audioTab = el("button", { className: "yt-dl-seg", type: "button", "aria-pressed": "false", text: "Audio" });
    const toggle = el("div", { className: "yt-dl-segmented", role: "group", "aria-label": "Download type" }, [ui.videoTab, ui.audioTab]);

    const quality = selectField(mode === "list" ? "Max quality" : "Quality", "quality");
    const codec = selectField("Codec", "codec");
    const container = selectField("Container", "container");
    ui.quality = quality.select;
    ui.codec = codec.select;
    ui.container = container.select;
    const maxSize = selectField("Max size", "max_size");
    const sizeErrorId = `yt-dl-size-error-${++idCounter}`;
    const sizeInputId = `yt-dl-size-${idCounter}`;
    ui.maxSize = maxSize.select;
    ui.sizeInput = el("input", {
      className: "yt-dl-input",
      id: sizeInputId,
      type: "number",
      inputmode: "numeric",
      min: String(SIZE_LIMITS.min),
      max: String(SIZE_LIMITS.max),
      step: "1",
      placeholder: `${SIZE_LIMITS.min}-${SIZE_LIMITS.max}`,
      autocomplete: "off",
      "aria-describedby": sizeErrorId,
    });
    ui.sizeCustom = el("div", { className: "yt-dl-field", hidden: true }, [
      el("label", { className: "yt-dl-label", for: sizeInputId, text: "Size (MB)" }),
      ui.sizeInput,
    ]);
    ui.sizeError = el("p", { className: "yt-dl-hint yt-dl-hint-error", id: sizeErrorId });
    ui.sizeHint = el("p", { className: "yt-dl-hint", hidden: true, text: "Converts to fit when no stream is small enough (slower, lower quality)" });
    ui.av1Hint = el("p", {
      className: "yt-dl-hint",
      hidden: true,
      text: "AV1 looks better at small sizes and converts slower. Some phones and older devices cannot play it.",
    });
    ui.videoSection = el("div", { className: "yt-dl-section" }, [
      quality.field,
      el("div", { className: "yt-dl-row" }, [codec.field, container.field]),
      el("div", { className: "yt-dl-row" }, [maxSize.field, ui.sizeCustom]),
      ui.sizeError,
      ui.sizeHint,
      ui.av1Hint,
    ]);

    const audioFormat = selectField("Format", "audio_format");
    const bitrate = selectField("Bitrate", "audio_bitrate");
    ui.audioFormat = audioFormat.select;
    ui.bitrate = bitrate.select;
    ui.bitrateHint = el("p", { className: "yt-dl-hint", hidden: true, text: "M4A keeps the original audio, so bitrate does not apply." });
    ui.audioSection = el("div", { className: "yt-dl-section", hidden: true }, [
      el("div", { className: "yt-dl-row" }, [audioFormat.field, bitrate.field]),
      ui.bitrateHint,
    ]);

    if (mode === "video") {
      const clipErrorId = `yt-dl-clip-error-${++idCounter}`;
      const timeField = (label, placeholder) => {
        const id = `yt-dl-clip-${label.toLowerCase()}-${++idCounter}`;
        const input = el("input", {
          className: "yt-dl-input",
          id,
          type: "text",
          inputmode: "numeric",
          placeholder,
          autocomplete: "off",
          spellcheck: "false",
          "aria-describedby": clipErrorId,
        });
        const now = el("button", {
          className: "yt-dl-btn yt-dl-btn-secondary yt-dl-btn-small",
          type: "button",
          text: "Now",
          title: `Use the current playback time as the ${label.toLowerCase()}`,
          "aria-label": `Set ${label.toLowerCase()} to the current playback time`,
        });
        const field = el("div", { className: "yt-dl-field" }, [
          el("label", { className: "yt-dl-label", for: id, text: label }),
          el("div", { className: "yt-dl-time" }, [input, now]),
        ]);
        return { field, input, now };
      };
      const start = timeField("Start", "0:00");
      const end = timeField("End", "end");
      ui.clipStart = start.input;
      ui.clipEnd = end.input;
      ui.clipStartNow = start.now;
      ui.clipEndNow = end.now;
      ui.clipError = el("p", { className: "yt-dl-hint yt-dl-hint-error", id: clipErrorId });
      ui.clipSection = el("fieldset", { className: "yt-dl-section yt-dl-fieldset" }, [
        el("legend", { className: "yt-dl-label" }, ["Clip ", el("span", { className: "yt-dl-muted", text: "(optional)" })]),
        el("div", { className: "yt-dl-row" }, [start.field, end.field]),
        ui.clipError,
      ]);
    }

    ui.ffmpegHint = el("p", {
      className: "yt-dl-hint yt-dl-hint-warn",
      hidden: true,
      text: "ffmpeg was not found, so qualities above 720p, max size, clips and MP3 are unavailable. The helper sets ffmpeg up by itself; check the toolbar popup for setup errors.",
    });

    if (mode === "list") {
      const group = `yt-dl-items-${++idCounter}`;
      const rangeId = `yt-dl-range-${idCounter}`;
      const errorId = `yt-dl-range-error-${idCounter}`;
      ui.allRadio = el("input", { type: "radio", name: group, value: "all", checked: true });
      ui.rangeRadio = el("input", { type: "radio", name: group, value: "range" });
      ui.rangeInput = el("input", {
        className: "yt-dl-input",
        id: rangeId,
        type: "text",
        inputmode: "numeric",
        placeholder: "1-10,15",
        autocomplete: "off",
        spellcheck: "false",
        "aria-label": "Item range",
        "aria-describedby": errorId,
        disabled: true,
      });
      ui.rangeError = el("p", { className: "yt-dl-hint yt-dl-hint-error", id: errorId });
      ui.entryCount = el("span", { className: "yt-dl-muted" });
      ui.listSection = el("fieldset", { className: "yt-dl-section yt-dl-fieldset" }, [
        el("legend", { className: "yt-dl-label" }, ["Items ", ui.entryCount]),
        el("label", { className: "yt-dl-radio" }, [ui.allRadio, "All"]),
        el("label", { className: "yt-dl-radio" }, [ui.rangeRadio, "Range"]),
        ui.rangeInput,
        ui.rangeError,
      ]);
    }

    ui.saveBtn = el("button", { className: "yt-dl-btn yt-dl-btn-secondary", type: "button", text: "Save as default" });
    ui.downloadBtn = el("button", { className: "yt-dl-btn yt-dl-btn-primary", type: "button", text: "Download" });

    ui.root = el(
      "div",
      { className: "yt-dl-panel", role: "dialog", "aria-modal": "false", "aria-labelledby": titleId, tabindex: "-1" },
      [
        el("div", { className: "yt-dl-panel-header" }, [ui.title, ui.closeBtn]),
        ui.status,
        toggle,
        ui.videoSection,
        ui.audioSection,
        ui.clipSection || null,
        ui.ffmpegHint,
        ui.listSection || null,
        el("div", { className: "yt-dl-panel-footer" }, [ui.saveBtn, ui.downloadBtn]),
      ],
    );
    return ui;
  }

  /**
   * Show a status line in the panel.
   * @param {string} text - Message; empty hides the line.
   * @param {"info"|"error"|"success"} [tone] - Visual tone.
   * @param {string} [detail] - Longer text (such as yt-dlp output) kept to a tooltip and the console.
   */
  function setStatus(text, tone = "info", detail = "") {
    if (!state) return;
    state.ui.status.textContent = text;
    state.ui.status.hidden = !text;
    state.ui.status.dataset.tone = tone;
    if (detail) {
      state.ui.status.title = detail;
      console.warn(`[yt-dl] ${text}
${detail}`);
    } else {
      state.ui.status.removeAttribute("title");
    }
  }

  /**
   * Whether an options object can run with the current ffmpeg availability.
   * @param {object} options - Download options.
   * @returns {boolean} False when it needs ffmpeg and ffmpeg is known to be missing.
   */
  function allowed(options) {
    return state.ffmpeg !== false || !NEEDS_FFMPEG(options);
  }

  /**
   * Quality options for the select: heights from /info when present, otherwise every preset.
   * @returns {Array<{value: string, label: string, disabled: boolean}>} Select options.
   */
  function qualityOptions() {
    const info = state.info;
    const presets = QUALITIES;
    let list;
    if (info && Array.isArray(info.qualities) && info.qualities.length) {
      const byHeight = new Map(info.qualities.map((q) => [String(q.height), q]));
      list = presets
        .filter((preset) => preset.value === "best" || byHeight.has(preset.value))
        .map((preset) => {
          const q = byHeight.get(preset.value);
          const size = q && q.filesize_est ? format.bytes(q.filesize_est) : "";
          return { value: preset.value, label: size ? `${preset.label} - ~${size}` : preset.label };
        });
    } else {
      list = presets.map((preset) => ({ value: preset.value, label: preset.label }));
    }
    return list.map((opt) => ({ ...opt, disabled: !allowed({ ...state.options, kind: "video", quality: opt.value }) }));
  }

  /**
   * Codec options, flagging codecs /info did not list for the chosen height.
   * @returns {Array<{value: string, label: string}>} Select options.
   */
  function codecOptions() {
    const info = state.info;
    let available = null;
    if (info && Array.isArray(info.qualities) && state.options.quality !== "best") {
      const q = info.qualities.find((item) => String(item.height) === state.options.quality);
      if (q && Array.isArray(q.codecs) && q.codecs.length) available = new Set(q.codecs.map(normaliseCodec));
    }
    return CODECS.map((codec) => {
      const missing = available && codec.value !== "any" && !available.has(codec.value);
      return { value: codec.value, label: missing ? `${codec.label} (not at this quality)` : codec.label };
    });
  }

  /**
   * Sync every control with state.options and the ffmpeg/info state.
   */
  function render() {
    const { ui, options } = state;
    const isVideo = options.kind === "video";

    ui.videoTab.setAttribute("aria-pressed", String(isVideo));
    ui.audioTab.setAttribute("aria-pressed", String(!isVideo));
    ui.videoSection.hidden = !isVideo;
    ui.audioSection.hidden = isVideo;

    // Size first: with ffmpeg missing it falls back to Off, which re-enables the qualities below
    const sizeBlocked = !allowed({ ...options, kind: "video", max_size_mb: SIZE_LIMITS.min });
    const sizes = MAX_SIZES.map((s) => ({ ...s, disabled: s.value !== "0" && sizeBlocked }));
    fillSelect(ui.maxSize, sizes, state.sizeChoice);
    state.sizeChoice = ui.maxSize.value;
    const custom = state.sizeChoice === MAX_SIZE_CUSTOM;
    options.max_size_mb = maxSizeValue(state.sizeChoice, ui.sizeInput.value) || 0;
    const sized = custom || options.max_size_mb > 0;
    ui.sizeCustom.hidden = !custom;
    ui.sizeHint.hidden = !sized;

    fillSelect(ui.quality, qualityOptions(), options.quality);
    options.quality = ui.quality.value;
    // With a max size the codec is the one the file is converted to (or picked in), so only
    // H.264 and AV1 make sense; the plain preference comes back when the size is turned off
    fillSelect(ui.codec, sized ? SIZED_CODECS : codecOptions(), sized && options.codec !== "av1" ? "any" : options.codec);
    if (!sized || ui.codec.value === "av1") options.codec = ui.codec.value;
    ui.av1Hint.hidden = !(sized && options.codec === "av1");
    // A max size always saves MP4: show that, but keep the chosen container for when it is turned off
    fillSelect(ui.container, CONTAINERS, sized ? "mp4" : options.container);
    if (!sized) options.container = ui.container.value;
    ui.container.disabled = sized;
    if (sized) ui.container.title = "Max size always saves MP4";
    else ui.container.removeAttribute("title");

    const formats = AUDIO_FORMATS.map((f) => ({ ...f, disabled: !allowed({ ...options, kind: "audio", audio_format: f.value }) }));
    fillSelect(ui.audioFormat, formats, options.audio_format);
    options.audio_format = ui.audioFormat.value;
    fillSelect(ui.bitrate, AUDIO_BITRATES, options.audio_bitrate);
    options.audio_bitrate = ui.bitrate.value;
    const lossless = options.audio_format !== "mp3";
    ui.bitrate.disabled = lossless;
    ui.bitrateHint.hidden = !lossless;

    ui.ffmpegHint.hidden = state.ffmpeg !== false;
    const sizeOk = sizeValid();
    const clipOk = clipValid();
    if (ui.clipSection) {
      const playing = Boolean(playerVideo());
      ui.clipStartNow.hidden = !playing;
      ui.clipEndNow.hidden = !playing;
    }
    ui.saveBtn.disabled = state.busy || !sizeOk;
    ui.downloadBtn.disabled = state.busy || !allowed(options) || !rangeValid() || !sizeOk || !clipOk;

    if (state.mode === "list") {
      const count = state.info && typeof state.info.entry_count === "number" ? state.info.entry_count : null;
      ui.entryCount.textContent = count === null ? "" : `(${count} ${count === 1 ? "video" : "videos"})`;
      ui.rangeInput.disabled = !ui.rangeRadio.checked;
    }
  }

  /**
   * Validate the range field and update its error text.
   * @returns {boolean} True when "All" is chosen or the range is valid.
   */
  function rangeValid() {
    const ui = state.ui;
    if (state.mode !== "list" || !ui.rangeRadio.checked) {
      if (ui.rangeInput) {
        ui.rangeInput.removeAttribute("aria-invalid");
        ui.rangeError.textContent = "";
      }
      return true;
    }
    const raw = ui.rangeInput.value.trim();
    let message = "";
    const normalised = raw ? format.parseRange(raw) : null;
    if (!raw) message = "Enter items, e.g. 1-10,15";
    else if (!normalised) message = "Use item numbers and ranges, e.g. 1-10,15";
    else {
      const count = state.info && state.info.entry_count;
      const highest = Math.max(...normalised.split(/[,-]/).map(Number));
      if (typeof count === "number" && highest > count) message = `This list only has ${count} items`;
    }
    // Hold back the empty-field message until the user has typed, to avoid an instant error
    const shown = !raw && !state.rangeTouched ? "" : message;
    ui.rangeError.textContent = shown;
    if (shown) ui.rangeInput.setAttribute("aria-invalid", "true");
    else ui.rangeInput.removeAttribute("aria-invalid");
    return !message;
  }

  /**
   * Validate the custom max size field and update its error text.
   * @returns {boolean} True unless a video download has Custom chosen with an invalid size.
   */
  function sizeValid() {
    const ui = state.ui;
    if (state.options.kind !== "video" || state.sizeChoice !== MAX_SIZE_CUSTOM) {
      ui.sizeInput.removeAttribute("aria-invalid");
      ui.sizeError.textContent = "";
      return true;
    }
    const valid = maxSizeValue(MAX_SIZE_CUSTOM, ui.sizeInput.value) !== null;
    // Same as the range field: no error for an empty field until the user has typed
    const shown = valid || (!ui.sizeInput.value && !state.sizeTouched) ? "" : `Enter a whole number of MB from ${SIZE_LIMITS.min} to ${SIZE_LIMITS.max}`;
    ui.sizeError.textContent = shown;
    if (shown) ui.sizeInput.setAttribute("aria-invalid", "true");
    else ui.sizeInput.removeAttribute("aria-invalid");
    return valid;
  }

  /**
   * Read the clip fields into state.options and update their error text. Empty fields mean
   * the start and the end of the video.
   * @returns {boolean} True when there is no clip field or both fields are usable.
   */
  function clipValid() {
    const { ui, options } = state;
    options.clip_start = 0;
    options.clip_end = 0;
    if (!ui.clipSection) return true;
    const rawStart = ui.clipStart.value.trim();
    const rawEnd = ui.clipEnd.value.trim();
    const start = rawStart ? format.parseTime(rawStart) : 0;
    const end = rawEnd ? format.parseTime(rawEnd) : 0;
    const duration = state.info && Number(state.info.duration) > 0 ? Number(state.info.duration) : 0;
    let message = "";
    let bad = null;
    if (start === null || end === null) {
      message = "Use m:ss or h:mm:ss, e.g. 1:30";
      bad = start === null ? ui.clipStart : ui.clipEnd;
    } else if (end && end <= start) {
      message = "End must be after start";
      bad = ui.clipEnd;
    } else if (duration && start >= duration) {
      message = `This video is only ${format.clock(duration)} long`;
      bad = ui.clipStart;
    }
    ui.clipError.textContent = message;
    for (const input of [ui.clipStart, ui.clipEnd]) {
      if (input === bad) input.setAttribute("aria-invalid", "true");
      else input.removeAttribute("aria-invalid");
    }
    if (message) return false;
    // An end at or past the video's end is the same as no end
    options.clip_start = start;
    options.clip_end = duration && end >= duration ? 0 : end;
    return true;
  }

  /**
   * Find the page's playing video for the "Now" buttons: the largest visible video element,
   * which is the main player on a watch page and the active Short on Shorts.
   * @returns {HTMLVideoElement|null} The player, or null when the page has none.
   */
  function playerVideo() {
    let best = null;
    let bestArea = 0;
    for (const video of document.querySelectorAll("video")) {
      const rect = video.getBoundingClientRect();
      const area = rect.width * rect.height;
      if (area > bestArea && Number.isFinite(video.duration)) {
        best = video;
        bestArea = area;
      }
    }
    return best;
  }

  /**
   * Fill a clip field with the player's current time.
   * @param {HTMLInputElement} input - Start or end field.
   */
  function useCurrentTime(input) {
    const video = playerVideo();
    if (!video) return;
    input.value = format.clock(video.currentTime);
    render();
  }

  /**
   * Fetch /info and backend health for the open panel, ignoring results from an older open.
   */
  async function loadInfo() {
    const token = state.token;
    state.info = null;
    setStatus(state.mode === "list" ? "Loading list details..." : "Loading formats...");
    render();

    const known = content.backend && content.backend.health;
    const [infoRes, healthRes] = await Promise.all([
      content.send({ type: "info", url: state.url }),
      known ? Promise.resolve({ ok: true, data: content.backend }) : content.send({ type: "health" }),
    ]);
    if (!state || state.token !== token) return;

    if (healthRes.ok && healthRes.data && healthRes.data.health) {
      state.ffmpeg = Boolean(healthRes.data.health.ffmpeg);
    }

    if (infoRes.ok) {
      state.info = infoRes.data;
      if (infoRes.data && infoRes.data.title) state.ui.title.textContent = infoRes.data.title;
      setStatus("");
    } else if (infoRes.code === HELPER_MISSING) {
      setStatus(content.helperMissingText(infoRes), "error");
    } else {
      const down = healthRes.ok && healthRes.data && healthRes.data.reachable === false;
      setStatus(down ? "Backend not running. Download will try to start it." : infoRes.error || "Could not load details", "error", infoRes.detail);
    }
    render();
  }

  /**
   * Collect the option fields in the shape the backend expects.
   * @returns {object} Download options.
   */
  function currentOptions() {
    const out = {};
    for (const key of OPTION_KEYS) out[key] = state.options[key];
    if (out.kind === "video" && out.max_size_mb > 0) out.container = "mp4";
    return out;
  }

  /**
   * The options for a download: {@link currentOptions} plus the clip, which is never saved
   * as a default because it only fits the one video.
   * @returns {object} Download options.
   */
  function jobOptions() {
    const out = currentOptions();
    if (state.options.clip_start > 0) out.clip_start = state.options.clip_start;
    if (state.options.clip_end > 0) out.clip_end = state.options.clip_end;
    return out;
  }

  /**
   * Queue the download, starting the backend first when it is known to be down. Every
   * trigger (button click or Enter in the range field) goes through the same busy and
   * ffmpeg checks, so one list cannot be queued twice.
   */
  async function download() {
    if (!state || state.busy || !allowed(state.options)) return;
    if (!sizeValid()) {
      state.sizeTouched = true;
      sizeValid();
      state.ui.sizeInput.focus();
      return;
    }
    if (!rangeValid()) {
      state.rangeTouched = true;
      rangeValid();
      state.ui.rangeInput.focus();
      return;
    }
    const token = state.token;
    state.busy = true;
    render();
    setStatus("Adding to queue...");

    if (content.backendDown()) {
      setStatus("Starting backend...");
      const started = await content.send({ type: "ensureBackend" });
      if (!state || state.token !== token) return;
      if (!started.ok || !started.data || !started.data.reachable) {
        state.busy = false;
        render();
        if (started.data && started.data.code === HELPER_MISSING) setStatus(content.helperMissingText(started.data), "error");
        else setStatus((started.data && started.data.error) || "Backend not running. Check the README setup steps.", "error");
        return;
      }
    }

    const message = { type: "createJobs", url: state.url, options: jobOptions() };
    if (state.mode === "list" && state.ui.rangeRadio.checked) {
      message.items = format.parseRange(state.ui.rangeInput.value.trim());
    }
    const res = await content.send(message);
    if (!state || state.token !== token) return;
    if (!res.ok) {
      state.busy = false;
      render();
      if (res.code === HELPER_MISSING) setStatus(content.helperMissingText(res), "error");
      else setStatus(res.error || "Download failed to start", "error", res.detail);
      return;
    }
    // Stay busy until the panel closes so a second click or Enter cannot queue it again
    const count = res.data && Array.isArray(res.data.jobs) ? res.data.jobs.length : 1;
    setStatus(count === 1 ? "Added to the queue" : `Added ${count} downloads to the queue`, "success");
    setTimeout(() => {
      if (state && state.token === token) close();
    }, CLOSE_AFTER_SUCCESS_MS);
  }

  /**
   * Save the panel's options as the default for one-click downloads.
   */
  async function saveDefault() {
    const ok = await content.saveDefaults(currentOptions());
    if (state) setStatus(ok ? "Saved as default" : "Could not save the default", ok ? "success" : "error");
  }

  /**
   * Place the panel next to its anchor, flipping above when there is no room below and
   * clamping to the viewport on every side.
   */
  function position() {
    if (!state) return;
    const { anchor, ui } = state;
    if (!anchor.isConnected) {
      close();
      return;
    }
    const panel = ui.root;
    const vw = document.documentElement.clientWidth;
    const vh = window.innerHeight;
    panel.style.maxHeight = `${vh - VIEWPORT_MARGIN * 2}px`;

    const a = anchor.getBoundingClientRect();
    const p = panel.getBoundingClientRect();

    let left = a.left;
    if (left + p.width > vw - VIEWPORT_MARGIN) left = a.right - p.width;
    left = Math.max(VIEWPORT_MARGIN, Math.min(left, vw - p.width - VIEWPORT_MARGIN));

    let top = a.bottom + ANCHOR_GAP;
    if (top + p.height > vh - VIEWPORT_MARGIN && a.top - ANCHOR_GAP - p.height >= VIEWPORT_MARGIN) {
      top = a.top - ANCHOR_GAP - p.height;
    }
    top = Math.max(VIEWPORT_MARGIN, Math.min(top, vh - p.height - VIEWPORT_MARGIN));

    panel.style.left = `${Math.round(left)}px`;
    panel.style.top = `${Math.round(top)}px`;
  }

  /**
   * Keep Tab focus inside the panel while it is open.
   * @param {KeyboardEvent} event - Keydown event.
   */
  function trapTab(event) {
    const focusable = [...state.ui.root.querySelectorAll("button, select, input")].filter(
      (node) => !node.disabled && content.isVisible(node),
    );
    if (!focusable.length) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  }

  /**
   * Close on Escape and trap Tab while focus is inside the panel.
   * @param {KeyboardEvent} event - Keydown event (capture phase on the document).
   */
  function onKeydown(event) {
    if (!state) return;
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      close({ restoreFocus: true });
    } else if (event.key === "Tab" && state.ui.root.contains(document.activeElement)) {
      trapTab(event);
    }
  }

  /**
   * Close when the user presses anywhere outside the panel and its anchor.
   * @param {PointerEvent} event - Pointerdown event (capture phase on the document).
   */
  function onPointerDown(event) {
    if (!state) return;
    const path = event.composedPath();
    if (path.includes(state.ui.root) || path.includes(state.anchor)) return;
    close({ restoreFocus: false });
  }

  /**
   * Stop keystrokes typed inside the panel from reaching YouTube's document-level
   * shortcuts (k, f, m, arrows). Escape and Tab still bubble for the panel's own handling.
   * @param {KeyboardEvent} event - Key event.
   */
  function isolateKeys(event) {
    if (event.key !== "Escape" && event.key !== "Tab") event.stopPropagation();
  }

  /**
   * Wire control events for a freshly built panel.
   */
  function bindEvents() {
    const { ui } = state;
    const setOption = (key, value) => {
      state.options[key] = value;
      render();
    };
    ui.closeBtn.addEventListener("click", () => close({ restoreFocus: true }));
    ui.videoTab.addEventListener("click", () => setOption("kind", "video"));
    ui.audioTab.addEventListener("click", () => setOption("kind", "audio"));
    ui.quality.addEventListener("change", () => setOption("quality", ui.quality.value));
    ui.codec.addEventListener("change", () => setOption("codec", ui.codec.value));
    ui.container.addEventListener("change", () => setOption("container", ui.container.value));
    ui.audioFormat.addEventListener("change", () => setOption("audio_format", ui.audioFormat.value));
    ui.bitrate.addEventListener("change", () => setOption("audio_bitrate", ui.bitrate.value));
    ui.maxSize.addEventListener("change", () => {
      state.sizeChoice = ui.maxSize.value;
      render();
      if (state.sizeChoice === MAX_SIZE_CUSTOM) ui.sizeInput.focus();
    });
    ui.sizeInput.addEventListener("input", () => {
      state.sizeTouched = true;
      render();
    });
    ui.sizeInput.addEventListener("keydown", (event) => {
      if (event.key === "Enter") download();
    });
    ui.downloadBtn.addEventListener("click", download);
    ui.saveBtn.addEventListener("click", saveDefault);
    ui.root.addEventListener("keydown", isolateKeys);
    ui.root.addEventListener("keyup", isolateKeys);
    ui.root.addEventListener("keypress", isolateKeys);

    if (ui.clipSection) {
      for (const input of [ui.clipStart, ui.clipEnd]) {
        input.addEventListener("input", render);
        input.addEventListener("keydown", (event) => {
          if (event.key === "Enter") download();
        });
      }
      ui.clipStartNow.addEventListener("click", () => useCurrentTime(ui.clipStart));
      ui.clipEndNow.addEventListener("click", () => useCurrentTime(ui.clipEnd));
    }

    if (state.mode === "list") {
      const onRadio = () => {
        render();
        if (ui.rangeRadio.checked) ui.rangeInput.focus();
      };
      ui.allRadio.addEventListener("change", onRadio);
      ui.rangeRadio.addEventListener("change", onRadio);
      ui.rangeInput.addEventListener("input", () => {
        state.rangeTouched = true;
        render();
      });
      ui.rangeInput.addEventListener("keydown", (event) => {
        if (event.key === "Enter") download();
      });
    }
  }

  /**
   * Close the panel if open. Focus returns to the anchor when asked, or by default when it
   * was inside the panel; a pointer press elsewhere passes restoreFocus: false so the click
   * keeps its target. The focus never scrolls, since the panel follows the page scroll and
   * the anchor may be far off screen.
   * @param {{restoreFocus?: boolean}} [opts] - Return focus to the anchor.
   */
  function close(opts = {}) {
    if (!state) return;
    const { ui, anchor, onClose } = state;
    const hadFocus = opts.restoreFocus !== false && ui.root.contains(document.activeElement);
    state = null;
    ui.root.remove();
    document.removeEventListener("keydown", onKeydown, true);
    document.removeEventListener("pointerdown", onPointerDown, true);
    window.removeEventListener("resize", position);
    window.removeEventListener("scroll", position, true);
    anchor.setAttribute("aria-expanded", "false");
    if ((opts.restoreFocus || hadFocus) && anchor.isConnected) anchor.focus({ preventScroll: true });
    if (onClose) onClose();
  }

  /**
   * Open the panel next to a button. Opening for the anchor that is already open closes it.
   * @param {object} opts - Panel options.
   * @param {HTMLElement} opts.anchor - Button that toggles the panel; gets aria-expanded.
   * @param {string} opts.url - Canonical URL to download.
   * @param {"video"|"list"} [opts.mode] - Single video or playlist/channel list.
   * @param {string} [opts.heading] - Heading shown until /info supplies the title.
   * @param {() => void} [opts.onClose] - Called after the panel closes.
   */
  function open(opts) {
    if (state && state.anchor === opts.anchor) {
      close({ restoreFocus: true });
      return;
    }
    close();

    const mode = opts.mode === "list" ? "list" : "video";
    const ui = buildPanel(mode, opts.heading || (mode === "list" ? "Download list" : "Download video"));
    state = {
      token: ++idCounter,
      ui,
      mode,
      url: opts.url,
      anchor: opts.anchor,
      onClose: opts.onClose,
      options: { ...content.settings.defaults },
      info: null,
      ffmpeg: null,
      busy: false,
      rangeTouched: false,
      sizeChoice: maxSizeChoice(content.settings.defaults.max_size_mb),
      sizeTouched: false,
    };
    if (state.sizeChoice === MAX_SIZE_CUSTOM) ui.sizeInput.value = String(content.settings.defaults.max_size_mb);

    bindEvents();
    setStatus("");
    render();
    document.body.append(ui.root);
    opts.anchor.setAttribute("aria-expanded", "true");
    position();

    document.addEventListener("keydown", onKeydown, true);
    document.addEventListener("pointerdown", onPointerDown, true);
    window.addEventListener("resize", position);
    window.addEventListener("scroll", position, true);

    (state.options.kind === "audio" ? ui.audioTab : ui.videoTab).focus();
    loadInfo().then(position);
  }

  content.on("navigate", () => close());

  content.panel = {
    open,
    close,
    /**
     * Whether the panel is currently open for a given anchor.
     * @param {HTMLElement} anchor - Button to test.
     * @returns {boolean} True when that anchor owns the open panel.
     */
    isOpenFor(anchor) {
      return Boolean(state && state.anchor === anchor);
    },
  };
})();
