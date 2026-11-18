# YouTube Video Downloader - Firefox Extension

Personal-use Firefox extension that lets you download YouTube videos directly from the video page using `yt-dlp`.

## Features

- 🎬 **Download YouTube videos** directly from the video page
- 📊 **Real-time download progress** tracking
- ⚙️ **Backend API** with validation and concurrency control
- 🔒 **Security-hardened** input validation (prevents command injection)
- 🧪 **Comprehensive test suite** (37 passing tests)

### Coming Soon (Phase 2-4)

- Quality selection dropdown (1080p, 720p, 480p, 360p)
- MP3 audio extraction
- Auto-start backend via native messaging
- Configurable DOM selectors

## Architecture

```
┌─────────────────┐       HTTP        ┌──────────────────┐
│ Firefox         │  ────────────►     │ Python Backend   │
│ Extension       │                    │ (FastAPI)        │
│ • Injects UI    │  ◄────────────     │ • Wraps yt-dlp   │
│ • Manages state │       JSON         │ • Port 18956     │
└─────────────────┘                    └──────────────────┘
```

---

## Prerequisites

### Required Software

1. **Python 3.8+**
   - Download: https://www.python.org/downloads/
   - Verify: `python --version`

2. **yt-dlp** (YouTube downloader)
   - Install: `pip install yt-dlp`
   - Verify: `yt-dlp --version`

3. **ffmpeg** (for MP3 extraction)
   - Download: https://ffmpeg.org/download.html#build-windows
   - Add to PATH
   - Verify: `ffmpeg -version`

4. **Firefox Browser**
   - Download: https://www.mozilla.org/firefox/

---

## Installation

### 1. Backend Setup

```powershell
# Navigate to project directory
cd C:\Users\wobkobi\Desktop\Projects\yt-dl-firefox

# Install Python dependencies
pip install -r backend\requirements.txt

# Start the backend server
python backend\server.py
```

You should see:

```
2026-03-05 14:32:10 INFO [server] ffmpeg found: MP3 extraction available
2026-03-05 14:32:10 INFO [server] Backend started on 127.0.0.1:18956
INFO:     Uvicorn running on http://127.0.0.1:18956 (Press CTRL+C to quit)
```

**Leave this terminal open** — the backend must be running for the extension to work.

### 2. Test Backend (Optional)

Open a new terminal:

```powershell
# Using PowerShell
(Invoke-WebRequest -Uri http://127.0.0.1:18956/health -UseBasicParsing).Content

# Expected output:
# {"status":"ok","version":"1.0","ffmpeg_available":true}
```

### 3. Extension Setup

1. Open Firefox
2. Navigate to `about:debugging`
3. Click **"This Firefox"**
4. Click **"Load Temporary Add-on..."**
5. Navigate to `C:\Users\wobkobi\Desktop\Projects\yt-dl-firefox\extension\`
6. Select `manifest.json`

The extension is now loaded! You should see "YT-DL Firefox Extension" in the list.

### 4. Verify Installation

1. Open a YouTube video (e.g., https://www.youtube.com/watch?v=jNQXAC9IVRw)
2. Open Firefox's Browser Console (`Ctrl+Shift+J`)
3. Look for these logs:
   ```
   [yt-dl-ext] Background script loaded
   [yt-dl-ext] Backend healthy: {status: "ok", version: "1.0", ffmpeg_available: true}
   [yt-dl-ext] Content script loaded
   ```

If you see these logs, **Phase 1 (Walking Skeleton) is complete!** ✅

---

## Current Status: Phase 2 Complete! 🎉

### ✅ Implemented

**Phase 1: Walking Skeleton** ✅

- [x] Backend server running on port 18956
- [x] `/health` endpoint with ffmpeg detection
- [x] Extension loads in Firefox
- [x] Content script runs on YouTube pages
- [x] Background script can communicate with backend

**Phase 2: Core Download Flow (PR1-PR4)** ✅ **COMPLETE**

- [x] URL validation with security hardening (PR1)
- [x] Download state management (PR2)
- [x] POST `/download` endpoint (PR2)
- [x] GET `/progress/{id}` endpoint (PR2)
- [x] yt-dlp subprocess spawning (PR3)
- [x] Progress parsing from yt-dlp output (PR3)
- [x] **Download button injection (PR4)** ✨ NEW
- [x] **Button click → backend integration (PR4)** ✨ NEW
- [x] **Progress polling and UI updates (PR4)** ✨ NEW
- [x] 37 passing tests (validation + API + subprocess)

### 🚧 Next Up

**Phase 3: Quality Selection + Progress**

- [ ] GET `/formats` endpoint
- [ ] Quality dropdown UI
- [ ] Progress polling in extension
- [ ] MP3 format handling

**Phase 4: Auto-Start + Configuration**

- [ ] Native messaging setup
- [ ] Options page for selectors
- [ ] Final documentation

---

## Testing

### Run All Tests

```powershell
cd backend

