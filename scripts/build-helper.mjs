// Cross-compile the VidGrab helper for every supported OS and CPU into dist/.
// Usage: node scripts/build-helper.mjs [--only windows-amd64,darwin-arm64]
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
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

/**
 * Find a windres that targets 64-bit Windows: llvm-mingw and Ubuntu's binutils-mingw-w64-x86-64
 * both ship the prefixed name, and a plain windres covers MSYS2.
 * @returns {string} The command to run.
 */
function findWindres() {
  for (const cmd of ["x86_64-w64-mingw32-windres", "windres"]) {
    try {
      execFileSync(cmd, ["--version"], { stdio: "ignore" });
      return cmd;
    } catch {
      // Not on PATH, try the next name
    }
  }
  throw new Error(
    "windres not found. The Windows helper needs it to embed its icon and version details: install " +
      "binutils-mingw-w64-x86-64 (Ubuntu) or llvm-mingw (Windows), or skip it with --only.",
  );
}

/**
 * Compile the icon and version details into a .syso next to main.go, which go build links into
 * the Windows binary on its own. Explorer shows them in the file's Properties, and a named,
 * versioned exe with an icon trips fewer antivirus heuristics than a bare one.
 * @param {string} name The exe's file name, shown as its original filename.
 * @returns {string} Path to the .syso, to delete once the build is done.
 */
function writeWindowsResources(name) {
  const [major, minor, patch] = version.split(".").map(Number);
  const numeric = `${major},${minor},${patch},0`;
  // windres reads the icon path from inside the .rc, where backslashes are escapes
  const icon = join(helper, "winres", "icon.ico").replaceAll("\\", "/");
  const rc = `1 ICON "${icon}"
1 VERSIONINFO
FILEVERSION ${numeric}
PRODUCTVERSION ${numeric}
FILEOS 0x40004
FILETYPE 0x1
BEGIN
  BLOCK "StringFileInfo"
  BEGIN
    BLOCK "040904B0"
    BEGIN
      VALUE "CompanyName", "VidGrab"
      VALUE "FileDescription", "VidGrab helper"
      VALUE "FileVersion", "${version}"
      VALUE "InternalName", "vidgrab"
      VALUE "LegalCopyright", "MIT License"
      VALUE "OriginalFilename", "${name}"
      VALUE "ProductName", "VidGrab"
      VALUE "ProductVersion", "${version}"
    END
  END
  BLOCK "VarFileInfo"
  BEGIN
    VALUE "Translation", 0x409, 1200
  END
END
`;
  const temp = mkdtempSync(join(tmpdir(), "vidgrab-rc-"));
  const syso = join(helper, "rsrc_windows_amd64.syso");
  try {
    const rcPath = join(temp, "vidgrab.rc");
    writeFileSync(rcPath, rc);
    execFileSync(findWindres(), ["--target=pe-x86-64", "-O", "coff", "-i", rcPath, "-o", syso], {
      stdio: "inherit",
    });
  } finally {
    rmSync(temp, { recursive: true, force: true });
  }
  return syso;
}

const onlyIndex = process.argv.indexOf("--only");
const only = onlyIndex > 0 ? new Set(process.argv[onlyIndex + 1].split(",")) : null;

mkdirSync(dist, { recursive: true });
for (const t of TARGETS) {
  if (only && !only.has(`${t.goos}-${t.goarch}`)) continue;
  const out = join(dist, t.name);
  const syso = t.goos === "windows" ? writeWindowsResources(t.name) : null;
  try {
    // CGO off keeps every binary static, so it runs without a C runtime on the target
    execFileSync("go", ["build", "-trimpath", "-ldflags", `-s -w -X main.version=${version}`, "-o", out, "."], {
      cwd: helper,
      stdio: "inherit",
      env: { ...process.env, GOOS: t.goos, GOARCH: t.goarch, CGO_ENABLED: "0" },
    });
  } finally {
    if (syso) rmSync(syso, { force: true });
  }
  console.log(`${t.name}  ${(statSync(out).size / 1048576).toFixed(1)} MB`);
}
