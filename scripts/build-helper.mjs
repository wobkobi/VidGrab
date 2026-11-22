// Build the VidGrab helper for Windows into dist/, with its icon and version details.
// Usage: node scripts/build-helper.mjs
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

// Windows on ARM runs this amd64 build under its x64 emulation, so it gets no build of its own
const NAME = "VidGrab-Setup.exe";

/**
 * Find a windres that targets 64-bit Windows: llvm-mingw ships the prefixed name, and a plain
 * windres covers MSYS2 and the mingw-w64 on GitHub's Windows runners.
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
    "windres not found. The helper needs it to embed its icon and version details: install llvm-mingw " +
      "or MSYS2's mingw-w64-x86_64-binutils and put its bin folder on PATH.",
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

mkdirSync(dist, { recursive: true });
const out = join(dist, NAME);
const syso = writeWindowsResources(NAME);
try {
  // CGO off keeps the binary static, so it runs without a C runtime
  execFileSync("go", ["build", "-trimpath", "-ldflags", `-s -w -X main.version=${version}`, "-o", out, "."], {
    cwd: helper,
    stdio: "inherit",
    env: { ...process.env, GOOS: "windows", GOARCH: "amd64", CGO_ENABLED: "0" },
  });
} finally {
  rmSync(syso, { force: true });
}
console.log(`${NAME}  ${(statSync(out).size / 1048576).toFixed(1)} MB`);
