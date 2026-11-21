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
  assert.equal(merged.selectors.watchInsertion, "last");
  assert.equal(merged.selectors.shortsBar, "#bar");
});

test("mergeSettings moves a pre-version-2 'first' placement to 'last'", () => {
  for (const stored of [{selectors: {watchInsertion: "first"}}, {version: 1, selectors: {watchInsertion: "first"}}]) {
    const merged = constants.mergeSettings(stored);
    assert.equal(merged.selectors.watchInsertion, "last");
    assert.equal(merged.version, constants.SETTINGS_VERSION);
  }
});

test("mergeSettings keeps a 'first' placement chosen from version 2 on", () => {
  const merged = constants.mergeSettings({version: 2, selectors: {watchInsertion: "first"}});
  assert.equal(merged.selectors.watchInsertion, "first");
  // Re-merging the saved result must not migrate it again
  assert.equal(constants.mergeSettings(merged).selectors.watchInsertion, "first");
});

test("default placement puts the watch button after YouTube's buttons", () => {
  assert.equal(constants.DEFAULT_SETTINGS.selectors.watchInsertion, "last");
  assert.equal(constants.mergeSettings(undefined).selectors.watchInsertion, "last");
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

test("nativeErrorCode spots a missing native host and nothing else", () => {
  const missing = constants.HELPER_MISSING;
  assert.equal(missing, "helper_missing");
  assert.equal(constants.nativeErrorCode(new Error("No such native application vidgrab")), missing);
  assert.equal(constants.nativeErrorCode(new Error("no SUCH native APPLICATION vidgrab")), missing);
  assert.equal(constants.nativeErrorCode("No such native application vidgrab"), missing);
  assert.equal(constants.nativeErrorCode(new Error("Attempt to postMessage on disconnected port")), null);
  assert.equal(constants.nativeErrorCode(new Error("An unexpected error occurred")), null);
  for (const bad of [undefined, null, {}, 42]) assert.equal(constants.nativeErrorCode(bad), null);
});

test("HELPER_URL is a string (empty until there is a release page)", () => {
  assert.equal(typeof constants.HELPER_URL, "string");
  assert.ok(constants.HELPER_MISSING_ERROR);
  assert.ok(constants.HELPER_MISSING_DETAIL);
});

test("NEEDS_FFMPEG counts a video max size but not an audio one", () => {
  assert.equal(constants.NEEDS_FFMPEG({kind: "video", quality: "360", max_size_mb: 10}), true);
  assert.equal(constants.NEEDS_FFMPEG({kind: "video", quality: "360", max_size_mb: 0}), false);
  assert.equal(constants.NEEDS_FFMPEG({kind: "audio", audio_format: "m4a", max_size_mb: 10}), false);
});

test("mergeSettings defaults max_size_mb to off", () => {
  assert.equal(constants.DEFAULT_SETTINGS.defaults.max_size_mb, 0);
  assert.equal(constants.mergeSettings(undefined).defaults.max_size_mb, 0);
  // Settings saved before the option existed need no migration
  const merged = constants.mergeSettings({version: 2, defaults: {kind: "video", quality: "720"}});
  assert.equal(merged.defaults.max_size_mb, 0);
  assert.equal(merged.version, constants.SETTINGS_VERSION);
});

test("mergeSettings keeps valid max sizes, presets and custom alike", () => {
  for (const mb of [1, 10, 50, 500, 25, 4000]) {
    assert.equal(constants.mergeSettings({defaults: {max_size_mb: mb}}).defaults.max_size_mb, mb);
  }
  assert.equal(constants.mergeSettings({defaults: {max_size_mb: "25"}}).defaults.max_size_mb, 25);
});

test("mergeSettings clamps large max sizes and turns invalid ones off", () => {
  const size = (value) => constants.mergeSettings({defaults: {max_size_mb: value}}).defaults.max_size_mb;
  assert.equal(size(9999), 4000);
  assert.equal(size(12.4), 12);
  for (const bad of [0, -5, "abc", "", null, undefined, true, {}, [], NaN, Infinity]) {
    assert.equal(size(bad), 0, `expected 0 for ${String(bad)}`);
  }
});

test("max size options have unique values and a custom choice", () => {
  const values = constants.MAX_SIZES.map((item) => item.value);
  assert.equal(new Set(values).size, values.length);
  assert.equal(values[0], "0");
  assert.ok(values.includes(constants.MAX_SIZE_CUSTOM));
  for (const item of constants.MAX_SIZES) assert.ok(item.label);
});

test("helperOutdated flags helpers older than MIN_HELPER_VERSION only", () => {
  assert.equal(constants.helperOutdated("2.3.0"), true);
  assert.equal(constants.helperOutdated("2.5.9"), true);
  assert.equal(constants.helperOutdated(constants.MIN_HELPER_VERSION), false);
  assert.equal(constants.helperOutdated("2.6.1"), false);
  assert.equal(constants.helperOutdated("2.10.0"), false);
  assert.equal(constants.helperOutdated("3.0"), false);
  for (const version of ["dev", "", null, undefined, 2]) assert.equal(constants.helperOutdated(version), false);
});

test("parseMaxSize accepts whole MB within the limits only", () => {
  assert.equal(constants.parseMaxSize("25"), 25);
  assert.equal(constants.parseMaxSize(" 4000 "), 4000);
  assert.equal(constants.parseMaxSize(1), 1);
  for (const bad of ["", "0", "4001", "12.5", "-3", "1e3", "abc", null, undefined, 0, 12.5]) {
    assert.equal(constants.parseMaxSize(bad), null, `expected null for ${JSON.stringify(bad)}`);
  }
});

test("maxSizeChoice maps sizes onto presets, off or custom", () => {
  assert.equal(constants.maxSizeChoice(0), "0");
  assert.equal(constants.maxSizeChoice(undefined), "0");
  assert.equal(constants.maxSizeChoice(20), "20");
  assert.equal(constants.maxSizeChoice(50), "50");
  assert.equal(constants.maxSizeChoice(100), "100");
  assert.equal(constants.maxSizeChoice(500), "500");
  assert.equal(constants.maxSizeChoice(25), "25");
  assert.equal(constants.maxSizeChoice(10), "10");
  assert.equal(constants.maxSizeChoice(250), "250");
  assert.equal(constants.maxSizeChoice(1000), "1000");
  assert.equal(constants.maxSizeChoice(64), constants.MAX_SIZE_CUSTOM);
});

test("maxSizeValue resolves the choice and custom field", () => {
  assert.equal(constants.maxSizeValue("0", "99"), 0);
  assert.equal(constants.maxSizeValue("20", ""), 20);
  assert.equal(constants.maxSizeValue("500", "7"), 500);
  assert.equal(constants.maxSizeValue(constants.MAX_SIZE_CUSTOM, "25"), 25);
  assert.equal(constants.maxSizeValue(constants.MAX_SIZE_CUSTOM, ""), null);
  assert.equal(constants.maxSizeValue(constants.MAX_SIZE_CUSTOM, "5000"), null);
  assert.equal(constants.maxSizeValue("junk", ""), 0);
});
