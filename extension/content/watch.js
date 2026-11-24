// Watch-page split button, plus the video button controller that shorts.js reuses.
var YTDL = globalThis.YTDL || (globalThis.YTDL = {});

(function () {
  "use strict";

  const content = YTDL.content;
  if (!content || !content.ready) return;

  const el = content.el;
  const DONE_MS = 3000;
  const FLASH_MS = 4000;
  // How long "Starting..." waits for the new job to show up in the job list before giving up
  const START_WAIT_MS = 5000;
  // Longer one-off messages show as "Error" with the text in the tooltip, so the button
  // never cuts a sentence off ("Helper not installed" still fits)
  const FLASH_LABEL_MAX = 24;

  // Every label the download flow shows on the full-size button (busy labels, then job
  // states); the button keeps the widest one's width throughout. Keep them about as short as
  // "Download", or the idle button widens too.
  const FIXED_LABELS = ["Download", "Starting...", "Stopping", "Retrying", "Resuming", "Queued", "Preparing", "100%", "Finishing", "Paused", "Done", "Retry", "Error"];
  const ACTIVE = new Set(YTDL.constants.ACTIVE_STATUSES);

  // Fallbacks tried after the configurable selector, newest layout first
  const WATCH_FALLBACKS = [
    "ytd-watch-metadata #actions #top-level-buttons-computed",
    "#top-level-buttons-computed",
    "#actions-inner #menu",
    "ytd-watch-metadata #actions",
  ];

  /**
   * Create a download button bound to one video at a time. It shows live job state from
   * the "jobs" event and opens the shared panel from its arrow.
   * @param {object} opts - Button options.
   * @param {"watch"|"shorts"} opts.owner - Page the button belongs to (used to find duplicates).
   * @param {boolean} [opts.compact] - Shorts layout: round icon button with a caption underneath.
   * @returns {{root: HTMLElement, setVideo: (videoId: string, url: string) => void, destroy: () => void}} Controller.
   */
  content.createVideoButton = function (opts) {
    const compact = Boolean(opts.compact);
    const labelNode = el("span", { className: "yt-dl-label-text", text: "Download" });
    const fill = el("span", { className: "yt-dl-fill", "aria-hidden": "true" });
    const main = el("button", { className: "yt-dl-main", type: "button" }, [fill, content.downloadIcon(), compact ? null : labelNode]);
    const arrow = el("button", {
      className: "yt-dl-arrow",
      type: "button",
      "aria-label": "Download options",
      title: "Download options",
      "aria-haspopup": "dialog",
      "aria-expanded": "false",
    });
    arrow.append(el("span", { className: "yt-dl-chevron", "aria-hidden": "true" }), el("span", { className: "yt-dl-cross", "aria-hidden": "true" }));

    const root = el(
      "div",
      {
        className: compact ? "yt-dl-split yt-dl-compact" : "yt-dl-split",
        role: "group",
        "aria-label": "Download with yt-dl",
        "data-yt-dl-owner": opts.owner,
      },
      [main, arrow, compact ? el("span", { className: "yt-dl-caption", "aria-hidden": "true" }, [labelNode]) : null],
    );

    let videoId = null;
    let url = null;
    let busyText = null;
    let flash = null;
    let flashTimer = null;
    let doneTimer = null;
    let doneUntil = 0;
    let lastJobId = null;
    let lastStatus = null;
    // Fixed label width, the widest of FIXED_LABELS in the label's font; 0 until measured
    let fixedWidth = 0;
    let fixedFont = "";
    let measureCanvas = null;
    let fixHold = false;
    // Fires when the label is first laid out, however long YouTube takes to attach the row,
    // and when its size changes. Setting the same min-width again does not resize it, so this
    // settles after one round.
    const labelObserver = compact ? null : new ResizeObserver(() => fixWidth(fixHold));
    if (labelObserver) labelObserver.observe(labelNode);
    // The label's font (Roboto) can finish loading after the first measure; measure again
    const remeasure = () => {
      fixedFont = "";
      fixWidth(fixHold);
    };
    document.fonts.ready.then(remeasure);
    document.fonts.addEventListener("loadingdone", remeasure);
    let startingUntil = 0;
    let startingTimer = null;

    /**
     * Show a temporary message (e.g. a failed request) on the button.
     * @param {string} text - Message.
     */
    function showFlash(text) {
      flash = text;
      clearTimeout(flashTimer);
      flashTimer = setTimeout(() => {
        flash = null;
        render();
      }, FLASH_MS);
      render();
    }

    /**
     * Track job transitions so "Done" shows only right after a download finishes on this page,
     * not for a file finished earlier.
     * @param {object|null} job - Latest job for the video.
     */
    function trackDone(job) {
      if (!job) return;
      if (job.id === lastJobId && lastStatus && lastStatus !== "complete" && job.status === "complete") {
        doneUntil = Date.now() + DONE_MS;
        clearTimeout(doneTimer);
        doneTimer = setTimeout(render, DONE_MS + 50);
      }
      lastJobId = job.id;
      lastStatus = job.status;
    }

    /**
     * Work out the label, tone, progress and click action from the current state.
     * @returns {{text: string, tone: string, progress: number|null, action: string, title: string, free?: boolean}} View model; free labels are outside the download flow and may widen the button.
     */
    function view() {
      const job = videoId ? content.latestJobFor(videoId) : null;
      // "Starting backend..." comes before the download flow and is too long for its width
      if (busyText) return { text: busyText, tone: "busy", progress: null, action: "none", title: busyText, free: !FIXED_LABELS.includes(busyText) };
      // The job was created but the next job poll hasn't delivered it yet: keep the busy label
      // instead of flashing back to "Download" for a moment
      if (Date.now() < startingUntil) {
        if (job && ACTIVE.has(job.status)) startingUntil = 0;
        else return { text: "Starting...", tone: "busy", progress: null, action: "none", title: "Starting..." };
      }
      if (flash) {
        const text = flash.length > FLASH_LABEL_MAX ? "Error" : flash;
        return { text, tone: "error", progress: null, action: "download", title: flash };
      }
      if (content.backendDown() && !(job && ACTIVE.has(job.status))) {
        if (content.helperMissing()) {
          const title = `${content.helperMissingText(content.backend)} Click to open the download page.`;
          return { text: "Helper not installed", tone: "offline", progress: null, action: "get-helper", title, free: true };
        }
        const title = (content.backend && content.backend.error) || "Click to start the backend and download";
        return { text: "Backend not running", tone: "offline", progress: null, action: "download", title };
      }
      if (content.backendStarting() && !(job && ACTIVE.has(job.status))) {
        return { text: "Starting backend...", tone: "busy", progress: null, action: "download", title: "Click to download once the backend is up", free: true };
      }
      const setup = content.setupStatus();
      if (setup && !(job && ACTIVE.has(job.status))) {
        const text = setup.state === "error" ? "Setup failed" : "Setting up...";
        return { text, tone: setup.state === "error" ? "error" : "busy", progress: null, action: "download", title: setup.text, free: true };
      }
      if (job) {
        const name = job.title || "this video";
        switch (job.status) {
          case "queued":
            return { text: "Queued", tone: "active", progress: 0, action: "none", title: `${name} is queued` };
          case "downloading": {
            if (YTDL.format.isPreparing(job)) return { text: "Preparing", tone: "active", progress: 0, action: "none", title: `Getting ${name} ready to download` };
            const pct = Math.max(0, Math.min(100, Number(job.percent) || 0));
            return { text: YTDL.format.percent(pct), tone: "active", progress: pct, action: "none", title: `Downloading ${name}` };
          }
          case "processing":
            if (YTDL.format.isConverting(job)) {
              const pct = Math.max(0, Math.min(100, Number(job.percent) || 0));
              // Percentage only: "Converting 45%" would widen the button; the tooltip and
              // popup say it is converting
              return { text: YTDL.format.percent(pct), tone: "active", progress: pct, action: "none", title: `Converting ${name} to fit the max size` };
            }
            return { text: "Finishing", tone: "active", progress: 100, action: "none", title: `Finishing ${name} (merging and tidying up)` };
          case "paused":
            return { text: "Paused", tone: "paused", progress: Number(job.percent) || 0, action: "resume", title: "Click to resume" };
          // yt-dlp errors run long ("unable to download video data: HTTP Error 403: ..."), so
          // the button says what to do and the tooltip and popup carry the message
          case "error":
            return { text: "Retry", tone: "error", progress: null, action: "retry", title: `${job.error || "Download failed"} - click to retry` };
          case "complete":
            if (Date.now() < doneUntil) return { text: "Done", tone: "done", progress: null, action: "download", title: "Downloaded - click to download again" };
            break;
          // Looks idle, but restarts the cancelled job rather than queuing a second one, so one
          // video keeps one row in the popup. The arrow still starts fresh.
          // A cancelled playlist item is left to its card, so it never lands in that folder.
          case "cancelled":
            if (job.group_id) break;
            return { text: "Download", tone: "idle", progress: null, action: "retry", title: "Download (restarts the cancelled download with its options)" };
          default:
            break;
        }
      }
      return { text: "Download", tone: "idle", progress: null, action: "download", title: "Download with your default options" };
    }

    let currentAction = "download";
    let cancelJobId = null;

    /**
     * Switch the arrow between opening the panel and cancelling the active job.
     * @param {string|null} jobId - Active job to cancel, or null for the options arrow.
     */
    function setArrowMode(jobId) {
      if (jobId === cancelJobId) return;
      cancelJobId = jobId;
      root.dataset.cancel = String(Boolean(jobId));
      if (jobId) {
        if (content.panel.isOpenFor(arrow)) content.panel.close();
        arrow.removeAttribute("aria-haspopup");
        arrow.removeAttribute("aria-expanded");
        arrow.setAttribute("aria-label", "Cancel download");
        arrow.title = "Cancel download";
      } else {
        arrow.setAttribute("aria-haspopup", "dialog");
        arrow.setAttribute("aria-expanded", "false");
        arrow.setAttribute("aria-label", "Download options");
        arrow.title = "Download options";
      }
    }

    /**
     * Keep the label one width through a whole download, so the button never pushes YouTube's
     * right-aligned row about. The width fits the widest of FIXED_LABELS, measured once per
     * font with a canvas (the label's tabular digits only matter for "100%", far narrower).
     * Every label of the download flow is at least that wide; in a held state it is also at
     * most that wide, so a longer flash message ends in an ellipsis, its full text in the
     * tooltip. Offline and setup labels ("Backend not running") are left free to grow.
     * @param {boolean} hold - Whether the label is part of the download flow.
     */
    function fixWidth(hold) {
      if (compact) return;
      fixHold = hold;
      // The first render runs before the button is on the page, with no font to measure in;
      // labelObserver calls this again once it is laid out
      if (!labelNode.isConnected) return;
      const style = getComputedStyle(labelNode);
      const font = `${style.fontWeight} ${style.fontSize} ${style.fontFamily}`;
      if (font !== fixedFont) {
        measureCanvas = measureCanvas || document.createElement("canvas");
        const ctx = measureCanvas.getContext("2d");
        ctx.font = font;
        fixedWidth = Math.ceil(Math.max(...FIXED_LABELS.map((text) => ctx.measureText(text).width)));
        fixedFont = font;
      }
      labelNode.style.minWidth = `${fixedWidth}px`;
      if (hold) labelNode.style.maxWidth = `${fixedWidth}px`;
      else labelNode.style.removeProperty("max-width");
    }

    /**
     * Apply the current view model to the button.
     */
    function render() {
      const v = view();
      currentAction = v.action;
      const job = videoId ? content.latestJobFor(videoId) : null;
      const active = Boolean(job && ACTIVE.has(job.status));
      labelNode.textContent = v.text;
      root.dataset.tone = v.tone;
      main.title = v.title;
      main.setAttribute("aria-label", compact ? `${v.text}. ${v.title}` : v.title);
      main.setAttribute("aria-disabled", String(v.action === "none"));
      if (v.progress === null) root.style.removeProperty("--yt-dl-progress");
      else root.style.setProperty("--yt-dl-progress", `${v.progress}%`);
      setArrowMode(active ? job.id : null);
      fixWidth(v.tone !== "offline" && !v.free);
    }

    /**
     * Run a request with a busy label, surfacing failures as a flash message.
     * @param {string} text - Busy label.
     * @param {() => Promise<{ok: boolean, error?: string}>} fn - Request to run.
     * @returns {Promise<boolean>} Whether it succeeded.
     */
    async function withBusy(text, fn) {
      busyText = text;
      render();
      let res;
      try {
        res = await fn();
      } finally {
        busyText = null;
      }
      if (!res.ok) showFlash(res.error || "Something went wrong");
      else render();
      return res.ok;
    }

    /**
     * Main click: retry an error, resume a paused job, or queue a download with the saved
     * defaults (starting the backend first when it is known to be down).
     */
    async function onMainClick() {
      if (!videoId || busyText) return;
      if (currentAction === "get-helper") {
        if (YTDL.constants.HELPER_URL) window.open(YTDL.constants.HELPER_URL, "_blank", "noopener");
        return;
      }
      const job = content.latestJobFor(videoId);
      if (currentAction === "retry" && job) {
        await withBusy("Retrying", () => content.send({ type: "jobAction", id: job.id, action: "retry" }));
        return;
      }
      if (currentAction === "resume" && job) {
        await withBusy("Resuming", () => content.send({ type: "jobAction", id: job.id, action: "resume" }));
        return;
      }
      if (currentAction !== "download") return;

      if (content.backendDown()) {
        const started = await withBusy("Starting backend...", async () => {
          const res = await content.send({ type: "ensureBackend" });
          if (res.ok && !(res.data && res.data.reachable)) {
            if (res.data && res.data.code === YTDL.constants.HELPER_MISSING) return { ok: false, error: "Helper not installed" };
            return { ok: false, error: (res.data && res.data.error) || "Backend not running" };
          }
          if (res.ok) content.backend = { reachable: true, health: res.data.health };
          return res;
        });
        if (!started) return;
      }
      const targetUrl = url;
      const ok = await withBusy("Starting...", async () => {
        const res = await content.send({ type: "createJobs", url: targetUrl, options: { ...content.settings.defaults } });
        if (res.ok) {
          startingUntil = Date.now() + START_WAIT_MS;
          clearTimeout(startingTimer);
          startingTimer = setTimeout(render, START_WAIT_MS + 50);
        }
        return res;
      });
      if (!ok) startingUntil = 0;
    }

    main.addEventListener("click", onMainClick);
    arrow.addEventListener("click", async () => {
      if (cancelJobId) {
        if (busyText) return;
        const id = cancelJobId;
        await withBusy("Stopping", () => content.send({ type: "jobAction", id, action: "cancel" }));
        return;
      }
      if (!url) return;
      content.panel.open({ anchor: arrow, url, mode: "video" });
    });

    const offJobs = content.on("jobs", () => {
      if (videoId) trackDone(content.latestJobFor(videoId));
      render();
    });

    render();

    return {
      root,
      /**
       * Point the button at a different video, resetting transient state.
       * @param {string} id - Video id.
       * @param {string} canonicalUrl - URL sent to the backend.
       */
      setVideo(id, canonicalUrl) {
        url = canonicalUrl;
        if (id === videoId) return;
        videoId = id;
        flash = null;
        doneUntil = 0;
        startingUntil = 0;
        const job = content.latestJobFor(id);
        lastJobId = job ? job.id : null;
        lastStatus = job ? job.status : null;
        if (content.panel.isOpenFor(arrow)) content.panel.close();
        render();
      },
      /**
       * Remove the button and stop listening for job updates.
       */
      destroy() {
        offJobs();
        document.fonts.removeEventListener("loadingdone", remeasure);
        if (labelObserver) labelObserver.disconnect();
        clearTimeout(flashTimer);
        clearTimeout(doneTimer);
        clearTimeout(startingTimer);
        if (content.panel.isOpenFor(arrow)) content.panel.close();
        root.remove();
      },
    };
  };

  // Watch page controller

  let button = null;

  /**
   * Find where the button goes: the configured selector first, then the fallbacks. The video's
   * own action row is searched before the whole page, because info panels above the title
   * (the Wikipedia or fact-check boxes) carry a #top-level-buttons-computed of their own that
   * comes first in the document.
   * @returns {Element|null} Container element.
   */
  function findContainer() {
    const selectors = [content.settings.selectors.watchContainer, ...WATCH_FALLBACKS];
    const actions = content.findFirst(["ytd-watch-metadata #actions", "ytd-watch-metadata"]);
    return (actions && content.findFirst(selectors, actions)) || content.findFirst(selectors);
  }

  /**
   * Insert or move the button so exactly one copy sits in the current container.
   */
  function sync() {
    const page = content.page;
    if (page.type !== "watch") {
      teardown();
      return;
    }
    if (!button) button = content.createVideoButton({ owner: "watch" });
    button.setVideo(page.videoId, page.url);

    for (const stray of document.querySelectorAll('.yt-dl-split[data-yt-dl-owner="watch"]')) {
      if (stray !== button.root) stray.remove();
    }

    const container = findContainer();
    if (!container) return;
    // Only check the parent, not the exact position: YouTube adds its own buttons later and
    // re-ordering on every render would fight it in a mutation loop
    if (button.root.parentElement === container) return;
    const last = content.settings.selectors.watchInsertion === "last";
    // content.css puts the 8px gap on the side facing YouTube's buttons
    button.root.dataset.ytDlPlacement = last ? "last" : "first";
    if (last) container.append(button.root);
    else container.prepend(button.root);
  }

  /**
   * Remove the watch button when leaving the watch page.
   */
  function teardown() {
    if (!button) return;
    button.destroy();
    button = null;
  }

  content.on("navigate", sync);
  content.on("dom", () => {
    if (content.page.type === "watch") sync();
  });
  // Selector settings changed: rebuild in the new place
  content.on("settings", () => {
    if (!button) return;
    button.root.remove();
    sync();
  });
})();
