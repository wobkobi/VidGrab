#!/usr/bin/env python3
"""
YouTube Downloader Backend Server
Provides REST API for the Firefox extension to download videos via yt-dlp
"""
import logging
import shutil
from fastapi import FastAPI, HTTPException
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse
from pydantic import BaseModel
import uvicorn

from validation import validate_youtube_url, validate_format_id
from downloader import download_manager

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s [%(name)s] %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
logger = logging.getLogger("server")

# Check for ffmpeg on startup
ffmpeg_available = shutil.which("ffmpeg") is not None
if ffmpeg_available:
    logger.info("ffmpeg found: MP3 extraction available")
else:
    logger.warning("ffmpeg not found: MP3 downloads will fail")

# Initialize FastAPI app
app = FastAPI(
    title="YT-DL Backend",
    version="1.0",
    description="Local backend for YouTube video downloads",
)

# Add CORS middleware for localhost only
app.add_middleware(
    CORSMiddleware,
    allow_origins=[
        "http://127.0.0.1",
        "http://localhost",
        "moz-extension://*",  # Firefox extension origin
    ],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)


@app.get("/health")
def health():
    """Health check endpoint"""
    return {"status": "ok", "version": "1.0", "ffmpeg_available": ffmpeg_available}


# Request/Response models
class DownloadRequest(BaseModel):
    """Request body for POST /download"""

    url: str
    format_id: str


@app.post("/download")
def start_download(req: DownloadRequest):
    """
    Start a new download.

    Returns:
        202: Download started successfully
        400: Invalid URL or format_id
        409: Another download already in progress
    """
    # Validate inputs (raises HTTPException if invalid)
    video_id = validate_youtube_url(req.url)
    validate_format_id(req.format_id)

    # Check if busy
    if download_manager.is_busy():
        # Type guard for Pylance
        assert download_manager.current_download is not None
        current_id = download_manager.current_download["download_id"]
        return JSONResponse(
            status_code=409,
            content={
                "error": "Download already in progress",
                "current_download_id": current_id,
            },
        )

    try:
        download_id = download_manager.start_download(req.url, req.format_id)
        logger.info(f"Download started: video_id={video_id}, download_id={download_id}")
        return JSONResponse(
            status_code=202, content={"download_id": download_id, "status": "starting"}
        )
    except Exception as e:
        logger.error(f"Failed to start download: {e}")
        raise HTTPException(status_code=500, detail=str(e))


@app.get("/progress/{download_id}")
def get_progress(download_id: str):
    """
    Get progress for an ongoing or completed download.

    Returns:
        200: Progress information
        404: Download not found
    """
    try:
        progress = download_manager.get_progress(download_id)
        return progress
    except Exception as e:
        raise HTTPException(status_code=404, detail=str(e))


if __name__ == "__main__":
    logger.info("Backend started on 127.0.0.1:18956")
    uvicorn.run(
        app,
        host="127.0.0.1",  # Localhost only for security
        port=18956,
        log_level="info",
    )
