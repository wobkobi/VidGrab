// Release notes from CHANGELOG.md: the sections since the last release, regrouped by area of the
// add-on with each bullet labelled New, Changed or Fixed.
// Usage: node scripts/release-notes.mjs <version> [--since <last released version>] [--out file]
// Exits 1 when the changelog has no section for <version>, so a release can't go out undescribed.
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

/** Keep a Changelog's heading order, used when sections from several versions are merged. */
const HEADING_ORDER = ["Added", "Changed", "Deprecated", "Removed", "Fixed", "Security"];

/** What each changelog heading is called beside a bullet once the notes are grouped by area. */
const TYPE_LABEL = {
  Added: "New",
  Changed: "Changed",
  Deprecated: "Deprecated",
  Removed: "Removed",
  Fixed: "Fixed",
  Security: "Security",
};

/**
 * Areas of the add-on in the order release notes list them. Areas not listed here follow in the
 * order they first appear, and bullets without one go under General, last.
 */
const AREA_ORDER = ["Download button", "Three-dot menu", "Popup", "Notifications", "Settings", "Helper", "Installing"];

/**
 * Compare two dotted versions by number.
 * @param {string} a - First version.
 * @param {string} b - Second version.
 * @returns {number} Negative, zero or positive, as a is older than, equal to or newer than b.
 */
export function compareVersions(a, b) {
  const [x, y] = [a, b].map((v) => v.split(".").map(Number));
  for (let i = 0; i < Math.max(x.length, y.length); i++) {
    if ((x[i] ?? 0) !== (y[i] ?? 0)) return (x[i] ?? 0) - (y[i] ?? 0);
  }
  return 0;
}

/**
 * A version's section of the changelog, without its "## [x.y.z]" heading. Bullets wrapped over
 * several lines are joined into one line each.
 * @param {string} changelog - CHANGELOG.md text.
 * @param {string} version - Version to find.
 * @returns {string|null} The notes, or null when the changelog has no section for that version.
 */
export function changelogSection(changelog, version) {
  const lines = changelog.split(/\r?\n/);
  const start = lines.findIndex((line) => line.startsWith(`## [${version}]`));
  if (start === -1) return null;
  const rest = lines.slice(start + 1);
  const end = rest.findIndex((line) => line.startsWith("## "));
  const body = end === -1 ? rest : rest.slice(0, end);

  const out = [];
  for (const line of body) {
    // An indented line under a bullet continues it
    if (/^\s+\S/.test(line) && out.length > 0 && /^[-*]\s/.test(out[out.length - 1])) {
      out[out.length - 1] += ` ${line.trim()}`;
    } else {
      out.push(line.trimEnd());
    }
  }
  const notes = out.join("\n").trim();
  return notes || null;
}

/**
 * Regroup changelog notes by area, each bullet labelled with its kind ("- New: ...",
 * "- Fixed: ..."), so a release reads as what changed where. Notes with no area-tagged bullet
 * come back unchanged.
 * @param {string} notes - "### Added" style groups of single-line bullets.
 * @returns {string} The notes grouped under "### Popup" style headings.
 */
export function byArea(notes) {
  const lines = notes.split("\n");
  const tagged = /^- \*\*([^*]+?):\*\*\s+(.*)$/;
  if (!lines.some((line) => tagged.test(line))) return notes;

  const areas = new Map();
  let kind = "";
  for (const line of lines) {
    if (line.startsWith("### ")) {
      kind = line.slice(4).trim();
      continue;
    }
    if (!line.trim()) continue;
    const match = tagged.exec(line);
    const area = match ? match[1].trim() : "General";
    const text = match ? match[2] : line.replace(/^[-*]\s+/, "");
    const label = TYPE_LABEL[kind] ?? kind;
    areas.set(area, [...(areas.get(area) ?? []), label ? `- ${label}: ${text}` : `- ${text}`]);
  }
  // Listed areas in AREA_ORDER, then others in first-seen order (sort is stable), then General
  const rank = (area) => {
    if (area === "General") return AREA_ORDER.length + 1;
    const i = AREA_ORDER.indexOf(area);
    return i === -1 ? AREA_ORDER.length : i;
  };
  return [...areas]
    .sort(([a], [b]) => rank(a) - rank(b))
    .map(([area, bullets]) => `### ${area}\n\n${bullets.join("\n")}`)
    .join("\n\n");
}

/**
 * Notes for a release: every changelog section after the last release up to this version, so
 * changes from a version that never got released (such as one AMO rejected) still get described.
 * @param {string} changelog - CHANGELOG.md text.
 * @param {string} version - Version being released.
 * @param {string|null} since - Last released version, or null for this version's section alone.
 * @returns {string|null} The notes grouped by area, or null when this version has no section.
 */
export function releaseNotes(changelog, version, since) {
  const own = changelogSection(changelog, version);
  if (!own) return null;
  if (!since) return byArea(own);
  const versions = [...changelog.matchAll(/^## \[(\d+(?:\.\d+)*)\]/gm)]
    .map((match) => match[1])
    .filter((v) => compareVersions(v, since) > 0 && compareVersions(v, version) <= 0);
  if (versions.length <= 1) return byArea(own);

  // Merge the sections under one heading each, newest version's bullets first
  const groups = new Map();
  for (const v of versions) {
    let heading = "";
    for (const line of (changelogSection(changelog, v) ?? "").split("\n")) {
      if (line.startsWith("### ")) heading = line;
      else if (line.trim()) groups.set(heading, [...(groups.get(heading) ?? []), line]);
    }
  }
  const rank = (heading) => {
    const i = HEADING_ORDER.indexOf(heading.slice(4));
    return i === -1 ? HEADING_ORDER.length : i;
  };
  const merged = [...groups]
    .sort(([a], [b]) => rank(a) - rank(b))
    .map(([heading, lines]) => (heading ? `${heading}\n\n${lines.join("\n")}` : lines.join("\n")))
    .join("\n\n");
  return byArea(merged);
}

/**
 * Read the value after a command-line flag.
 * @param {string} flag - Flag such as "--since".
 * @returns {string|null} Its value, or null when the flag is absent or has none.
 */
function flagValue(flag) {
  const i = process.argv.indexOf(flag);
  return i > 0 && process.argv[i + 1] ? process.argv[i + 1] : null;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const version = process.argv[2];
  if (!version || version.startsWith("--")) {
    console.error("Usage: node scripts/release-notes.mjs <version> [--since <version>] [--out file]");
    process.exit(2);
  }
  const root = join(dirname(fileURLToPath(import.meta.url)), "..");
  const notes = releaseNotes(readFileSync(join(root, "CHANGELOG.md"), "utf8"), version, flagValue("--since"));
  if (!notes) {
    console.error(`CHANGELOG.md has no section for ${version}. Add one before releasing.`);
    process.exit(1);
  }
  const out = flagValue("--out");
  if (out) writeFileSync(out, `${notes}\n`);
  else console.log(notes);
}
