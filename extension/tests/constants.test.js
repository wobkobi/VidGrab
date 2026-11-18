const test = require("node:test");
const assert = require("node:assert/strict");

const constants = require("../shared/constants.js");

test("NEEDS_FFMPEG covers MP3 and video above 720p", () => {
  assert.equal(constants.NEEDS_FFMPEG({kind: "audio", audio_format: "mp3"}), true);
  assert.equal(constants.NEEDS_FFMPEG({kind: "audio", audio_format: "m4a"}), false);
  assert.equal(constants.NEEDS_FFMPEG({kind: "video", quality: "best"}), true);
  assert.equal(constants.NEEDS_FFMPEG({kind: "video", quality: "2160"}), true);
  assert.equal(constants.NEEDS_FFMPEG({kind: "video", quality: "1080"}), true);
  assert.equal(constants.NEEDS_FFMPEG({kind: "video", quality: "720"}), false);
  assert.equal(constants.NEEDS_FFMPEG({kind: "video", quality: "360"}), false);
  assert.equal(constants.NEEDS_FFMPEG(null), false);
});

test("mergeSettings returns the defaults for missing or malformed input", () => {
  for (const stored of [undefined, null, "x", [], {}]) {
    assert.deepEqual(constants.mergeSettings(stored), constants.DEFAULT_SETTINGS);
  }
});

test("mergeSettings deep-merges defaults and selectors", () => {
  const merged = constants.mergeSettings({
    defaults: {kind: "audio"},
    selectors: {watchInsertion: "last"},
    cookies: true,
  });
  assert.equal(merged.defaults.kind, "audio");
  assert.equal(merged.defaults.quality, "best");
  assert.equal(merged.defaults.audio_bitrate, "192");
  assert.equal(merged.selectors.watchInsertion, "last");
  assert.equal(merged.selectors.watchContainer, constants.DEFAULT_SETTINGS.selectors.watchContainer);
  assert.equal(merged.cookies, true);
  assert.equal(merged.auto_update, true);
});

test("mergeSettings replaces invalid values and clamps numbers", () => {
  const merged = constants.mergeSettings({
    defaults: {quality: "999", codec: "h265", container: "avi"},
    concurrency: 99,
    fragments: "0",
    notifications: "yes",
    selectors: {watchContainer: "   ", watchInsertion: "middle", shortsBar: " #bar "},
  });
  assert.equal(merged.defaults.quality, "best");
  assert.equal(merged.defaults.codec, "any");
  assert.equal(merged.defaults.container, "mp4");
  assert.equal(merged.concurrency, 5);
  assert.equal(merged.fragments, 1);
  assert.equal(merged.notifications, true);
  assert.equal(merged.selectors.watchContainer, "#top-level-buttons-computed");
  assert.equal(merged.selectors.watchInsertion, "first");
  assert.equal(merged.selectors.shortsBar, "#bar");
});

test("mergeSettings output is not tied to the frozen defaults", () => {
  const merged = constants.mergeSettings(undefined);
  merged.defaults.kind = "audio";
  assert.equal(constants.DEFAULT_SETTINGS.defaults.kind, "video");
});

test("every enum list has unique values and labels", () => {
  for (const list of [
    constants.QUALITIES,
    constants.CODECS,
    constants.CONTAINERS,
    constants.AUDIO_FORMATS,
    constants.AUDIO_BITRATES,
  ]) {
    assert.equal(new Set(list.map((item) => item.value)).size, list.length);
    for (const item of list) assert.ok(item.label);
  }
  for (const status of constants.ACTIVE_STATUSES) assert.ok(constants.STATUS_LABELS[status]);
});
