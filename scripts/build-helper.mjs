// Cross-compile the VidGrab helper for every supported OS and CPU into dist/.
// Usage: node scripts/build-helper.mjs [--only windows-amd64,darwin-arm64]
import { execFileSync } from "node:child_process";
import { mkdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const helper = join(root, "helper");
const dist = join(root, "dist");

// The helper reports the add-on's version so /health and the popup agree
const version = JSON.parse(readFileSync(join(root, "extension", "manifest.json"), "utf8")).version;

// Windows on ARM runs the amd64 build under its x64 emulation, and ARM Linux desktops are rare,
// so neither gets a build of its own
const TARGETS = [
  { goos: "windows", goarch: "amd64", name: "vidgrab-windows-amd64.exe" },
  { goos: "darwin", goarch: "amd64", name: "vidgrab-macos-amd64" },
  { goos: "darwin", goarch: "arm64", name: "vidgrab-macos-arm64" },
  { goos: "linux", goarch: "amd64", name: "vidgrab-linux-amd64" },
];

const onlyIndex = process.argv.indexOf("--only");
const only = onlyIndex > 0 ? new Set(process.argv[onlyIndex + 1].split(",")) : null;

mkdirSync(dist, { recursive: true });
for (const t of TARGETS) {
  if (only && !only.has(`${t.goos}-${t.goarch}`)) continue;
  const out = join(dist, t.name);
  // CGO off keeps every binary static, so it runs without a C runtime on the target
  execFileSync("go", ["build", "-trimpath", "-ldflags", `-s -w -X main.version=${version}`, "-o", out, "."], {
    cwd: helper,
    stdio: "inherit",
    env: { ...process.env, GOOS: t.goos, GOARCH: t.goarch, CGO_ENABLED: "0" },
  });
  console.log(`${t.name}  ${(statSync(out).size / 1048576).toFixed(1)} MB`);
}
