/**
 * Background script for YT-DL Extension
 * Handles communication between content script and local backend
 */

const BACKEND_URL = "http://127.0.0.1:18956";

console.log("[yt-dl-ext] Background script loaded");

/**
 * Check if the backend server is running and healthy
 * @returns {Promise<Object>} Health status object
 */
async function checkBackendHealth() {
  try {
    const response = await fetch(`${BACKEND_URL}/health`);
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    const data = await response.json();
    console.log("[yt-dl-ext] Backend healthy:", data);
    return {healthy: true, ...data};
  } catch (error) {
    console.error("[yt-dl-ext] Backend unreachable:", error.message);
    return {
      healthy: false,
      error: error.message,
    };
  }
}

/**
 * Start a download via the backend
 * @param {string} url - YouTube video URL
 * @param {string} format_id - Format to download (e.g., "best", "137")
 * @returns {Promise<Object>} Response with success flag and download_id or error
 */
async function startDownload(url, format_id) {
  try {
    console.log(`[yt-dl-ext] Starting download: ${url}, format: ${format_id}`);

    const response = await fetch(`${BACKEND_URL}/download`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        url: url,
        format_id: format_id,
      }),
    });

    if (response.status === 202) {
      // Success
      const data = await response.json();
      console.log(`[yt-dl-ext] Download started: ${data.download_id}`);
      return {
        success: true,
        download_id: data.download_id,
        status: data.status,
      };
    } else if (response.status === 409) {
      // Download already in progress
      const data = await response.json();
      console.warn("[yt-dl-ext] Download already in progress");
      return {
        success: false,
        error: "Busy",
      };
    } else if (response.status === 400) {
      // Invalid request
      const data = await response.json();
      console.error("[yt-dl-ext] Invalid request:", data.detail);
      return {
        success: false,
        error: "Invalid URL",
      };
    } else {
      // Other error
      console.error(`[yt-dl-ext] Unexpected status: ${response.status}`);
      return {
        success: false,
        error: `HTTP ${response.status}`,
      };
    }
  } catch (error) {
    console.error("[yt-dl-ext] Error starting download:", error);
    return {
      success: false,
      error: error.message,
    };
  }
}

/**
 * Get download progress from the backend
 * @param {string} download_id - The download ID to check
 * @returns {Promise<Object>} Response with success flag and progress or error
 */
async function getProgress(download_id) {
  try {
    const response = await fetch(`${BACKEND_URL}/progress/${download_id}`);

    if (response.status === 200) {
      const data = await response.json();
      return {
        success: true,
        progress: data,
      };
    } else if (response.status === 404) {
      console.warn(`[yt-dl-ext] Download not found: ${download_id}`);
      return {
        success: false,
        error: "Not found",
      };
    } else {
      console.error(`[yt-dl-ext] Unexpected status: ${response.status}`);
      return {
        success: false,
        error: `HTTP ${response.status}`,
      };
    }
  } catch (error) {
    console.error("[yt-dl-ext] Error getting progress:", error);
    return {
      success: false,
      error: error.message,
    };
  }
}

/**
 * Message handler for content script requests
 */
browser.runtime.onMessage.addListener((message, sender, sendResponse) => {
  console.log("[yt-dl-ext] Received message:", message.action);

  if (message.action === "checkBackendHealth") {
    checkBackendHealth().then(sendResponse);
    return true; // Required for async response
  }

  if (message.action === "startDownload") {
    startDownload(message.url, message.format_id).then(sendResponse);
    return true; // Required for async response
  }

  if (message.action === "getProgress") {
    getProgress(message.download_id).then(sendResponse);
    return true; // Required for async response
  }

  console.warn("[yt-dl-ext] Unknown action:", message.action);
  return false;
});

// Check backend health on startup
checkBackendHealth();
