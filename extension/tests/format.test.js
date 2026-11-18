const test = require("node:test");
const assert = require("node:assert/strict");

require("../shared/constants.js");
const format = require("../shared/format.js");

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
