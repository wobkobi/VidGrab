# VidGrab

A Firefox add-on that downloads YouTube videos, Shorts, playlists and channels through a small helper program running on your computer, which drives [yt-dlp](https://github.com/yt-dlp/yt-dlp) and ffmpeg. Version 2.6.0.

## What it does

- Adds a download button to watch pages, Shorts, playlist headers and channel headers.
- Downloads video (quality, codec preference, MP4 or MKV) or audio only (MP3 or M4A).
- Runs a queue with parallel downloads, pause, resume, cancel and retry, shown in the toolbar popup.
- Shows a desktop notification when a download finishes or fails.
- Can use your Firefox cookies for age-restricted, members-only or private videos.
- Keeps yt-dlp up to date and starts the helper automatically when Firefox needs it.

Files are saved to your Downloads folder as `Title.ext`. An existing file is never replaced: if that name is taken the download is saved as `Title (2).ext`, `Title (3).ext` and so on. While a download is in progress its files carry the video id (`Title [id].ext`), so two videos with the same title never get mixed up; the id is dropped once it finishes. A file of the old `Title [id].ext` form that is already in the folder counts as downloaded and keeps its name. Playlist and channel items go into `Downloads\<playlist title>\` or `Downloads\<channel name>\`.

Every file gets embedded tags: title, channel (as artist), upload date, description and the video URL. Video files also get a stream summary such as `1920x1080, 30 fps, avc1.640028` in the comment tag, which Explorer shows under Properties > Details.

## Requirements

- Firefox 140 or newer.
- Windows 10 or 11, macOS or Linux, on x64 or ARM64. Only Windows has been tested so far; the macOS and Linux builds compile and pass the unit tests but have not been run for real.
- An internet connection on first run, when the helper downloads yt-dlp (about 25 MB) and, if ffmpeg is not already installed, ffmpeg (about 100 MB).

## Setup

1. Download the helper for your system from `dist\` (or build it, see Development):

   | System | File |
   |---|---|
   | Windows | `vidgrab-windows-amd64.exe` (`-arm64` on ARM PCs) |
   | macOS | `vidgrab-macos-arm64` (Apple silicon) or `vidgrab-macos-amd64` (Intel) |
   | Linux | `vidgrab-linux-amd64` or `vidgrab-linux-arm64` |

2. Run it once.
   - **Windows**: double-click it. The helper is not code-signed yet, so SmartScreen may say "Windows protected your PC"; click **More info > Run anyway**.
   - **macOS**: in Terminal, `chmod +x vidgrab-macos-arm64 && ./vidgrab-macos-arm64 install`. If macOS blocks it, allow it under System Settings > Privacy & Security, then run it again.
   - **Linux**: `chmod +x vidgrab-linux-amd64 && ./vidgrab-linux-amd64 install`.

   It copies itself to a per-user folder and registers itself with Firefox. No admin rights are needed.

   | System | Installed to | Firefox registration |
   |---|---|---|
   | Windows | `%LOCALAPPDATA%\VidGrab\` | registry key `HKCU\Software\Mozilla\NativeMessagingHosts\vidgrab` |
   | macOS | `~/Library/Application Support/VidGrab/` | `~/Library/Application Support/Mozilla/NativeMessagingHosts/vidgrab.json` |
   | Linux | `~/.local/share/vidgrab/` | `~/.mozilla/native-messaging-hosts/vidgrab.json` |

   You can delete the downloaded file afterwards. Running a newer helper the same way upgrades the installed one.

3. Install the add-on and restart Firefox. On first use the popup shows "Setting up yt-dlp and ffmpeg..." until the downloads finish.

To remove the helper, run `vidgrab uninstall` from its install folder (or run the downloaded file with `uninstall`).

Firefox installed as a Snap or Flatpak on Linux cannot start programs outside its sandbox, so the helper does not work there yet.

### Installing the add-on

Install [VidGrab from addons.mozilla.org](https://addons.mozilla.org/firefox/addon/vidgrab/). Firefox then updates it by itself. The helper downloads for every system are on the [latest release](https://github.com/wobkobi/VidGrab/releases/latest).

For development, temporary add-ons (`about:debugging#/runtime/this-firefox` > **Load Temporary Add-on...** > `extension\manifest.json`) are removed when Firefox restarts. To keep it installed, either:

- Use Firefox Developer Edition or Nightly, set `xpinstall.signatures.required` to `false` in `about:config`, then run `npx web-ext build -s extension` and install the `.zip` from `about:addons` > gear menu > **Install Add-on From File...**; or
- Sign it as an unlisted add-on with your AMO API keys: `npx web-ext sign -s extension --channel unlisted --api-key ... --api-secret ...`, then install the signed `.xpi` in normal Firefox. Each signing needs a new version in `manifest.json`.

The add-on id `yt-dl-firefox@local` must stay the same, because the helper's Firefox registration only allows that id.

### Releases

Merging a new `version` in `extension/manifest.json` into main publishes it: `.github/workflows/release.yml` runs the tests, submits the add-on to AMO as a listed version, builds the six helpers, and creates the `v<version>` GitHub release with the helpers and `updates.json`. AMO reviews the version and Firefox updates installed copies from AMO once it is approved; `updates.json` only moves installs from 2.4.x (which updated from GitHub) over to AMO. A push without a version bump releases nothing. Signing needs the repository secrets `WEB_EXT_API_KEY` and `WEB_EXT_API_SECRET` (the API key and secret from addons.mozilla.org > Developer Hub > Manage API Keys).

## Usage

Firefox starts the helper when the add-on needs it. While Firefox is open the add-on checks in every 30 seconds, so the helper keeps running; it exits about 5 minutes after Firefox closes, once nothing is downloading.

- **Watch page**: a split button sits next to Like. The main part downloads straight away with your default options and then shows live progress (a percentage with a fill behind it), "Done", or an error you can click to retry. While a download is queued, running, converting or paused, the arrow turns into an X that cancels it. The download runs in the helper, so you can leave the page or close the tab once it has started; the popup keeps showing it. Otherwise the arrow opens the panel: Video or Audio, quality (with size estimates), codec, container, MP3/M4A and bitrate. Use **Save as default** to change what the main button does.
- **Shorts**: the same button appears in the Shorts action bar.
- **Three-dot menu**: on the home page, search results, related videos and playlists, a video's menu has a **Download** item that downloads it with your default options. A notice in the bottom-left corner confirms it; progress is in the popup.
- **Sound in MP4**: MP4 downloads prefer AAC sound. When a site only has sound MP4 players handle badly (Opus, Vorbis, FLAC), the sound is re-encoded to AAC (160 kbps stereo, 128 mono, 64 per channel for surround) and the picture is copied untouched.
- **Clips**: on a single video, fill in **Start** and/or **End** in the panel (`1:30`, `1:02:05` or plain seconds; an empty start means the beginning and an empty end the end of the video). **Now** next to each field takes the player's current time. Only that part is downloaded, cut on the exact second (ffmpeg re-encodes the cut), and saved as `Title (1m30s-4m05s).mp4` or `.mp3`, so a clip never replaces the whole video or another clip. Clips combine with **Max size**, which then only has to fit the clip, the easy way to get a sharp Discord upload out of a long video. A clip is never saved as a default.
- **Size limit (Discord)**: set **Max size** in the panel (for example 20 MB, the free Discord limit) and the video is saved as an H.264 + AAC MP4 that fits, named `Title (20 MB).mp4` so it never replaces a normal download of the same video (an existing file of that name gets ` (2)`, ` (3)` and so on). When YouTube already has H.264 + AAC streams small enough, at least as sharp as a conversion would be, they are downloaded as they are; otherwise the best source up to a suitable resolution is downloaded and re-encoded to H.264 + AAC at the bitrate that fits. The finished download is measured either way: one that fits is kept as it is, one that came out too big is converted. With a max size set, **Codec** offers H.264 (plays everywhere) or AV1: AV1 looks clearly better at the same size (on a music video at 20 MB for 24 minutes it scored VMAF 29 against 13 for the best H.264, and at 770 kbps 89 against 76) and encodes slower, but some phones and older devices cannot play it. An AV1 download keeps YouTube's own AV1 or H.264 streams when they fit, and otherwise converts with SVT-AV1. The job shows "converting" with its own progress while it encodes. Pausing during the conversion keeps the download and restarts the conversion on resume; if the helper exits first, the half-done work is deleted. A video too long to fit even at 50 kbps of video (10 fps, with 32 kbps mono sound) fails with the longest length that would fit.
  - **Picture and sound**: the lower the bitrate per second, the fewer frames and pixels it is spread over, because a smaller sharp picture beats a bigger smeared one. Under 200 kbps of video the output keeps at most 10 frames per second, under 400 at most 15, otherwise up to 30, never more than the source has (a 60 fps source drops to 30, a 50 fps one to 25). The resolution is then the tallest of 1080p, 720p, 480p, 360p, 240p and 144p that still gets enough bits for each pixel of each frame, never above the source or the chosen quality; for example 20 MB over 23 minutes comes out at 360p 10 fps, and 20 MB over a 3-minute 24 fps music video at 720p 24 fps. Sound is 96 kbps stereo AAC, 64 kbps stereo when the whole budget is under 400 kbps, and 32 kbps mono under 200, but never more than the downloaded sound itself has (1.5 times that for HE-AAC or Opus, which need about that much more in plain AAC): extra kbps could not make it sound better and would only come out of the picture. Mono sound stays mono at half the stereo rate, at least 32 kbps. A video with no sound at all gives the whole budget to the picture. AV1 keeps up to 30 frames per second at every bitrate (it loses far less to motion than H.264 does) and needs fewer bits per pixel, so it gets a taller picture at the same size.
  - **Encoder**: AV1 always uses SVT-AV1 (preset 5 up to 480p, 7 at 720p, 8 above). For H.264, under 1500 kbps of video, two-pass x264 is always used, because at low bitrates it looks far better than GPU encoders. At or above that, the first GPU encoder that passes a one-frame test is used: NVENC, then Quick Sync, then AMF on Windows; NVENC, Quick Sync, then VA-API (`/dev/dri/renderD128`, AMD and Intel) on Linux; VideoToolbox on macOS. Without one, or if the GPU encode fails (the helper then stops using that encoder until it restarts), it uses two-pass x264. Only NVENC and x264 have been tested on real hardware; Quick Sync, AMF, VA-API and VideoToolbox are untested.
- **Playlists and channels**: a "Download playlist" or "Download channel" button in the page header, styled to match YouTube's own buttons in that row, opens the panel in list mode. Choose all items or a range such as `1-10,15`. Quality is a maximum; items that don't reach it download at their best. A channel downloads the tab you are on when that is Videos, Shorts or Live, and its Videos tab otherwise. One request takes at most 1000 items, so use a range for bigger channels.
- **Popup** (toolbar icon): helper status, ffmpeg and yt-dlp versions, an **Update** button when a newer yt-dlp is out, and every job with progress, speed and ETA. Pause, resume, cancel and retry jobs; open finished files or show them in their folder; **Clear finished** removes finished jobs from the list (the files stay). The list keeps the newest 500 finished jobs. Playlist jobs are grouped with a total.
- **Notifications**: one per finished or failed single video, and one summary per playlist or channel when its last item finishes. Clicking a notification shows the (first) downloaded file in its folder.

The queue lives in memory only, so it is empty after the helper restarts.

## Options

Open them from the popup's gear button or `about:addons`.

| Setting | Default | Notes |
|---|---|---|
| Default download | Video, Best, any codec, MP4 (audio: MP3 192 kbps) | Used by the one-click button |
| Max size | Off | Video only: keep each download under this many MB (1 MB = 1,000,000 bytes, up to 4000), re-encoding when needed. Always an MP4 with AAC sound, H.264 unless the codec is AV1, saved as `Title (N MB).mp4` |
| Parallel downloads | 2 | 1 to 5 |
| Download speed-up | 4 | Fragments fetched at once per download, 1 to 8 |
| Use Firefox cookies | Off | Passes `--cookies-from-browser firefox` to yt-dlp |
| Notifications | On | |
| Auto-update yt-dlp | On | Checks GitHub at most once a day and only updates while nothing is downloading |
| Page selectors | | CSS selectors for the watch page button row (plus before/after placement) and the Shorts action bar. Change these if YouTube moves its buttons; **Reset selectors** restores the defaults |

**Test backend** shows whether the helper is reachable and which versions it runs.

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

## Where the helper gets its tools

- **yt-dlp**: the standalone build from the latest [yt-dlp release](https://github.com/yt-dlp/yt-dlp/releases), checked against the release's `SHA2-256SUMS`.
- **ffmpeg**: an `ffmpeg` and `ffprobe` already on `PATH` are used as they are. Otherwise the helper downloads the static GPL build from [yt-dlp/FFmpeg-Builds](https://github.com/yt-dlp/FFmpeg-Builds/releases) (Windows, Linux) or [ffmpeg.martin-riedl.de](https://ffmpeg.martin-riedl.de) (macOS), checked against the published SHA-256 sums. Set `VIDGRAB_IGNORE_PATH_FFMPEG=1` to force the download even when ffmpeg is on `PATH`.

Both live in the install folder under `data\tools\`. The folder also holds the backend key (`data\token`), settings, update state and the log (`data\server.log`).

## Troubleshooting

| Problem | Fix |
|---|---|
| "VidGrab helper not installed" | Download the helper from the [latest release](https://github.com/wobkobi/VidGrab/releases/latest) (the popup message and the page button both link there; a fresh install opens it by itself), run it once, then click **Check again** in the popup or restart Firefox |
| "Setting up yt-dlp and ffmpeg" stays for a long time | The first run downloads up to about 125 MB. If it shows an error, check your connection and click **Check again**; details are in `data\server.log` |
| "Another program is using the helper's port" | Something other than the helper answered on the helper's port, so the add-on refuses to talk to it. Click **Check again**: the helper then moves to a new random port by itself. |
| No button on the page | Reload the tab. If YouTube changed its layout, adjust the selectors in Options |
| "Sign in to confirm your age", members-only or private videos | Turn on **Use Firefox cookies** and make sure you are signed in to YouTube in Firefox |
| HTTP 403 or "Requested format is not available" | Usually an outdated yt-dlp. Click **Update** in the popup |
| "Extension was reloaded - refresh this page" | The add-on was reloaded after the tab opened; refresh the tab |

## API reference

The helper listens on `127.0.0.1` only, on a port picked at random (20000 to 32767) the first time it runs and saved in `data\port`. The native messaging host tells the add-on the port along with the key, and picks a new one if another program has taken it. Every request must send `X-YTDL-Client: 1`, and any `Origin` header must start with `moz-extension://`; otherwise it returns 403. Every request except `GET /health` must also send `X-YTDL-Token` with the per-install key in `data\token` (401 otherwise). The helper's native messaging host hands that key to the add-on, so other add-ons and local programs cannot drive the helper. `GET /health?nonce=<hex>` adds `proof`, the hex HMAC-SHA256 of the nonce under the key; the add-on and the native host check it before trusting whatever answers on the port. Errors are JSON `{"error": "...", "detail": "..."}`.

| Method and path | Body or query | Response |
|---|---|---|
| `GET /health` | `?nonce=` (optional) | `{status, version, ffmpeg, yt_dlp_version, yt_dlp_latest, update_available, concurrency, fragments, cookies, auto_update, tools: {state, step, percent, error}}`, plus `proof` when a nonce is sent |
| `GET /info` | `?url=` | `{kind, id, title, uploader, duration, qualities: [{height, codecs, filesize_est}], entry_count, entries}`. 400 bad URL, 502 yt-dlp failure, 503 during setup, 504 after 20s |
| `POST /jobs` | `{url, options: {kind, quality, codec, container, audio_format, audio_bitrate, max_size_mb, clip_start, clip_end}, items?}` | 202 `{jobs: [Job]}`. 400 bad URL or items, or a clip on a playlist or channel, 422 needs ffmpeg, 503 during setup |
| `GET /jobs` | | `{jobs: [Job]}` |
| `POST /jobs/{id}/{action}` | action: `pause`, `resume`, `cancel`, `retry`, `open`, `reveal` | `{job: Job}`. 404 unknown id, 409 invalid transition or file missing |
| `DELETE /jobs` | `?finished=true` | `{removed}` (files are kept) |
| `PUT /settings` | `{concurrency?: 1-5, fragments?: 1-8, cookies?, auto_update?}` | Settings object |
| `POST /update` | | `{updated, version}`. 409 while downloads are active |
| `POST /shutdown` | | `{status: "stopping"}`, then cancels running jobs and exits. Used by install and uninstall |

`tools.state` is `installing`, `ready` or `error`. Option values: `kind` video or audio; `quality` best, 2160, 1440, 1080, 720, 480 or 360; `codec` any, h264, vp9 or av1; `container` mp4 or mkv; `audio_format` mp3 or m4a; `audio_bitrate` 320, 192 or 128. `max_size_mb` 0 (off, the default) or 1 to 4000; it applies to video only, forces MP4 and needs ffmpeg (422 without it); with it, `codec` av1 converts to AV1 and every other value to H.264. `clip_start` and `clip_end` are whole seconds (0 to 360000, both 0 by default, `clip_end` 0 meaning the end of the video, otherwise after `clip_start`); a clip needs ffmpeg and works on single videos only. `items` is a 1-based range like `1-10,15`.

A job is `{id, group_id, group_title, url, title, kind, status, stage, percent, speed, eta, filename, filepath, error, detail, options, created_at}`, where `status` is one of `queued`, `downloading`, `processing`, `paused`, `complete`, `error` or `cancelled`. `stage` is `""` except while a size-limited video is re-encoded, when it is `converting` (status stays `processing`, `percent` runs 0-100 for the conversion, `speed` reads like `2.4x` and `eta` is set when known).

## Development

Needs Go 1.22+ and Node.js.

```bat
cd helper
go vet ./...
go test ./...
cd ..
node scripts/build-helper.mjs
node --test extension/tests
npx web-ext lint -s extension
npx web-ext run -s extension
```

`build-helper.mjs` cross-compiles all six helpers into `dist\` and stamps them with the add-on version; `--only windows-amd64` builds one. Tests fake the network and yt-dlp; set `VIDGRAB_TEST_YTDLP` to a yt-dlp binary to also run the real `/info` test. `vidgrab serve --port N --data-dir DIR --downloads-dir DIR` runs a throwaway helper that doesn't touch the installed one. `web-ext run` starts a throwaway Firefox profile with the add-on loaded and reloads it on change.

## Project structure

```
helper/
  main.go                   Entry point: install, uninstall, serve, or native messaging host
  internal/auth/            Per-install key, /health proof and the port probe
  internal/core/            URL validation, request models, yt-dlp arguments, /info summary, progress parsing, size-limit fitting and ffmpeg re-encoding
  internal/server/          HTTP API, request guard, idle timeout, download queue (size-limited jobs in sized.go)
  internal/system/          Install dirs, tool downloads, yt-dlp updates, native host, install/uninstall, OS specifics
scripts/build-helper.mjs    Cross-compile the helper into dist/
extension/
  manifest.json
  background.js             Only HTTP client: message routing, polling, badge, notifications
  shared/                   constants.js, format.js, api.js
  content/                  Page buttons and the download panel (core, panel, watch, shorts, lists)
  popup/                    Toolbar popup with the job list
  options/                  Settings page
  icons/
  tests/                    node --test tests for shared/
```

## Licence

MIT. See [LICENSE](LICENSE).
