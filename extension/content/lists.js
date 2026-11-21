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
   * Build the header button for a list page. The button sits in a wrapper so it can take the
   * same slot class as YouTube's own actions (Play all, Subscribe) and line up with them.
   * @param {"playlist"|"channel"} type - Page type.
   * @returns {{root: HTMLElement, button: HTMLElement, type: string, url: string|null, fitted: boolean}} Controller.
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
      },
      [content.downloadIcon(), content.el("span", { className: "yt-dl-label-text", text: label })],
    );
    const root = content.el("div", { className: "yt-dl-list-box", "data-yt-dl-owner": "list" }, [button]);
    const result = { root, button, type, url: null, fitted: false };
    button.addEventListener("click", () => {
      if (!result.url) return;
      content.panel.open({ anchor: button, url: result.url, mode: "list", heading: label });
    });
    return result;
  }

  /**
   * Match the button to the row it joined: copy the slot class YouTube's own actions use (so
   * spacing and wrapping line up with Play all), their button height, and the colours of the
   * last one (a secondary button like share or the menu, not the filled Play all/Subscribe).
   * Retried on later syncs until a sibling button has rendered with a real height.
   * @param {Element} target - Container the button was placed in.
   */
  function fitToRow(target) {
    const box = ctrl.root;
    const slot = [...target.children].find((child) => child !== box && child.tagName === "DIV" && child.className);
    if (slot && target.tagName === "YT-FLEXIBLE-ACTIONS-VIEW-MODEL") {
      box.className = `${slot.className} yt-dl-list-box`;
      box.dataset.ytDlSlot = "true";
    }
    const refs = [...target.querySelectorAll("button")].filter((b) => b !== ctrl.button && b.offsetHeight > 0);
    if (!refs.length) return;
    const btn = ctrl.button;
    const height = refs[0].offsetHeight;
    btn.style.height = `${height}px`;
    btn.style.lineHeight = `${height}px`;
    btn.style.borderRadius = `${height / 2}px`;
    // Feed the colours through the chip variable so content.css hover and focus rules still apply
    const style = getComputedStyle(refs[refs.length - 1]);
    if (refs.length > 1 && style.backgroundColor && style.backgroundColor !== "rgba(0, 0, 0, 0)") {
      btn.style.setProperty("--yt-dl-chip", style.backgroundColor);
      btn.style.setProperty("--yt-dl-chip-hover", `color-mix(in srgb, ${style.backgroundColor} 85%, ${style.color})`);
      btn.style.color = style.color;
    }
    ctrl.fitted = true;
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
    if (ctrl.url !== page.url && content.panel.isOpenFor(ctrl.button)) content.panel.close();
    ctrl.url = page.url;

    for (const stray of document.querySelectorAll('[data-yt-dl-owner="list"]')) {
      if (stray !== ctrl.root) stray.remove();
    }

    const target = content.findFirst(page.type === "playlist" ? PLAYLIST_TARGETS : CHANNEL_TARGETS);
    if (!target || !content.isVisible(target)) return;
    if (ctrl.root.parentElement !== target) {
      ctrl.fitted = false;
      ctrl.root.className = "yt-dl-list-box";
      delete ctrl.root.dataset.ytDlSlot;
      target.append(ctrl.root);
    }
    if (!ctrl.fitted) fitToRow(target);
  }

  /**
   * Remove the header button and close its panel.
   */
  function teardown() {
    if (!ctrl) return;
    if (content.panel.isOpenFor(ctrl.button)) content.panel.close();
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
