// "Download with VidGrab" item in the three-dot menu of videos on the home page, search results, the
// watch page's related videos and playlists. Downloads with the saved defaults.
var YTDL = globalThis.YTDL || (globalThis.YTDL = {});

(function () {
  "use strict";

  const content = YTDL.content;
  if (!content || !content.ready) return;

  const el = content.el;

  // Video cards, innermost first: closest() returns the nearest one, so a lockup inside a
  // rich-item renderer resolves to the lockup
  const ITEM_SELECTOR = [
    "yt-lockup-view-model",
    "ytm-shorts-lockup-view-model",
    "ytm-shorts-lockup-view-model-v2",
    "ytd-rich-item-renderer",
    "ytd-video-renderer",
    "ytd-compact-video-renderer",
    "ytd-grid-video-renderer",
    "ytd-playlist-video-renderer",
    "ytd-playlist-panel-video-renderer",
    "ytd-reel-item-renderer",
  ].join(",");
  // Single-video rows inside a list, whose links carry &list= without the row being a list
  const LIST_ROWS = "ytd-playlist-video-renderer, ytd-playlist-panel-video-renderer";
  const VIDEO_LINK = 'a[href*="/watch?v="], a[href*="/shorts/"]';
  // The open menu's item list: the older Polymer menu, then the newer list view model
  const MENU_LISTS = ["ytd-menu-popup-renderer tp-yt-paper-listbox", "ytd-menu-popup-renderer #items", "yt-list-view-model", '[role="menu"]', '[role="listbox"]'];
  // How long after a click on a card's menu button the opening menu still belongs to that card
  const CLAIM_MS = 1500;
  const TOAST_MS = 3500;

  // Video whose menu button was clicked last, and when
  let pending = null;
  let watcher = null;
  let toastTimer = null;

  /**
   * Find the video a card links to.
   * @param {Element} card - Video card element.
   * @returns {string|null} Watch URL for the video, or null when the card is not a single video.
   */
  function cardVideoUrl(card) {
    const link = card.querySelector(VIDEO_LINK);
    if (!link) return null;
    const href = link.getAttribute("href") || "";
    // A playlist or Mix card links to its first video with &list=; skip those, since the item
    // would download one video while the card stands for the whole list. Any link on the card
    // counts (the first one isn't always the list link), and Mix ids start with RD ("My Mix"
    // is RDMM). Rows of a playlist page or the watch-page queue carry &list= too, but each
    // stands for one video.
    if (!card.matches(LIST_ROWS) && !card.querySelector('a[href*="/shorts/"]')) {
      if (card.querySelector('a[href^="/playlist?list="], a[href*="start_radio="], a[href*="list=RD"]')) return null;
    }
    const id = content.videoIdFromUrl(href);
    return id ? `https://www.youtube.com/watch?v=${id}` : null;
  }

  /**
   * Find the open dropdown's item list.
   * @returns {Element|null} The list to add the item to.
   */
  function openMenuList() {
    for (const dropdown of document.querySelectorAll("ytd-popup-container tp-yt-iron-dropdown")) {
      if (!content.isVisible(dropdown) || dropdown.getAttribute("aria-hidden") === "true") continue;
      for (const selector of MENU_LISTS) {
        const list = dropdown.querySelector(selector);
        if (list && content.isVisible(list)) return list;
      }
    }
    return null;
  }

  /**
   * Close YouTube's open menu. The dropdown's own close() runs through the page's script
   * (wrappedJSObject is Firefox's way in); Escape is the fallback.
   * @param {Element} list - An element inside the open menu.
   */
  function closeMenu(list) {
    const dropdown = list.closest("tp-yt-iron-dropdown");
    try {
      const page = dropdown && dropdown.wrappedJSObject;
      if (page && typeof page.close === "function") {
        page.close();
        return;
      }
    } catch {
      // fall through to Escape
    }
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", code: "Escape", keyCode: 27, bubbles: true }));
  }

  /**
   * Show a short message in the bottom-left corner, like YouTube's own toasts.
   * @param {string} text - Message.
   * @param {boolean} [error] - Show it as an error.
   */
  function toast(text, error = false) {
    let node = document.querySelector(".yt-dl-toast");
    if (!node) {
      node = el("div", { className: "yt-dl-toast", role: "status" });
      document.body.append(node);
    }
    node.textContent = text;
    node.dataset.tone = error ? "error" : "info";
    node.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => {
      node.hidden = true;
    }, TOAST_MS);
  }

  /**
   * Queue a download of url with the saved defaults and report the result in a toast.
   * @param {string} url - Watch URL.
   */
  async function download(url) {
    toast("Starting download...");
    const res = await content.send({ type: "createJobs", url, options: { ...content.settings.defaults } });
    if (res.ok) {
      toast("Download queued - progress is in the VidGrab toolbar popup");
      return;
    }
    if (res.code === YTDL.constants.HELPER_MISSING) toast(content.helperMissingText(res), true);
    else toast(res.error || "Could not start the download", true);
  }

  /**
   * Build the menu item. It is rebuilt for every menu, since YouTube reuses one dropdown for
   * every card and may re-render its contents.
   * @param {string} url - Video to download.
   * @param {Element} list - Menu list it goes into.
   * @returns {HTMLElement} The item.
   */
  function buildItem(url, list) {
    const item = el("div", { className: "yt-dl-menu-item", role: "menuitem", tabindex: "0" }, [content.brandIcon(), el("span", { text: "Download with VidGrab" })]);
    const activate = (event) => {
      event.preventDefault();
      event.stopPropagation();
      closeMenu(list);
      download(url);
    };
    item.addEventListener("click", activate);
    item.addEventListener("keydown", (event) => {
      if (event.key === "Enter" || event.key === " ") activate(event);
    });
    return item;
  }

  /**
   * Add the item to the open menu when it belongs to the card clicked last. Returns whether
   * the menu was found, so the watcher can stop.
   * @returns {boolean} True once the menu was handled.
   */
  function inject() {
    if (!pending || Date.now() - pending.at > CLAIM_MS) return true;
    const list = openMenuList();
    if (!list) return false;
    for (const old of document.querySelectorAll(".yt-dl-menu-item")) old.remove();
    list.prepend(buildItem(pending.url, list));
    pending = null;
    return true;
  }

  /**
   * Watch the page for the menu to open, for at most CLAIM_MS. A dedicated observer keeps the
   * item from popping in late, which the throttled "dom" event would cause.
   */
  function watchForMenu() {
    if (watcher) watcher.disconnect();
    if (inject()) return;
    watcher = new MutationObserver(() => {
      if (inject()) stop();
    });
    const stop = () => {
      if (!watcher) return;
      watcher.disconnect();
      watcher = null;
    };
    watcher.observe(document.body, { childList: true, subtree: true, attributes: true, attributeFilter: ["aria-hidden", "style"] });
    setTimeout(stop, CLAIM_MS);
  }

  // Capture phase, so the card is known before YouTube's own handler opens the menu
  document.addEventListener(
    "click",
    (event) => {
      const target = event.target instanceof Element ? event.target : null;
      if (!target || target.closest(".yt-dl-menu-item")) return;
      const button = target.closest("button, yt-icon-button, [role='button']");
      if (!button || button.closest("a[href]")) return;
      const card = button.closest(ITEM_SELECTOR);
      if (!card) return;
      const url = cardVideoUrl(card);
      if (!url) return;
      pending = { url, at: Date.now() };
      // Remove a stale item at once, so a menu reopened for another card never shows it
      for (const old of document.querySelectorAll(".yt-dl-menu-item")) old.remove();
      watchForMenu();
    },
    true,
  );
})();
