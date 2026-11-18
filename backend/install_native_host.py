"""Register the native messaging host with Firefox for the current user (--uninstall removes it)."""

from __future__ import annotations

import argparse
import importlib.util
import json
import stat
import sys
from pathlib import Path

BACKEND_DIR = Path(__file__).resolve().parent
PROJECT_DIR = BACKEND_DIR.parent
HOST_NAME = "yt_dl_backend"
EXTENSION_ID = "yt-dl-firefox@local"
MANIFEST_PATH = PROJECT_DIR / "native-messaging-host" / f"{HOST_NAME}.json"
BAT_PATH = BACKEND_DIR / "native_messaging.bat"
SH_PATH = BACKEND_DIR / "data" / "native_messaging.sh"
REGISTRY_KEY = rf"Software\Mozilla\NativeMessagingHosts\{HOST_NAME}"
REQUIRED_MODULES = ("fastapi", "uvicorn", "yt_dlp", "pydantic")


def posix_manifest_dir() -> Path:
    """Return the per-user directory Firefox scans for native host manifests on macOS/Linux."""
    if sys.platform == "darwin":
        return Path.home() / "Library" / "Application Support" / "Mozilla" / "NativeMessagingHosts"
    return Path.home() / ".mozilla" / "native-messaging-hosts"


def write_launcher(python: str) -> Path:
    """
    Write the launcher script that runs native_messaging.py with a fixed interpreter.

    Pinning the interpreter matters because Firefox starts the host with its own PATH, which
    may resolve "python" to an install without the dependencies.

    Args:
        python: Absolute path of the interpreter to use.

    Returns:
        Path of the launcher.
    """
    host_script = BACKEND_DIR / "native_messaging.py"
    if sys.platform == "win32":
        content = (
            "@echo off\r\n"
            "rem Launcher for the Firefox native messaging host, written by install_native_host.py.\r\n"
            f'"{python}" "{host_script}" %*\r\n'
        )
        BAT_PATH.write_bytes(content.encode("utf-8"))
        return BAT_PATH
    SH_PATH.parent.mkdir(parents=True, exist_ok=True)
    SH_PATH.write_text(f'#!/bin/sh\nexec "{python}" "{host_script}" "$@"\n', encoding="utf-8")
    SH_PATH.chmod(SH_PATH.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
    return SH_PATH


def build_manifest(launcher: Path) -> dict[str, object]:
    """
    Build the native host manifest Firefox reads.

    Args:
        launcher: Absolute path of the launcher script.

    Returns:
        The manifest as a dict.
    """
    return {
        "name": HOST_NAME,
        "description": "Starts the yt-dl-firefox download backend",
        "path": str(launcher),
        "type": "stdio",
        "allowed_extensions": [EXTENSION_ID],
    }


def manifest_path() -> Path:
    """Return where the manifest is written: the project folder on Windows, Firefox's folder elsewhere."""
    if sys.platform == "win32":
        return MANIFEST_PATH
    return posix_manifest_dir() / f"{HOST_NAME}.json"


def install() -> None:
    """Write the launcher and manifest and register the manifest with Firefox."""
    missing = [m for m in REQUIRED_MODULES if importlib.util.find_spec(m) is None]
    if missing:
        print(f"Warning: {', '.join(missing)} not installed for {sys.executable}.")
        print(f'Run: "{sys.executable}" -m pip install -r "{BACKEND_DIR / "requirements.txt"}"')

    launcher = write_launcher(sys.executable)
    path = manifest_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(build_manifest(launcher), indent=2) + "\n", encoding="utf-8")

    if sys.platform == "win32":
        import winreg

        with winreg.CreateKey(winreg.HKEY_CURRENT_USER, REGISTRY_KEY) as key:
            winreg.SetValueEx(key, "", 0, winreg.REG_SZ, str(path))
        print(rf"Registered HKCU\{REGISTRY_KEY}")
    print(f"Launcher: {launcher}")
    print(f"Manifest: {path}")
    print("Restart Firefox (or reload the add-on) so it picks up the native host.")


def uninstall() -> None:
    """Remove the registry entry, manifest and generated launcher."""
    if sys.platform == "win32":
        import winreg

        try:
            winreg.DeleteKey(winreg.HKEY_CURRENT_USER, REGISTRY_KEY)
            print(rf"Removed HKCU\{REGISTRY_KEY}")
        except FileNotFoundError:
            print("Registry entry was not present")
    path = manifest_path()
    if path.exists():
        path.unlink()
        print(f"Removed {path}")
    if sys.platform != "win32" and SH_PATH.exists():
        SH_PATH.unlink()


def main(argv: list[str] | None = None) -> int:
    """
    Parse arguments and install or uninstall.

    Args:
        argv: Command-line arguments (defaults to sys.argv[1:]).

    Returns:
        The process exit code.
    """
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--uninstall", action="store_true", help="remove the native host registration")
    args = parser.parse_args(argv)
    try:
        if args.uninstall:
            uninstall()
        else:
            install()
    except OSError as exc:
        print(f"Failed: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
