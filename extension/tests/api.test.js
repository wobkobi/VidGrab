const test = require("node:test");
const assert = require("node:assert/strict");
const {createHmac} = require("node:crypto");

const api = require("../shared/api.js");

/**
 * Replace global fetch with a stub that records requests and answers with one response.
 * @param {number} status - HTTP status to answer with.
 * @param {object} body - JSON body to answer with.
 * @returns {Array<{url: string, init: object}>} Recorded requests.
 */
function stubFetch(status, body) {
  const calls = [];
  globalThis.fetch = async (url, init) => {
    calls.push({url, init});
    return {ok: status >= 200 && status < 300, status, text: async () => JSON.stringify(body)};
  };
  return calls;
}

test("verifyProof matches the backend's HMAC-SHA256 hex of the nonce", async () => {
  api.setToken("k".repeat(43));
  const nonce = api.newNonce();
  assert.match(nonce, /^[0-9a-f]{32}$/);
  const proof = createHmac("sha256", "k".repeat(43)).update(nonce).digest("hex");
  assert.equal(await api.verifyProof(nonce, proof), true);
  const tampered = proof.slice(0, -1) + (proof.endsWith("0") ? "1" : "0");
  assert.equal(await api.verifyProof(nonce, tampered), false);
  assert.equal(await api.verifyProof(nonce, undefined), false);
  api.setToken(null);
  assert.equal(await api.verifyProof(nonce, proof), false);
});

test("token goes on API calls but not on /health", async () => {
  api.setToken("secret-token-value-that-is-long-enough");
  const calls = stubFetch(200, {jobs: []});
  await api.listJobs();
  await api.health("abc");
  assert.equal(calls[0].init.headers["X-YTDL-Token"], "secret-token-value-that-is-long-enough");
  assert.equal(calls[1].init.headers["X-YTDL-Token"], undefined);
  assert.ok(calls[1].url.endsWith("/health?nonce=abc"));
  api.setToken(null);
});

test("errors keep the short message and carry the detail separately", async () => {
  stubFetch(502, {error: "Sign in to confirm your age", detail: "[youtube] abc: Downloading webpage\nERROR: ..."});
  const res = await api.info("https://www.youtube.com/watch?v=jNQXAC9IVRw");
  assert.equal(res.ok, false);
  assert.equal(res.status, 502);
  assert.equal(res.error, "Sign in to confirm your age");
  assert.match(res.detail, /Downloading webpage/);
});

test("update waits over five minutes for a slow yt-dlp download", () => {
  assert.ok(api.TIMEOUTS.update > 300000);
});

test("a 503 during setup keeps the backend's setup message and detail", async () => {
  stubFetch(503, {error: "Setting up yt-dlp and ffmpeg", detail: "Downloading ffmpeg 40%"});
  const res = await api.createJobs("https://www.youtube.com/watch?v=jNQXAC9IVRw", {kind: "video"});
  assert.equal(res.ok, false);
  assert.equal(res.status, 503);
  assert.equal(res.error, "Setting up yt-dlp and ffmpeg");
  assert.equal(res.detail, "Downloading ffmpeg 40%");
});

test("requests go to the port the native host reported", async () => {
  const calls = stubFetch(200, {jobs: []});
  api.setPort(24680);
  await api.listJobs();
  assert.ok(calls[0].url.startsWith("http://127.0.0.1:24680/"));
  // A helper too old to report a port, or a bad value, falls back to the old fixed port
  for (const bad of [undefined, "abc", 0, 70000, 1.5]) {
    api.setPort(bad);
    assert.equal(api.baseUrl(), "http://127.0.0.1:18956");
  }
});
