// HTTP client for the local backend. Every call resolves to {ok: true, data} or {ok: false, error, status?}.
var YTDL = globalThis.YTDL || (globalThis.YTDL = {});

YTDL.api = (function () {
  const BASE_URL = "http://127.0.0.1:18956";
  const NOT_RUNNING = "Backend not running";
  const TOKEN_HEADER = "X-YTDL-Token";
  // Creating jobs for a playlist or channel lists its entries first, which the backend allows 120s
  // for. The update outlasts the backend's 300s pip timeout so a slow pip run still reports back.
  const TIMEOUTS = {health: 2000, info: 25000, jobs: 5000, create: 130000, update: 320000};

  // Install token from the native host; sent on every call except /health
  let token = null;

  /**
   * Send one request to the backend with a timeout. The custom header forces a CORS
   * preflight, which the backend only accepts from moz-extension:// origins; the token
   * header is what tells this extension apart from any other.
   * @param {string} method - HTTP method.
   * @param {string} path - Path including any query string.
   * @param {{body?: object, timeout?: number, noToken?: boolean}} [opts] - JSON body, timeout in ms, and whether to leave the token out.
   * @returns {Promise<{ok: boolean, data?: any, error?: string, detail?: string, status?: number}>} Normalised result.
   */
  async function request(method, path, opts = {}) {
    const controller = new AbortController();
    const timeout = opts.timeout || TIMEOUTS.jobs;
    const timer = setTimeout(() => controller.abort(), timeout);
    const headers = {"X-YTDL-Client": "1"};
    if (token && !opts.noToken) headers[TOKEN_HEADER] = token;
    if (opts.body !== undefined) headers["Content-Type"] = "application/json";

    let response;
    try {
      response = await fetch(BASE_URL + path, {
        method,
        headers,
        body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
        signal: controller.signal,
        cache: "no-store",
      });
    } catch (err) {
      clearTimeout(timer);
      if (err && err.name === "AbortError") {
        // A timed-out health check means nothing answered; anything slower is a busy backend.
        if (timeout <= TIMEOUTS.health) return {ok: false, error: NOT_RUNNING};
        return {ok: false, error: "The backend took too long to respond", status: 504};
      }
      return {ok: false, error: NOT_RUNNING};
    }

    let payload = null;
    try {
      const text = await response.text();
      payload = text ? JSON.parse(text) : null;
    } catch (err) {
      payload = null;
    } finally {
      clearTimeout(timer);
    }

    if (response.ok) return {ok: true, data: payload};
    const result = {ok: false, error: errorMessage(payload, response.status), status: response.status};
    if (payload && typeof payload.error === "string" && typeof payload.detail === "string" && payload.detail) {
      result.detail = payload.detail;
    }
    return result;
  }

  /**
   * Build a short user-facing message from a backend error body ({error, detail}). The
   * detail can be 20 lines of yt-dlp output, so it travels separately as `detail` for
   * tooltips and logs. FastAPI's own validation errors use `detail` as a list.
   * @param {any} payload - Parsed response body, or null.
   * @param {number} status - HTTP status code.
   * @returns {string} Message to show the user.
   */
  function errorMessage(payload, status) {
    if (payload && typeof payload.error === "string" && payload.error) return payload.error;
    if (payload && typeof payload.detail === "string" && payload.detail) return payload.detail;
    if (status === 422) return "Invalid request";
    if (status === 403) return "Request blocked by the backend";
    return `Backend error (HTTP ${status})`;
  }

  /**
   * Format bytes as lowercase hex.
   * @param {ArrayBuffer|Uint8Array} buffer - Bytes to format.
   * @returns {string} Hex string.
   */
  function toHex(buffer) {
    return Array.from(new Uint8Array(buffer), (b) => b.toString(16).padStart(2, "0")).join("");
  }

  /**
   * Make a random nonce for {@link verifyProof}.
   * @returns {string} 32 hex characters.
   */
  function newNonce() {
    return toHex(crypto.getRandomValues(new Uint8Array(16)));
  }

  /**
   * Check the `proof` from GET /health: HMAC-SHA256 of the nonce under the install token.
   * Only the real backend can read the token file, so a program squatting on the port fails.
   * @param {string} nonce - Nonce sent with the health request.
   * @param {unknown} proof - The `proof` field of the health body.
   * @returns {Promise<boolean>} True when the proof matches the current token.
   */
  async function verifyProof(nonce, proof) {
    if (!token || typeof proof !== "string") return false;
    const encoder = new TextEncoder();
    const key = await crypto.subtle.importKey("raw", encoder.encode(token), {name: "HMAC", hash: "SHA-256"}, false, ["sign"]);
    const expected = toHex(await crypto.subtle.sign("HMAC", key, encoder.encode(nonce)));
    return expected === proof;
  }

  return {
    BASE_URL,
    NOT_RUNNING,
    TIMEOUTS,
    request,
    newNonce,
    verifyProof,
    /**
     * Set the install token sent with every call.
     * @param {string|null} value - Token from the native host, or null to forget it.
     */
    setToken(value) {
      token = typeof value === "string" && value ? value : null;
    },
    /**
     * Whether a token is known.
     * @returns {boolean} True once the native host has supplied one.
     */
    hasToken() {
      return Boolean(token);
    },
    health: (nonce) =>
      request("GET", nonce ? `/health?nonce=${encodeURIComponent(nonce)}` : "/health", {timeout: TIMEOUTS.health, noToken: true}),
    info: (url) => request("GET", `/info?url=${encodeURIComponent(url)}`, {timeout: TIMEOUTS.info}),
    createJobs: (url, options, items) => {
      const body = {url, options};
      if (items) body.items = items;
      return request("POST", "/jobs", {body, timeout: TIMEOUTS.create});
    },
    listJobs: () => request("GET", "/jobs", {timeout: TIMEOUTS.jobs}),
    jobAction: (id, action) =>
      request("POST", `/jobs/${encodeURIComponent(id)}/${action}`, {timeout: TIMEOUTS.jobs}),
    clearFinished: () => request("DELETE", "/jobs?finished=true", {timeout: TIMEOUTS.jobs}),
    putSettings: (settings) => request("PUT", "/settings", {body: settings, timeout: TIMEOUTS.jobs}),
    update: () => request("POST", "/update", {timeout: TIMEOUTS.update}),
  };
})();

if (typeof module !== "undefined") module.exports = YTDL.api;
