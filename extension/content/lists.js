// "Download playlist" / "Download channel" header button. Loaded last, so it also starts the runtime.
var YTDL = globalThis.YTDL || (globalThis.YTDL = {});

(function () {
  "use strict";

  const content = YTDL.content;
  if (!content || !content.ready) return;

  // Scope to the visible browse page: YouTube keeps earlier pages in the DOM, hidden
  const PAGE = "ytd-browse:not([hidden])";

  // Action rows first (button sits with Play all / Subscribe), then whole headers as a last resort
  const PLAYLIST_TARGETS = [
    `${PAGE} yt-page-header-renderer yt-flexible-actions-view-model`,
    `${PAGE} ytd-playlist-header-renderer .metadata-buttons-wrapper`,
    `${PAGE} ytd-playlist-header-renderer #top-level-buttons-computed`,
    `${PAGE} ytd-playlist-header-renderer .metadata-action-bar`,
    `${PAGE} yt-page-header-renderer`,
    `${PAGE} ytd-playlist-header-renderer`,
  ];
  const CHANNEL_TARGETS = [
    `${PAGE} #page-header yt-flexible-actions-view-model`,
    `${PAGE} yt-page-header-renderer yt-flexible-actions-view-model`,
    `${PAGE} ytd-c4-tabbed-header-renderer #buttons`,
    `${PAGE} #channel-header #buttons`,
    `${PAGE} #page-header`,
    `${PAGE} yt-page-header-renderer`,
    `${PAGE} ytd-c4-tabbed-header-renderer`,
  ];

  let ctrl = null;

  /**
   * Build the header button for a list page.
   * @param {"playlist"|"channel"} type - Page type.
   * @returns {{root: HTMLElement, type: string, url: string|null}} Controller.
   */
  function createListButton(type) {
    const label = type === "playlist" ? "Download playlist" : "Download channel";
    const button = content.el(
      "button",
      {
        className: "yt-dl-list-btn",
        type: "button",
        "aria-haspopup": "dialog",
        "aria-expanded": "false",
        title: `${label} with yt-dl`,
        "data-yt-dl-owner": "list",
      },
      [content.downloadIcon(), content.el("span", { className: "yt-dl-label-text", text: label })],
    );
    const result = { root: button, type, url: null };
    button.addEventListener("click", () => {
      if (!result.url) return;
      content.panel.open({ anchor: button, url: result.url, mode: "list", heading: label });
    });
    return result;
  }

  /**
   * Insert, move or remove the header button to match the current page.
   */
  function sync() {
    const page = content.page;
    if (page.type !== "playlist" && page.type !== "channel") {
      teardown();
      return;
    }
    if (ctrl && ctrl.type !== page.type) teardown();
    if (!ctrl) ctrl = createListButton(page.type);
    if (ctrl.url !== page.url && content.panel.isOpenFor(ctrl.root)) content.panel.close();
    ctrl.url = page.url;

    for (const stray of document.querySelectorAll('[data-yt-dl-owner="list"]')) {
      if (stray !== ctrl.root) stray.remove();
    }

    const target = content.findFirst(page.type === "playlist" ? PLAYLIST_TARGETS : CHANNEL_TARGETS);
    if (!target || !content.isVisible(target) || ctrl.root.parentElement === target) return;
    target.append(ctrl.root);
  }

  /**
   * Remove the header button and close its panel.
   */
  function teardown() {
    if (!ctrl) return;
    if (content.panel.isOpenFor(ctrl.root)) content.panel.close();
    ctrl.root.remove();
    ctrl = null;
  }

  content.on("navigate", sync);
  content.on("dom", () => {
    const type = content.page.type;
    if (type === "playlist" || type === "channel") sync();
  });

  content.start();
})();
