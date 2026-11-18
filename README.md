# YT-DL Firefox

A Firefox add-on that downloads YouTube videos, Shorts, playlists and channels through a local [yt-dlp](https://github.com/yt-dlp/yt-dlp) backend. Version 2.0.0.

## What it does

- Adds a download button to watch pages, Shorts, playlist headers and channel headers.
- Downloads video (quality, codec preference, MP4 or MKV) or audio only (MP3 or M4A).
- Runs a queue with parallel downloads, pause, resume, cancel and retry, shown in the toolbar popup.
- Shows a desktop notification when a download finishes or fails.
- Can use your Firefox cookies for age-restricted, members-only or private videos.
- Keeps yt-dlp up to date and starts the backend automatically when Firefox needs it.

Files are saved to your Downloads folder as `Title [id].ext`. Playlist and channel items go into `Downloads\<playlist title>\` or `Downloads\<channel name>\`.

## Requirements

- Windows 10 or 11 (macOS and Linux work too, with the equivalent commands).
- Firefox 140 or newer.
- Python 3.10 or newer.
- [ffmpeg](https://ffmpeg.org/download.html) on `PATH`. Without it only 720p and lower video and M4A audio work; "Best", anything above 720p and MP3 need ffmpeg to merge or convert. Check with `ffmpeg -version`.

## Setup

1. Create a virtual environment in the project folder and install the Python dependencies into it:

   ```bat
   py -3.12 -m venv .venv
   .venv\Scripts\python -m pip install -r backend\requirements.txt
   ```

   Any Python 3.10+ works in place of `py -3.12`. Keeping the packages in `.venv` means the editor, the terminal and the auto-started backend all use the same interpreter. `.vscode\settings.json` already points VS Code at it.

2. Register the native messaging host so Firefox can start the backend on demand. Run it with the venv's Python, because the backend will run with whichever Python ran this:

   ```bat
   .venv\Scripts\python backend\install_native_host.py
   ```

   This writes `native-messaging-host\yt_dl_backend.json`, points `backend\native_messaging.bat` at the Python you ran it with, and adds the registry key `HKCU\Software\Mozilla\NativeMessagingHosts\yt_dl_backend`. Run it again if you move the folder or change Python. Remove it with `--uninstall`.

3. Load the add-on: open `about:debugging#/runtime/this-firefox`, click **Load Temporary Add-on...** and pick `extension\manifest.json`.

### Permanent install

Temporary add-ons are removed when Firefox restarts. To keep it installed, either:

- Use Firefox Developer Edition or Nightly, set `xpinstall.signatures.required` to `false` in `about:config`, then zip the contents of `extension\` (or run `npx web-ext build -s extension`) and install the `.zip` from `about:addons` > gear menu > **Install Add-on From File...**; or
- Sign it as an unlisted add-on with your AMO API keys: `npx web-ext sign -s extension --channel unlisted --api-key ... --api-secret ...`, then install the signed `.xpi` in normal Firefox.

The add-on id `yt-dl-firefox@local` must stay the same, because the native host manifest only allows that id.

## Usage

The backend starts itself when Firefox starts. While Firefox is open the add-on checks in every 30 seconds, so the backend keeps running; it exits about 5 minutes after Firefox closes, once nothing is downloading. To run it by hand: `.venv\Scripts\python backend\server.py` (the native host must still be installed, because it hands the add-on the backend key).

- **Watch page**: a split button sits next to Like. The main part downloads straight away with your default options and then shows live progress, "Done", or an error you can click to retry. The arrow opens the panel: Video or Audio, quality (with size estimates), codec, container, MP3/M4A and bitrate. Use **Save as default** to change what the main button does.
- **Shorts**: the same button appears in the Shorts action bar.
- **Playlists and channels**: a "Download playlist" or "Download channel" button in the page header opens the panel in list mode. Choose all items or a range such as `1-10,15`. Quality is a maximum; items that don't reach it download at their best. A channel downloads the tab you are on when that is Videos, Shorts or Live, and its Videos tab otherwise. One request takes at most 1000 items, so use a range for bigger channels.
- **Popup** (toolbar icon): backend status, ffmpeg and yt-dlp versions, an **Update** button when a newer yt-dlp is out, and every job with progress, speed and ETA. Pause, resume, cancel and retry jobs; open finished files or show them in their folder; **Clear finished** removes finished jobs from the list (the files stay). Playlist jobs are grouped with a total.
- **Notifications**: one per finished or failed single video, and one summary per playlist or channel when its last item finishes. Clicking a notification shows the (first) downloaded file in Explorer.

The queue lives in memory only, so it is empty after the backend restarts.

## Options

Open them from the popup's gear button or `about:addons`.

| Setting | Default | Notes |
|---|---|---|
| Default download | Video, Best, any codec, MP4 (audio: MP3 192 kbps) | Used by the one-click button |
| Parallel downloads | 2 | 1 to 5 |
| Download speed-up | 4 | Fragments fetched at once per download, 1 to 8 |
| Use Firefox cookies | Off | Passes `--cookies-from-browser firefox` to yt-dlp |
| Notifications | On | |
| Auto-update yt-dlp | On | Checks PyPI at most once a day and only updates while nothing is downloading |
| Page selectors | | CSS selectors for the watch page button row (plus before/after placement) and the Shorts action bar. Change these if YouTube moves its buttons; **Reset selectors** restores the defaults |

**Test backend** shows whether the backend is reachable and which versions it runs.

## Performance notes

- **Parallel downloads** run several videos at once. Two is a good balance; more mainly helps with many small files.
- **Fragments** (download speed-up) fetch parts of one video at once. This uses more bandwidth, not more CPU. Lower it if downloads slow down the rest of your network.
- **Low priority**: yt-dlp and the ffmpeg it starts run at below-normal priority, so downloads and MP3 conversion don't compete with games or other foreground apps.
- **File size guide** (rough, per 10 minutes of video):

  | Format | Size |
  |---|---|
  | 1080p (H.264) | 200 to 350 MB |
  | 1080p (VP9/AV1) | 100 to 200 MB |
  | 720p (H.264) | 100 to 180 MB |
  | 720p (VP9/AV1) | 50 to 100 MB |
  | MP3 128 kbps | about 1 MB per minute |
  | MP3 320 kbps | about 2.4 MB per minute |

  4K is often 1 GB or more per 10 minutes. The panel shows an estimate per quality when YouTube reports one.

## Troubleshooting

| Problem | Fix |
|---|---|
| Button says "Backend not running" | Run `.venv\Scripts\python backend\install_native_host.py` again and restart Firefox. To see startup errors, start `.venv\Scripts\python backend\server.py` by hand. The auto-started backend logs to `backend\data\server.log` |
| "Another program is using port 18956" | Something other than the backend holds the port, so the add-on refuses to talk to it. Close that program, then click **Start** in the popup |
| "Needs ffmpeg" or greyed-out options | Install ffmpeg and make sure `ffmpeg -version` works in a new terminal, then restart the backend |
| No button on the page | Reload the tab. If YouTube changed its layout, adjust the selectors in Options |
| "Sign in to confirm your age", members-only or private videos | Turn on **Use Firefox cookies** and make sure you are signed in to YouTube in Firefox |
| HTTP 403 or "Requested format is not available" | Usually an outdated yt-dlp. Click **Update** in the popup, or run `.venv\Scripts\python -m pip install -U yt-dlp` |
| "Extension was reloaded - refresh this page" | The add-on was reloaded after the tab opened; refresh the tab |
| Port 18956 already in use | Another backend is already running; that is fine. A second instance exits by itself (and logs a warning if the port holder is not the backend) |

## API reference

The backend listens on `http://127.0.0.1:18956` only. Every request must send `X-YTDL-Client: 1`, and any `Origin` header must start with `moz-extension://`; otherwise it returns 403. Every request except `GET /health` must also send `X-YTDL-Token` with the per-install key in `backend\data\token` (401 otherwise). The native host passes that key to the add-on, so other add-ons and local programs cannot drive the backend. `GET /health?nonce=<hex>` adds `proof`, the hex HMAC-SHA256 of the nonce under the key; the add-on and the native host check it before trusting whatever answers on the port. Errors are JSON `{"error": "...", "detail": "..."}`.

| Method and path | Body or query | Response |
|---|---|---|
| `GET /health` | `?nonce=` (optional) | `{status, version, ffmpeg, yt_dlp_version, yt_dlp_latest, update_available, concurrency, fragments, cookies, auto_update}`, plus `proof` when a nonce is sent |
| `GET /info` | `?url=` | `{kind, id, title, uploader, duration, qualities: [{height, codecs, filesize_est}], entry_count, entries}`. 400 bad URL, 502 yt-dlp failure, 504 after 20s |
| `POST /jobs` | `{url, options: {kind, quality, codec, container, audio_format, audio_bitrate}, items?}` | 202 `{jobs: [Job]}`. 400 bad URL or items, 422 needs ffmpeg |
| `GET /jobs` | | `{jobs: [Job]}` |
| `POST /jobs/{id}/{action}` | action: `pause`, `resume`, `cancel`, `retry`, `open`, `reveal` | `{job: Job}`. 404 unknown id, 409 invalid transition or file missing |
| `DELETE /jobs` | `?finished=true` | `{removed}` (files are kept) |
| `PUT /settings` | `{concurrency?: 1-5, fragments?: 1-8, cookies?, auto_update?}` | Settings object |
| `POST /update` | | `{updated, version}`. 409 while downloads are active |

Option values: `kind` video or audio; `quality` best, 2160, 1440, 1080, 720, 480 or 360; `codec` any, h264, vp9 or av1; `container` mp4 or mkv; `audio_format` mp3 or m4a; `audio_bitrate` 320, 192 or 128. `items` is a 1-based range like `1-10,15`.

A job is `{id, group_id, group_title, url, title, kind, status, percent, speed, eta, filename, filepath, error, detail, options, created_at}`, where `status` is one of `queued`, `downloading`, `processing`, `paused`, `complete`, `error` or `cancelled`.

## Development

```bat
cd backend
..\.venv\Scripts\python -m pytest -m "not slow" -q
..\.venv\Scripts\python -m pytest -m slow -q
cd ..
node --test extension/tests
npx web-ext lint -s extension
npx web-ext run -s extension
```

The `slow` test hits YouTube for real. Backend tests mock yt-dlp, pip and PyPI. `web-ext run` starts a throwaway Firefox profile with the add-on loaded and reloads it on change.

## Project structure

```
backend/
  server.py              FastAPI routes, request guard, idle timeout
  auth.py                Per-install key, /health proof and the port probe
  models.py              Request and settings models (enums and bounds)
  validation.py          URL allowlist and video/playlist/channel classification
  ytdlp.py               yt-dlp arguments, /info, progress parsing
  jobs.py                Download queue, workers, playlist expansion
  updater.py             yt-dlp version check and pip update
  platform_utils.py      Downloads folder, ffmpeg check, open/reveal, low priority
  native_messaging.py    Native host that starts the backend (.bat launcher alongside)
  install_native_host.py Native host registration
  test_*.py, conftest.py Tests
  data/                  Runtime settings, backend key, update state and log (ignored by git)
extension/
  manifest.json
  background.js          Only HTTP client: message routing, polling, badge, notifications
  shared/                constants.js, format.js, api.js
  content/               Page buttons and the download panel (core, panel, watch, shorts, lists)
  popup/                 Toolbar popup with the job list
  options/               Settings page
  icons/
  tests/                 node --test tests for shared/
native-messaging-host/   Generated native host manifest (ignored by git)
```
