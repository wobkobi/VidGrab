/**
 * Content script for YT-DL Extension
 * Injects download UI into YouTube pages
 */

console.log("[yt-dl-ext] Content script loaded");

// State management
let downloadButton = null;
let currentVideoUrl = null;
let isDownloading = false;

/**
 * Get the current video URL from the page
 * @returns {string|null} YouTube video URL or null if not found
 */
function getCurrentVideoUrl() {
  const url = window.location.href;
  if (url.includes("youtube.com/watch?v=")) {
    return url.split("&")[0]; // Get base URL without extra params
  }
  return null;
}

/**
 * Find the Like button container where we'll inject our button
 * @returns {HTMLElement|null} Container element or null if not found
 */
function findLikeButtonContainer() {
  // YouTube's button container (as of 2024-2026)
  // This selector may need updating if YouTube changes their DOM
  const selectors = [
    "#top-level-buttons-computed", // Primary container
    "#top-level-buttons", // Fallback
    "ytd-menu-renderer.ytd-video-primary-info-renderer", // Older layout
  ];

  for (const selector of selectors) {
    const container = document.querySelector(selector);
    if (container) {
      console.log(`[yt-dl-ext] Found button container: ${selector}`);
      return container;
    }
  }

  console.warn("[yt-dl-ext] Could not find Like button container");
  return null;
}

/**
 * Create the Download button element
 * @returns {HTMLElement} The button element
 */
function createDownloadButton() {
  const button = document.createElement("button");
  button.id = "yt-dl-ext-download-btn";
  button.className = "yt-dl-ext-button";
  button.textContent = "⬇️ Download";
  button.title = "Download this video";
  button.disabled = false;

  return button;
}

/**
 * Update button state based on download status
 * @param {string} state - One of: active, downloading, complete, error
 * @param {string} message - Optional message to display
 */
function updateButtonState(state, message = "") {
  if (!downloadButton) return;

  switch (state) {
    case "active":
      downloadButton.textContent = "⬇️ Download";
      downloadButton.disabled = false;
      downloadButton.className = "yt-dl-ext-button";
      break;

    case "downloading":
      downloadButton.textContent = `⏳ ${message || "Downloading..."}`;
      downloadButton.disabled = true;
      downloadButton.className = "yt-dl-ext-button yt-dl-ext-downloading";
      break;

    case "complete":
      downloadButton.textContent = "✅ Complete!";
      downloadButton.disabled = false;
      downloadButton.className = "yt-dl-ext-button yt-dl-ext-complete";
      // Reset after 3 seconds
      setTimeout(() => updateButtonState("active"), 3000);
      break;

    case "error":
      downloadButton.textContent = `❌ ${message || "Error"}`;
      downloadButton.disabled = false;
      downloadButton.className = "yt-dl-ext-button yt-dl-ext-error";
      // Reset after 5 seconds
      setTimeout(() => updateButtonState("active"), 5000);
      break;
  }
}

/**
 * Handle download button click
 */
async function handleDownloadClick() {
  if (isDownloading) {
    console.warn("[yt-dl-ext] Download already in progress");
    return;
  }

  const videoUrl = getCurrentVideoUrl();
  if (!videoUrl) {
    console.error("[yt-dl-ext] Could not get video URL");
    updateButtonState("error", "Invalid URL");
    return;
  }

  console.log(`[yt-dl-ext] Starting download for: ${videoUrl}`);
  isDownloading = true;
  updateButtonState("downloading");

  try {
    // Send download request to background script
    const response = await browser.runtime.sendMessage({
      action: "startDownload",
      url: videoUrl,
      format_id: "best", // Default to best quality (Phase 3 will add dropdown)
    });

    if (response.success) {
      console.log(`[yt-dl-ext] Download started: ${response.download_id}`);
      // Poll for progress
      pollDownloadProgress(response.download_id);
    } else {
      console.error("[yt-dl-ext] Download failed:", response.error);
      updateButtonState("error", response.error);
      isDownloading = false;
    }
  } catch (error) {
    console.error("[yt-dl-ext] Error starting download:", error);
    updateButtonState("error", "Backend error");
    isDownloading = false;
  }
}