# Run all unit and integration tests
python -m pytest test_validation.py test_download_api.py test_subprocess.py::TestProgressRegex -v

# Expected: 37 passed in ~0.6s
```

### Test Coverage

- **22 validation tests** — URL/format_id security hardening
- **10 API tests** — Endpoint behavior and concurrency
- **5 subprocess tests** — Progress regex parsing

### Manual API Testing

```powershell
# Start a download (use a short video for testing)
curl -X POST http://127.0.0.1:18956/download `
  -H "Content-Type: application/json" `
  -d '{\"url\": \"https://www.youtube.com/watch?v=jNQXAC9IVRw\", \"format_id\": \"worst\"}'

# Response: {"download_id": "abc123...", "status": "starting"}

# Poll progress
curl http://127.0.0.1:18956/progress/abc123...

# Response: {"download_id": "abc123...", "status": "downloading", "percent": 45.2, ...}
```

---

## Usage

### Download a YouTube Video

1. **Ensure backend is running**:

   ```powershell
   python backend\server.py
   ```

2. **Navigate to any YouTube video** in Firefox

3. **Click the "⬇️ Download" button** next to the Like button

4. **Watch the progress**:
   - Button shows "⏳ Starting..."
   - Then "⏳ X%" with increasing percentage
   - Finally "✅ Complete!" when done

5. **Find your video** in `%USERPROFILE%\Downloads`

### Current Features

- ✅ One-click download from YouTube video pages
- ✅ Real-time progress display
- ✅ Error handling with visual feedback
- ✅ Automatic button injection on page navigation

### Coming in Phase 3

- Quality selection dropdown (1080p, 720p, 480p, 360p)
- MP3 audio extraction
- Visual progress bar
- Download history

---

## Project Structure

```
yt-dl-firefox/
├── backend/
│   ├── server.py              # FastAPI backend (✓ implemented)
│   └── requirements.txt       # Python dependencies (✓ implemented)
├── extension/
│   ├── manifest.json          # Extension config (✓ implemented)
│   ├── background.js          # Background script (✓ implemented)
│   ├── content.js             # Content script (✓ implemented)
│   ├── content.css            # UI styles (stub)
│   └── icons/                 # Extension icons (placeholder)
├── docs/
│   ├── product/               # PRD, Backlog, Tech Lead Brief
│   └── tech/                  # Architecture, Implementation Plan, Handoff
└── README.md                  # This file
```

---

## Development Workflow

### Running the Backend

```powershell
# Start backend
python backend\server.py

# Backend runs on http://127.0.0.1:18956
# Press Ctrl+C to stop
```

### Reloading the Extension

After making changes to extension code:

1. Go to `about:debugging`
2. Find "YT-DL Firefox Extension"
3. Click **"Reload"**

Or use the keyboard shortcut in the extension popup.

### Viewing Logs

- **Backend logs**: Check the terminal where `server.py` is running
- **Extension logs**: Firefox Browser Console (`Ctrl+Shift+J`)
- **Content script logs**: Firefox Developer Tools Console (`F12`) on YouTube page

---

## Troubleshooting

### Backend won't start

**Error**: `ModuleNotFoundError: No module named 'fastapi'`

- **Solution**: Run `pip install -r backend\requirements.txt`

**Error**: `Address already in use` (port 18956)

- **Solution**: Another instance of the backend is running. Find and stop it, or restart your computer.

### Extension not working

**Extension doesn't load**

- Make sure you selected `manifest.json` (not a folder)
- Check `about:debugging` for error messages

**No logs in console**

- Verify the extension is enabled
- Reload the YouTube page
- Check if you're on a video page (`/watch?v=...`)

**Backend unreachable error**

- Verify backend is running: `python backend\server.py`
- Test health endpoint manually (see "Test Backend" above)
- Check Windows Firewall isn't blocking port 18956

### ffmpeg not found

**Warning**: `ffmpeg not found: MP3 downloads will fail`

- **Solution**: Install ffmpeg and add it to your system PATH
- Verify with: `ffmpeg -version`
- Restart the backend after installing ffmpeg

---

## Security Notes

- Backend binds to `127.0.0.1` **only** (localhost) — no external access
- Extension permissions limited to YouTube and localhost
- All URLs validated before passing to yt-dlp
- No shell injection vulnerabilities (subprocess uses argument lists)

---

## Contributing

This is a personal-use project. If you want to extend it:

1. Read the architecture docs in `docs/tech/`
2. Follow the Implementation Plan for phased development
3. Run manual tests after each change (see `docs/tech/HANDOFF.md`)

---

## License

Personal use only. Uses `yt-dlp` under the Unlicense.

---

## Next Steps for Development

Continue with **Phase 2** implementation:

1. Read `docs/tech/IMPLEMENTATION_PLAN.md` — Step 2.1 onwards
2. Implement URL validation module
3. Create downloader module
4. Inject Download button into YouTube page

See `docs/tech/HANDOFF.md` for detailed task assignments.

---

**Questions?** Check the docs in `docs/tech/` or review the Implementation Plan.
