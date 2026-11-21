import assert from "node:assert/strict";
import test from "node:test";
import { byArea, changelogSection, compareVersions, releaseNotes } from "./release-notes.mjs";

const CHANGELOG = `# Changelog

Intro text.

## [2.1.0] - 2026-12-02

### Added

- **Popup:** Shows the channel
  under each title.

### Fixed

- **Download button:** Keeps its width.
- A bullet with no area.

## [2.0.1] - 2026-12-01

### Fixed

- **Popup:** Scrolls a long list.

## [2.0.0] - 2026-11-30

### Added

- **Helper:** First release.
`;

test("changelogSection joins wrapped bullets and stops at the next version", () => {
  assert.equal(
    changelogSection(CHANGELOG, "2.1.0"),
    "### Added\n\n- **Popup:** Shows the channel under each title.\n\n### Fixed\n\n- **Download button:** Keeps its width.\n- A bullet with no area.",
  );
  assert.equal(changelogSection(CHANGELOG, "9.9.9"), null);
});

test("byArea groups by area in the set order with General last", () => {
  assert.equal(
    byArea(changelogSection(CHANGELOG, "2.1.0")),
    "### Download button\n\n- Fixed: Keeps its width.\n\n### Popup\n\n- New: Shows the channel under each title.\n\n### General\n\n- Fixed: A bullet with no area.",
  );
});

test("byArea leaves notes without areas alone", () => {
  assert.equal(byArea("### Fixed\n\n- Plain bullet."), "### Fixed\n\n- Plain bullet.");
});

test("releaseNotes merges every version since the last release", () => {
  const notes = releaseNotes(CHANGELOG, "2.1.0", "2.0.0");
  assert.match(notes, /### Popup\n\n- New: Shows the channel under each title\.\n- Fixed: Scrolls a long list\./);
  assert.doesNotMatch(notes, /First release/);
  assert.equal(releaseNotes(CHANGELOG, "2.1.0", "2.0.1"), byArea(changelogSection(CHANGELOG, "2.1.0")));
  assert.equal(releaseNotes(CHANGELOG, "2.1.0", null), byArea(changelogSection(CHANGELOG, "2.1.0")));
  assert.equal(releaseNotes(CHANGELOG, "9.9.9", "2.0.0"), null);
});

test("compareVersions compares by number, padding missing parts", () => {
  assert.ok(compareVersions("2.10.0", "2.9.9") > 0);
  assert.equal(compareVersions("2.6", "2.6.0"), 0);
  assert.ok(compareVersions("2.6.1.2", "2.6.1") > 0);
});
