"""
Download manager for yt-dlp subprocess coordination
Handles download state, concurrency control, and subprocess lifecycle
"""

import os
import re
import subprocess
import threading
import uuid
import logging
from typing import Optional

logger = logging.getLogger("downloader")

# Regex to parse yt-dlp progress output
# Format: "[download]  45.2% of 123.45MiB at 2.50MiB/s ETA 00:30"
PROGRESS_REGEX = re.compile(
    r"\[download\]\s+(\d+\.?\d*)%(?:\s+of\s+~?(\d+\.?\d+)(\w+))?(?:\s+at\s+(\d+\.?\d+\w+/s))?(?:\s+ETA\s+(\d+:\d+))?"
)


class DownloadManager:
    """
    Manages download state and coordinates yt-dlp subprocess execution.

    Enforces single concurrent download constraint.
    Tracks download progress and status.
    """

    def __init__(self):
        """Initialize download manager with no active download"""
        self.current_download: Optional[dict] = None
        logger.info("DownloadManager initialized")

    def is_busy(self) -> bool:
        """
        Check if a download is currently in progress.

        Returns:
            True if download is active, False otherwise
        """
        return self.current_download is not None and self.current_download[
            "status"
        ] in ["starting", "downloading"]

    def start_download(self, url: str, format_id: str) -> str:
        """
        Initialize a new download.

        Args:
            url: YouTube video URL (already validated)
            format_id: Format to download (already validated)

        Returns:
            download_id: UUID for this download

        Raises:
            Exception: If another download is in progress
        """
        if self.is_busy():
            raise Exception("Download already in progress")

        download_id = str(uuid.uuid4())

        # Initialize download state
        self.current_download = {
            "download_id": download_id,
            "url": url,
            "format_id": format_id,
            "status": "starting",
            "percent": 0.0,
            "speed": None,
            "eta": None,
            "filename": None,
            "filepath": None,
            "downloaded_bytes": 0,
            "total_bytes": 0,
            "error": None,
            "process": None,
        }

        logger.info(f"Download initialized: id={download_id}, format={format_id}")

        # Start subprocess in background thread
        thread = threading.Thread(target=self._run_download, daemon=True)
        thread.start()

        return download_id

    def get_progress(self, download_id: str) -> dict:
        """
        Get progress information for a download.

        Args:
            download_id: UUID of the download

        Returns:
            Progress dict with status, percent, speed, eta, etc.

        Raises:
            Exception: If download_id not found
        """
        if self.current_download is None:
            raise Exception("No download in progress")

        if self.current_download["download_id"] != download_id:
            raise Exception("Download not found")

        # Return copy to prevent external modification
        return {
            "download_id": self.current_download["download_id"],
            "status": self.current_download["status"],
            "percent": self.current_download["percent"],
            "speed": self.current_download["speed"],
            "eta": self.current_download["eta"],
            "filename": self.current_download["filename"],
            "filepath": self.current_download["filepath"],
            "downloaded_bytes": self.current_download["downloaded_bytes"],
            "total_bytes": self.current_download["total_bytes"],
            "error": self.current_download["error"],
        }

    def _run_download(self):
        """
        Run yt-dlp subprocess in background thread.
        Updates download state as progress is reported.

        This method runs in a separate thread spawned by start_download().
        """
        if self.current_download is None:
            logger.error("_run_download called with no active download")
            return

        download_id = self.current_download["download_id"]
        url = self.current_download["url"]
        format_id = self.current_download["format_id"]

        # Resolve Downloads folder
        downloads_path = os.path.join(os.path.expanduser("~"), "Downloads")

        # Build yt-dlp command (safe - no shell=True)
        # Use argument list to prevent command injection
        # Use 'python -m yt_dlp' instead of 'yt-dlp' for better compatibility
        import sys

        cmd = [
            sys.executable,  # Current Python interpreter
            "-m",
            "yt_dlp",
            "-f",
            format_id,
            "-o",
            os.path.join(downloads_path, "%(title)s [%(id)s].%(ext)s"),
            "--newline",  # Progress on separate lines for easier parsing
            "--no-playlist",  # Only download single video
            url,
        ]

        logger.info(f"Starting yt-dlp subprocess for download_id={download_id}")

        try:
            # Spawn subprocess without shell (security)
            process = subprocess.Popen(
                cmd,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,  # Merge stderr into stdout
                text=True,
                bufsize=1,  # Line buffered
            )

            # Check if download was cancelled while we were starting
            if self.current_download is None:
                process.kill()
                logger.warning(f"Download {download_id} was cancelled during startup")
                return

            self.current_download["process"] = process
            self.current_download["status"] = "downloading"

            # Read output line by line and parse progress
            for line in iter(process.stdout.readline, ""):
                if not line:
                    break

                line = line.strip()

                # Parse progress information
                match = PROGRESS_REGEX.search(line)
                if match:
                    percent = float(match.group(1))
                    self.current_download["percent"] = percent

                    # Optional fields (may not always be present)
                    if match.group(2):  # Total size
                        size_value = match.group(2)
                        size_unit = match.group(3)
                        # Store as string for simplicity
                        self.current_download["total_bytes"] = (
                            f"{size_value}{size_unit}"
                        )

                    if match.group(4):  # Speed
                        self.current_download["speed"] = match.group(4)

                    if match.group(5):  # ETA
                        self.current_download["eta"] = match.group(5)

                # Check for destination filename
                if "[download] Destination:" in line:
                    # Extract filename from line like "[download] Destination: filename.mp4"
                    parts = line.split("Destination:", 1)
                    if len(parts) == 2:
                        filepath = parts[1].strip()
                        self.current_download["filepath"] = filepath
                        self.current_download["filename"] = os.path.basename(filepath)

                # Log at debug level to avoid spam
                logger.debug(f"yt-dlp: {line}")

            # Wait for process to complete
            process.stdout.close()
            exit_code = process.wait()

            # Update final status based on exit code
            if exit_code == 0:
                self.current_download["status"] = "complete"
                self.current_download["percent"] = 100.0
                logger.info(
                    f"Download complete: {download_id}, file={self.current_download.get('filename', 'unknown')}"
                )
            else:
                self.current_download["status"] = "error"
                self.current_download["error"] = f"yt-dlp exited with code {exit_code}"
                logger.error(f"Download failed: {download_id}, exit_code={exit_code}")

        except FileNotFoundError:
            if self.current_download is not None:
                self.current_download["status"] = "error"
                self.current_download["error"] = (
                    "yt-dlp module not found. Please install: pip install yt-dlp"
                )
            logger.error(f"yt-dlp module not found for download_id={download_id}")

        except Exception as e:
            if self.current_download is not None:
                self.current_download["status"] = "error"
                self.current_download["error"] = str(e)
            logger.exception(f"Download exception for {download_id}")


# Global singleton instance
download_manager = DownloadManager()
