// Package the add-on as an unsigned dist/vidgrab-<version>.<build>.xpi for local installs.
// Usage: node scripts/build-xpi.mjs
//
// Regular Firefox only installs signed add-ons. This file installs in Firefox Developer Edition,
// Nightly or ESR once xpinstall.signatures.required is false in about:config.
//
// Each build adds a build number as a fourth version part (2.6.1.1, 2.6.1.2, ...), so Firefox
// shows which build is installed and treats every new one as an update. The count lives in
// dist/xpi-build.json and starts again at 1 when the manifest version changes.
import { execSync } from "node:child_process";
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const dist = join(root, "dist");
const counterFile = join(dist, "xpi-build.json");
const manifest = JSON.parse(readFileSync(join(root, "extension", "manifest.json"), "utf8"));

/**
 * Read the last build number for a version.
 * @param {string} version - Manifest version.
 * @returns {number} Last build number, 0 when this version has none yet.
 */
function lastBuild(version) {
  if (!existsSync(counterFile)) return 0;
  try {
    const saved = JSON.parse(readFileSync(counterFile, "utf8"));
    return saved.version === version && Number.isInteger(saved.build) ? saved.build : 0;
  } catch {
    return 0;
  }
}

const build = lastBuild(manifest.version) + 1;
const version = `${manifest.version}.${build}`;
const name = `vidgrab-${version}.xpi`;

// Build from a copy, so the stamped version never touches extension/manifest.json
const staging = mkdtempSync(join(tmpdir(), "vidgrab-xpi-"));
try {
  cpSync(join(root, "extension"), staging, { recursive: true });
  writeFileSync(join(staging, "manifest.json"), JSON.stringify({ ...manifest, version }, null, 2));
  mkdirSync(dist, { recursive: true });
  // The same web-ext version the release workflow uses. One command string, since npx is a .cmd
  // script on Windows and needs a shell; every part of it is fixed or comes from the manifest.
  execSync(`npx --yes web-ext@10.8.0 build -s "${staging}" -a "${dist}" -n "${name}" --overwrite-dest`, {
    cwd: root,
    stdio: "inherit",
  });
} finally {
  rmSync(staging, { recursive: true, force: true });
}
writeFileSync(counterFile, JSON.stringify({ version: manifest.version, build }, null, 2));
console.log(`Built ${join(dist, name)}`);
