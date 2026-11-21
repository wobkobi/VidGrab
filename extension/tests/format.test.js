const test = require("node:test");
const assert = require("node:assert/strict");

require("../shared/constants.js");
const format = require("../shared/format.js");

test("videoId reads watch, Shorts, live and youtu.be links only", () => {
  assert.equal(format.videoId("https://www.youtube.com/watch?v=abc123&t=42s&list=PL1"), "abc123");
  assert.equal(format.videoId("https://m.youtube.com/watch?v=abc123"), "abc123");
  assert.equal(format.videoId("https://music.youtube.com/watch?v=abc123"), "abc123");
  assert.equal(format.videoId("https://www.youtube.com/shorts/abc123?feature=share"), "abc123");
  assert.equal(format.videoId("https://www.youtube.com/live/abc123"), "abc123");
  assert.equal(format.videoId("https://youtu.be/abc123?t=5"), "abc123");
  for (const url of ["https://www.youtube.com/playlist?list=PL1", "https://www.youtube.com/@chan", "https://example.com/watch?v=abc123", "https://www.youtube.com/watch", "not a url", null]) {
    assert.equal(format.videoId(url), null);
  }
});

test("bytes formats binary units and rejects missing values", () => {
  assert.equal(format.bytes(0), "0 B");
  assert.equal(format.bytes(512), "512 B");
  assert.equal(format.bytes(1536), "1.5 KB");
  assert.equal(format.bytes(12.3 * 1024 * 1024), "12.3 MB");
  assert.equal(format.bytes(150 * 1024 * 1024), "150 MB");
  assert.equal(format.bytes(2 * 1024 ** 3), "2.0 GB");
  for (const bad of [null, undefined, "", -1, NaN, "abc"]) assert.equal(format.bytes(bad), "");
});

test("eta formats seconds and passes strings through", () => {
  assert.equal(format.eta(5), "0:05");
  assert.equal(format.eta(65), "1:05");
  assert.equal(format.eta(3723), "1:02:03");
  assert.equal(format.eta("00:42"), "00:42");
  assert.equal(format.eta(null), "");
  assert.equal(format.eta(undefined), "");
  assert.equal(format.eta(-3), "");
});

test("percent floors and clamps", () => {
  assert.equal(format.percent(45.9), "45%");
  assert.equal(format.percent(0), "0%");
  assert.equal(format.percent(100), "100%");
  assert.equal(format.percent(140), "100%");
  assert.equal(format.percent(-5), "0%");
  assert.equal(format.percent(null), "0%");
  assert.equal(format.percent("abc"), "0%");
});

test("parseRange normalises valid ranges", () => {
  assert.equal(format.parseRange("1-10,15"), "1-10,15");
  assert.equal(format.parseRange(" 1 - 10 , 15 "), "1-10,15");
  assert.equal(format.parseRange("3"), "3");
  assert.equal(format.parseRange("4-4"), "4");
  assert.equal(format.parseRange("2,5,9-12"), "2,5,9-12");
});

test("parseRange rejects empty and invalid input", () => {
  for (const bad of ["", "   ", null, undefined, 5, "0", "0-3", "10-1", "1-", "-3", "1,,2", "1,", "a-b", "1-2-3", "1;rm -rf"]) {
    assert.equal(format.parseRange(bad), null, `expected null for ${JSON.stringify(bad)}`);
  }
});

test("statusLabel uses the shared labels with a capitalised fallback", () => {
  assert.equal(format.statusLabel("error"), "Failed");
  assert.equal(format.statusLabel("complete"), "Complete");
  assert.equal(format.statusLabel("mystery"), "Mystery");
  assert.equal(format.statusLabel(""), "");
});

test("setupStatus treats a missing or unknown tools field as ready", () => {
  for (const health of [null, undefined, {}, {tools: null}, {tools: "x"}, {tools: {state: "ready"}}, {tools: {state: "odd"}}]) {
    assert.equal(format.setupStatus(health), null);
  }
});

