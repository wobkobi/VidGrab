// Compact download button in the action bar of the Short currently on screen.
var YTDL = globalThis.YTDL || (globalThis.YTDL = {});

(function () {
  "use strict";

  const content = YTDL.content;
  if (!content || !content.ready) return;

  // YouTube keeps the neighbouring reels in the DOM; only the on-screen one is marked active
  const ACTIVE_REEL_SELECTORS = [
    "ytd-reel-video-renderer[is-active]",
    ".reel-video-in-sequence-new[is-active]",
    ".reel-video-in-sequence[is-active]",
  ];
  const REEL_SELECTORS = "ytd-reel-video-renderer, .reel-video-in-sequence-new, .reel-video-in-sequence";
  const BAR_FALLBACKS = ["reel-action-bar-view-model", "#actions", "#button-bar"];

  let button = null;

  /**
   * Find the active reel, falling back to whichever reel covers the centre of the viewport.
   * @returns {Element|null} Reel element.
   */
  function activeReel() {
    const marked = content.findFirst(ACTIVE_REEL_SELECTORS);
    if (marked) return marked;
    const cx = window.innerWidth / 2;
    const cy = window.innerHeight / 2;
    for (const reel of document.querySelectorAll(REEL_SELECTORS)) {
      const r = reel.getBoundingClientRect();
      if (r.top <= cy && r.bottom >= cy && r.left <= cx && r.right >= cx) return reel;
    }
    return null;
  }

  /**
   * Find the action bar for the active Short.
   * @returns {Element|null} Bar element.
   */
  function findBar() {
    const selectors = [content.settings.selectors.shortsBar, ...BAR_FALLBACKS];
    const reel = activeReel();
    if (reel) {
      const inReel = content.findFirst(selectors, reel);
      if (inReel) return inReel;
    }
    // Some layouts render one shared action bar outside the reels
    const shared = content.findFirst(selectors.map((s) => s && `ytd-shorts ${s}`).filter(Boolean));
    return content.isVisible(shared) ? shared : null;
  }

  /**
   * Keep one button in the active Short's action bar, moving it as the user scrolls.
   */
  function sync() {
    const page = content.page;
    if (page.type !== "shorts") {
      teardown();
      return;
    }
    if (!button) button = content.createVideoButton({ owner: "shorts", compact: true });
    button.setVideo(page.videoId, page.url);

    for (const stray of document.querySelectorAll('.yt-dl-split[data-yt-dl-owner="shorts"]')) {
      if (stray !== button.root) stray.remove();
    }

    const bar = findBar();
    if (!bar || button.root.parentElement === bar) return;
    bar.prepend(button.root);
  }

  /**
   * Remove the Shorts button when leaving Shorts.
   */
  function teardown() {
    if (!button) return;
    button.destroy();
    button = null;
  }

  // The URL can change before YouTube moves [is-active] to the next reel, and attribute
  // changes do not reach the "dom" feed, so check again once the scroll has settled
  const SETTLE_MS = 400;
  content.on("navigate", () => {
    sync();
    setTimeout(sync, SETTLE_MS);
  });
  content.on("dom", () => {
    if (content.page.type === "shorts") sync();
  });
  content.on("settings", () => {
    if (!button) return;
    button.root.remove();
    sync();
  });
})();