/**
 * Poll backend for download progress
 * @param {string} downloadId - The download ID to poll
 */
async function pollDownloadProgress(downloadId) {
  const pollInterval = 1000; // Poll every 1 second
  let attempts = 0;
  const maxAttempts = 300; // 5 minutes max

  const poll = async () => {
    if (attempts >= maxAttempts) {
      console.error("[yt-dl-ext] Download polling timeout");
      updateButtonState("error", "Timeout");
      isDownloading = false;
      return;
    }

    attempts++;

    try {
      const response = await browser.runtime.sendMessage({
        action: "getProgress",
        download_id: downloadId,
      });

      if (response.success) {
        const progress = response.progress;
        console.log(`[yt-dl-ext] Progress: ${progress.status} ${progress.percent}%`);

        if (progress.status === "downloading") {
          const percent = Math.round(progress.percent);
          updateButtonState("downloading", `${percent}%`);
          setTimeout(poll, pollInterval);
        } else if (progress.status === "complete") {
          console.log(`[yt-dl-ext] Download complete: ${progress.filename}`);
          updateButtonState("complete");
          isDownloading = false;
        } else if (progress.status === "error") {
          console.error(`[yt-dl-ext] Download error: ${progress.error}`);
          updateButtonState("error", "Failed");
          isDownloading = false;
        } else {
          // Still starting
          updateButtonState("downloading", "Starting...");
          setTimeout(poll, pollInterval);
        }
      } else {
        console.error("[yt-dl-ext] Progress check failed:", response.error);
        updateButtonState("error", "Check failed");
        isDownloading = false;
      }
    } catch (error) {
      console.error("[yt-dl-ext] Error polling progress:", error);
      updateButtonState("error", "Poll error");
      isDownloading = false;
    }
  };

  poll();
}

/**
 * Inject the download button into the page
 */
function injectDownloadButton() {
  // Remove existing button if present
  if (downloadButton) {
    downloadButton.remove();
    downloadButton = null;
  }

  const container = findLikeButtonContainer();
  if (!container) {
    console.warn("[yt-dl-ext] Cannot inject button: container not found");
    return;
  }

  downloadButton = createDownloadButton();
  downloadButton.addEventListener("click", handleDownloadClick);

  // Insert button at the start of the container
  container.insertBefore(downloadButton, container.firstChild);

  console.log("[yt-dl-ext] Download button injected");
}

/**
 * Initialize the extension on YouTube video pages
 */
async function init() {
  console.log("[yt-dl-ext] Initializing on YouTube video page");

  // Check backend health via background script
  const health = await browser.runtime.sendMessage({
    action: "checkBackendHealth",
  });

  console.log("[yt-dl-ext] Backend status:", health);

  if (!health.healthy) {
    console.warn("[yt-dl-ext] Backend is not running. Start the server at: python backend/server.py");
    // Still inject button, but it will show error on click
  }

  // Get current video URL
  currentVideoUrl = getCurrentVideoUrl();
  if (!currentVideoUrl) {
    console.log("[yt-dl-ext] Not on a video page, skipping injection");
    return;
  }

  // Wait for YouTube's dynamic content to load
  setTimeout(() => {
    injectDownloadButton();
  }, 1000);
}

/**
 * Handle YouTube SPA navigation
 * YouTube is a Single Page App, so we need to re-inject on navigation
 */
function handleNavigation() {
  const newUrl = getCurrentVideoUrl();

  if (newUrl && newUrl !== currentVideoUrl) {
    console.log(`[yt-dl-ext] Navigated to new video: ${newUrl}`);
    currentVideoUrl = newUrl;
    isDownloading = false;

    // Re-inject button after navigation
    setTimeout(() => {
      injectDownloadButton();
    }, 1000);
  }
}

// Listen for YouTube's navigation events
document.addEventListener("yt-navigate-finish", handleNavigation);

// Initial load
init();