test("setupStatus reports install progress, preferring the step text", () => {
  assert.deepEqual(format.setupStatus({tools: {state: "installing", percent: 42.7}}), {
    state: "installing",
    text: "Setting up yt-dlp and ffmpeg... 42%",
  });
  assert.equal(
    format.setupStatus({tools: {state: "installing", step: "Downloading ffmpeg...", percent: 10}}).text,
    "Downloading ffmpeg... 10%",
  );
  assert.equal(format.setupStatus({tools: {state: "installing", step: "Extracting\u2026"}}).text, "Extracting...");
  assert.equal(format.setupStatus({tools: {state: "installing", step: "  ", percent: null}}).text, "Setting up yt-dlp and ffmpeg...");
  assert.equal(format.setupStatus({tools: {state: "installing", percent: 150}}).text, "Setting up yt-dlp and ffmpeg... 100%");
});

test("setupStatus surfaces the setup error", () => {
  assert.deepEqual(format.setupStatus({tools: {state: "error", error: "checksum mismatch"}}), {
    state: "error",
    text: "Setting up yt-dlp and ffmpeg failed: checksum mismatch",
  });
  assert.equal(format.setupStatus({tools: {state: "error"}}).text, "Setting up yt-dlp and ffmpeg failed: Unknown error");
});

test("isPreparing holds until the first progress report", () => {
  assert.equal(format.isPreparing({status: "downloading", percent: 0, speed: null}), true);
  assert.equal(format.isPreparing({status: "downloading"}), true);
  assert.equal(format.isPreparing({status: "downloading", percent: 0, speed: "1.2MiB/s"}), false);
  assert.equal(format.isPreparing({status: "downloading", percent: 0.4, speed: null}), false);
  assert.equal(format.isPreparing({status: "queued", percent: 0}), false);
  assert.equal(format.isPreparing(null), false);
});

test("isConverting needs status processing with stage converting", () => {
  assert.equal(format.isConverting({status: "processing", stage: "converting"}), true);
  assert.equal(format.isConverting({status: "processing", stage: ""}), false);
  assert.equal(format.isConverting({status: "processing"}), false);
  assert.equal(format.isConverting({status: "downloading", stage: "converting"}), false);
  assert.equal(format.isConverting(null), false);
});

test("convertingLabel and convertingMeta show conversion progress", () => {
  assert.equal(format.convertingLabel({percent: 45.7}), "Converting 45%");
  assert.equal(format.convertingLabel({}), "Converting 0%");
  assert.equal(format.convertingMeta({percent: 45, speed: "2.4x", eta: 30}), "Converting 45% · 2.4x · ETA 0:30");
  assert.equal(format.convertingMeta({percent: 10, speed: "", eta: null}), "Converting 10%");
  assert.equal(format.convertingMeta({percent: 10, eta: "01:05"}), "Converting 10% · ETA 01:05");
});

test("parseTime reads seconds, m:ss and h:mm:ss", () => {
  assert.equal(format.parseTime("90"), 90);
  assert.equal(format.parseTime("1:30"), 90);
  assert.equal(format.parseTime(" 1:02:05 "), 3725);
  assert.equal(format.parseTime("0:05.7"), 5);
  assert.equal(format.parseTime("90:00"), 5400);
  assert.equal(format.parseTime("0"), 0);
  assert.equal(format.parseTime(""), null);
  assert.equal(format.parseTime("1:75"), null);
  assert.equal(format.parseTime("1:2:3:4"), null);
  assert.equal(format.parseTime("-5"), null);
  assert.equal(format.parseTime("abc"), null);
  assert.equal(format.parseTime(undefined), null);
});

test("clock formats seconds the way parseTime reads them", () => {
  assert.equal(format.clock(0), "0:00");
  assert.equal(format.clock(90.9), "1:30");
  assert.equal(format.clock(3725), "1:02:05");
  assert.equal(format.parseTime(format.clock(4000)), 4000);
});
