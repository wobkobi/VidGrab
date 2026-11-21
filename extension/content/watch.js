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
    // Label min-width held for the current job so it only grows ("Queued" > "45%" > "Converting 45%")
    let heldWidth = 0;
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
     * @returns {{text: string, tone: string, progress: number|null, action: string, title: string}} View model.
     */
    function view() {
      const job = videoId ? content.latestJobFor(videoId) : null;
      if (busyText) return { text: busyText, tone: "busy", progress: null, action: "none", title: busyText };
      // The job was created but the next job poll hasn't delivered it yet: keep the busy label
      // instead of flashing back to "Download" for a moment
      if (Date.now() < startingUntil) {
        if (job && ACTIVE.has(job.status)) startingUntil = 0;
        else return { text: "Starting...", tone: "busy", progress: null, action: "none", title: "Starting..." };
      }
      if (flash) return { text: flash, tone: "error", progress: null, action: "download", title: flash };
      if (content.backendDown() && !(job && ACTIVE.has(job.status))) {
        if (content.helperMissing()) {
          const title = `${content.helperMissingText(content.backend)} Click to open the download page.`;
          return { text: "Helper not installed", tone: "offline", progress: null, action: "get-helper", title };
        }
        const title = (content.backend && content.backend.error) || "Click to start the backend and download";
        return { text: "Backend not running", tone: "offline", progress: null, action: "download", title };
      }
      if (content.backendStarting() && !(job && ACTIVE.has(job.status))) {
        return { text: "Starting backend...", tone: "busy", progress: null, action: "download", title: "Click to download once the backend is up" };
      }
      const setup = content.setupStatus();
      if (setup && !(job && ACTIVE.has(job.status))) {
        const text = setup.state === "error" ? "Setup failed" : "Setting up...";
        return { text, tone: setup.state === "error" ? "error" : "busy", progress: null, action: "download", title: setup.text };
      }
      if (job) {
        const name = job.title || "this video";
        switch (job.status) {
          case "queued":
            return { text: "Queued", tone: "active", progress: 0, action: "none", title: `${name} is queued` };
          case "downloading": {
            const pct = Math.max(0, Math.min(100, Number(job.percent) || 0));
            return { text: YTDL.format.percent(pct), tone: "active", progress: pct, action: "none", title: `Downloading ${name}` };
          }
          case "processing":
            if (YTDL.format.isConverting(job)) {
              const pct = Math.max(0, Math.min(100, Number(job.percent) || 0));
              return { text: YTDL.format.convertingLabel(job), tone: "active", progress: pct, action: "none", title: `Converting ${name} to fit the max size` };
            }
            return { text: "Processing", tone: "active", progress: 100, action: "none", title: `Processing ${name}` };
          case "paused":
            return { text: "Paused", tone: "paused", progress: Number(job.percent) || 0, action: "resume", title: "Click to resume" };
          case "error":
            return { text: job.error || "Download failed", tone: "error", progress: null, action: "retry", title: `${job.error || "Download failed"} - click to retry` };
          case "complete":
            if (Date.now() < doneUntil) return { text: "Done", tone: "done", progress: null, action: "download", title: "Downloaded - click to download again" };
            break;
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
     * Hold the label at its widest during a job so the button does not resize on every
     * progress tick; release it once the job is over.
     * @param {boolean} hold - Whether a job is in progress.
     */
    function holdWidth(hold) {
      if (compact) return;
      if (!hold) {
        heldWidth = 0;
        labelNode.style.removeProperty("min-width");
        return;
      }
      heldWidth = Math.max(heldWidth, Math.ceil(labelNode.getBoundingClientRect().width));
      labelNode.style.minWidth = `${heldWidth}px`;
    }

    /**
     * Apply the current view model to the button.
     */
    function render() {
      const v = view();
      currentAction = v.action;
      labelNode.textContent = v.text;
      root.dataset.tone = v.tone;
      main.title = v.title;
      main.setAttribute("aria-label", compact ? `${v.text}. ${v.title}` : v.title);
      main.setAttribute("aria-disabled", String(v.action === "none"));
      if (v.progress === null) root.style.removeProperty("--yt-dl-progress");
      else root.style.setProperty("--yt-dl-progress", `${v.progress}%`);
      const job = videoId ? content.latestJobFor(videoId) : null;
      const active = Boolean(job && ACTIVE.has(job.status));
      setArrowMode(active ? job.id : null);
      holdWidth(active || v.tone === "busy");
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
        await withBusy("Retrying...", () => content.send({ type: "jobAction", id: job.id, action: "retry" }));
        return;
      }
      if (currentAction === "resume" && job) {
        await withBusy("Resuming...", () => content.send({ type: "jobAction", id: job.id, action: "resume" }));
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
        await withBusy("Cancelling...", () => content.send({ type: "jobAction", id, action: "cancel" }));
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
   * Find where the button goes: the configured selector first, then the fallbacks.
   * @returns {Element|null} Container element.
   */
  function findContainer() {
    const configured = content.settings.selectors.watchContainer;
    return content.findFirst([configured, ...WATCH_FALLBACKS]);
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
