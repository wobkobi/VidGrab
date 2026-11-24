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
   * @returns {{root: HTMLElement, button: HTMLElement, type: string, url: string|null, fitted: boolean, row: Element|null, host: Element|null, below: boolean|null, naturalWidth: number, layout: string}} Controller.
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
    const result = { root, button, type, url: null, fitted: false, row: null, host: null, below: null, naturalWidth: 0, layout: "" };
    button.addEventListener("click", () => {
      if (!result.url) return;
      content.panel.open({ anchor: button, url: result.url, mode: "list", heading: label });
    });
    return result;
  }

  /**
   * Find the element that lays YouTube's actions out side by side. The flexible actions view
   * model often wraps them in a single row div (`...ActionRow` / `...__action-row`, as on
   * channel and playlist headers); the button belongs inside that row when it fits there.
   * @param {Element} target - Container matched by the target selectors.
   * @returns {Element} The action row.
   */
  function actionRow(target) {
    let row = target;
    for (;;) {
      // Skip the button itself, which sits beside the row when it is on a line of its own
      const children = [...row.children].filter((child) => !ctrl || child !== ctrl.root);
      if (children.length !== 1) break;
      const only = children[0];
      if (only.tagName !== "DIV" || !/row/i.test(only.className)) break;
      row = only;
    }
    return row;
  }

  /**
   * Class of one of YouTube's own action slots in the row (the wrapper around Play all or
   * Subscribe), so the button's spacing matches theirs. Only inside flexible actions, where
   * the slot class carries no page-specific styling.
   * @param {Element} row - Action row.
   * @returns {string} The slot class, or "" when there is none to copy.
   */
  function slotClass(row) {
    if (!row.closest("yt-flexible-actions-view-model")) return "";
    const slot = [...row.children].find((child) => child !== ctrl.root && child.tagName === "DIV" && child.className && !/row/i.test(child.className));
    return slot ? slot.className : "";
  }

  /**
   * Whether a computed background is a tonal fill: partly see-through, like YouTube's
   * secondary buttons (share, Join, Subscribed), as opposed to a solid fill (Play all,
   * Subscribe) or none at all (outlined buttons).
   * @param {string} colour - Computed background-color.
   * @returns {boolean} True for a tonal fill.
   */
  function isTonal(colour) {
    const match = /^rgba\(.*,\s*([\d.]+)\)$/.exec(colour || "");
    const alpha = match ? Number(match[1]) : 1;
    return alpha > 0 && alpha < 1;
  }

  /**
   * Match the button to YouTube's buttons in the row: their height, and the colours of the
   * last tonal one (a secondary button like share, Join or Subscribed). Solid ones are never
   * copied: a filled Play all or Subscribe would turn the button into a second white pill.
   * With no tonal button the button keeps its own chip colours, whose text colour follows
   * YouTube's theme. Links count as buttons: on your own channel, Customise channel and Manage
   * videos are links styled as buttons. Retried on later syncs until a button has a real height.
   * @param {Element} row - Action row.
   */
  function fitToRow(row) {
    const refs = [...row.querySelectorAll("button, a[href]")].filter((b) => !ctrl.root.contains(b) && b.offsetHeight > 0);
    if (!refs.length) return;
    const btn = ctrl.button;
    const height = refs[0].offsetHeight;
    btn.style.height = `${height}px`;
    btn.style.lineHeight = `${height}px`;
    btn.style.borderRadius = `${height / 2}px`;
    // Cleared first, so a refit after a layout change can fall back to the default colours
    btn.style.removeProperty("--yt-dl-chip");
    btn.style.removeProperty("--yt-dl-chip-hover");
    btn.style.color = "";
    // Feed the colours through the chip variable so content.css hover and focus rules still apply
    const tonal = refs.map((ref) => getComputedStyle(ref)).filter((style) => isTonal(style.backgroundColor));
    const style = tonal[tonal.length - 1];
    if (style) {
      btn.style.setProperty("--yt-dl-chip", style.backgroundColor);
      btn.style.setProperty("--yt-dl-chip-hover", `color-mix(in srgb, ${style.backgroundColor} 85%, ${style.color})`);
      btn.style.color = style.color;
    }
    ctrl.fitted = true;
  }

  /**
   * Whether the button fits beside YouTube's actions without squeezing them. Their free space
   * is read with the button hidden, because a nowrap row (the playlist sidebar) shrinks Play
   * all to make room rather than wrapping, so measuring with the button in place always looks
   * like a fit. Hiding and restoring in one go forces a layout but never paints.
   * @param {Element} row - Action row.
   * @returns {boolean} True when the unstretched button fits after the last action.
   */
  function fitsBeside(row) {
    const box = ctrl.root;
    const display = box.style.display;
    box.style.display = "none";
    const rowRect = row.getBoundingClientRect();
    const rights = [...row.children]
      .filter((child) => child !== box)
      .map((child) => child.getBoundingClientRect())
      .filter((rect) => rect.width > 0)
      .map((rect) => rect.right);
    const gap = parseFloat(getComputedStyle(row).columnGap) || 8;
    box.style.display = display;
    const lastRight = rights.length ? Math.max(...rights) : rowRect.left;
    return rowRect.right - lastRight >= gap + ctrl.naturalWidth;
  }

  /**
   * Put the button beside the actions, or on a full-width line of its own under them. A row
   * that wraps keeps the button and pushes it to a new line; a nowrap row can't, so the button
   * goes right after the row instead, copying the row's class for the same spacing.
   * @param {Element} row - Action row.
   * @param {boolean} below - True for its own line under the actions.
   */
  function setPlacement(row, below) {
    const box = ctrl.root;
    const style = getComputedStyle(row);
    const host = below && !holdsOwnLine(style) ? row.parentElement : row;
    let className = "yt-dl-list-box";
    const slot = slotClass(row);
    if (host === row && slot) className = `${slot} yt-dl-list-box`;
    else if (host !== row && /row/i.test(row.className)) className = `${row.className} yt-dl-list-box`;
    box.className = className;
    if (host === row && slot) box.dataset.ytDlSlot = "true";
    else delete box.dataset.ytDlSlot;
    if (below) box.dataset.ytDlWrapped = "true";
    else delete box.dataset.ytDlWrapped;
    // A stacked row (narrow channel header) may have no gap of its own, which left the button
    // touching a full-width Subscribe
    const gapless = !(parseFloat(style.rowGap) > 0);
    box.style.marginTop = below && host === row && gapless ? "8px" : "";
    if (host === row) {
      if (box.parentElement !== row) row.append(box);
    } else if (box.previousElementSibling !== row) {
      row.after(box);
    }
    ctrl.host = host;
    ctrl.below = below;
    ctrl.layout = layoutKey(style);
  }

  /**
   * Whether a row can give the button a line of its own inside it: one that wraps, stacks
   * (column flex, as the channel header does when narrow) or isn't flex at all. Only a nowrap
   * side-by-side row can't, and would squeeze its own buttons instead.
   * @param {CSSStyleDeclaration} style - The row's computed style.
   * @returns {boolean} True when the button can sit inside the row on its own line.
   */
  function holdsOwnLine(style) {
    return !style.display.includes("flex") || style.flexDirection.startsWith("column") || style.flexWrap !== "nowrap";
  }

  /**
   * The parts of a row's layout that decide where the button goes, so a change (YouTube
   * stacking the header at a narrow width) re-places it.
   * @param {CSSStyleDeclaration} style - The row's computed style.
   * @returns {string} Layout key.
   */
  function layoutKey(style) {
    return `${style.display}|${style.flexDirection}|${style.flexWrap}|${style.rowGap}`;
  }

  /**
   * Place the button for the current row: inline first, to fit its height and measure its
   * natural width, then on its own full-width line when that width doesn't fit beside the
   * actions (including when they stack and fill the width themselves).
   * @param {Element} row - Action row.
   */
  function place(row) {
    const box = ctrl.root;
    if (ctrl.row !== row) {
      ctrl.row = row;
      ctrl.fitted = false;
      ctrl.below = null;
      ctrl.naturalWidth = 0;
    }
    // YouTube swapped the header layout (compact below about 640px): its buttons change size
    // and style with it, so fit and measure again
    if (ctrl.layout && layoutKey(getComputedStyle(row)) !== ctrl.layout) ctrl.fitted = false;
    if (!ctrl.fitted) {
      setPlacement(row, false);
      fitToRow(row);
      if (!ctrl.fitted) return;
      ctrl.naturalWidth = ctrl.button.getBoundingClientRect().width;
    }
    const style = getComputedStyle(row);
    // A column stacks whatever the free width beside its buttons
    const below = style.flexDirection.startsWith("column") && style.display.includes("flex") ? true : !fitsBeside(row);
    if (below !== ctrl.below || box.parentElement !== ctrl.host || layoutKey(style) !== ctrl.layout) {
      setPlacement(row, below);
    }
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
    place(actionRow(target));
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

  window.addEventListener("resize", () => {
    if (ctrl && ctrl.row && ctrl.row.isConnected) place(ctrl.row);
  });
  content.on("navigate", sync);
  content.on("dom", () => {
    const type = content.page.type;
    if (type === "playlist" || type === "channel") sync();
  });

  content.start();
})();
