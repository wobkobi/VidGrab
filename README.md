# VidGrab

A Firefox add-on that downloads YouTube videos, Shorts, playlists and channels. It works through a small helper program on your computer, which drives [yt-dlp](https://github.com/yt-dlp/yt-dlp) and ffmpeg.

**Version 2.7.1** · [Add-on on AMO](https://addons.mozilla.org/firefox/addon/vidgrab/) · [Helper downloads](https://github.com/wobkobi/VidGrab/releases/latest) · [Changelog](CHANGELOG.md)

## What it does

- Adds a download button to watch pages, Shorts, playlist headers and channel headers.
- Downloads video (quality, codec, MP4 or MKV) or audio only (MP3 or M4A).
- Cuts clips and shrinks videos to fit a size limit, such as Discord's 20 MB.
- Runs a queue with parallel downloads, pause, resume, cancel and retry, shown in the toolbar popup.
- Shows a desktop notification when a download finishes or fails.
- Can use your Firefox cookies for age-restricted, members-only or private videos.
- Keeps yt-dlp up to date and starts the helper automatically when Firefox needs it.

## Requirements

- Firefox 142 or newer.
- Windows 10 or 11, macOS or Linux (see the table below).
- An internet connection on first run, when the helper downloads yt-dlp (about 25 MB) and, if ffmpeg is not already installed, ffmpeg (about 100 MB).

| System | Helper file | Tested |
|---|---|---|
| Windows (x64, or ARM through x64 emulation) | `vidgrab-windows-amd64.exe` | Yes |
| macOS, Apple silicon | `vidgrab-macos-arm64` | Builds and passes unit tests only |
| macOS, Intel | `vidgrab-macos-amd64` | Builds and passes unit tests only |
| Linux (x64) | `vidgrab-linux-amd64` | Builds and passes unit tests only |

Firefox installed as a Snap or Flatpak on Linux cannot start programs outside its sandbox, so the helper does not work there yet.

## Setup

1. **Download the helper** for your system from the [latest release](https://github.com/wobkobi/VidGrab/releases/latest).
2. **Run it once.** No admin rights are needed.
   - **Windows**: double-click it. The helper is not code-signed yet, so SmartScreen may say "Windows protected your PC"; click **More info > Run anyway**.
   - **macOS**: in Terminal, run `chmod +x vidgrab-macos-arm64 && ./vidgrab-macos-arm64 install`. If macOS blocks it, allow it under System Settings > Privacy & Security, then run it again.
   - **Linux**: run `chmod +x vidgrab-linux-amd64 && ./vidgrab-linux-amd64 install`.
3. **Install [VidGrab for YouTube](https://addons.mozilla.org/firefox/addon/vidgrab/)** from addons.mozilla.org and restart Firefox. Firefox keeps the add-on up to date by itself.

On first use the popup shows "Setting up yt-dlp and ffmpeg..." until those downloads finish.

The helper copies itself to a per-user folder and registers itself with Firefox, so you can delete the downloaded file afterwards. To upgrade, run a newer helper the same way. To remove it, run `vidgrab uninstall` from its install folder (or run the downloaded file with `uninstall`).

| System | Installed to | Firefox registration |
|---|---|---|
| Windows | `%LOCALAPPDATA%\VidGrab\` | registry key `HKCU\Software\Mozilla\NativeMessagingHosts\vidgrab` |
| macOS | `~/Library/Application Support/VidGrab/` | `~/Library/Application Support/Mozilla/NativeMessagingHosts/vidgrab.json` |
| Linux | `~/.local/share/vidgrab/` | `~/.mozilla/native-messaging-hosts/vidgrab.json` |

## Usage

Firefox starts the helper when the add-on needs it. While Firefox is open the add-on checks in every 30 seconds, which keeps the helper running. It exits about 5 minutes after Firefox closes, once nothing is downloading.

### Watch pages and Shorts

A split button sits next to Like (on Shorts, in the action bar).

- **The main part** downloads straight away with your default options. It then shows live progress, "Done", or an error you can click to retry.
- **While a download is active** (queued, running, converting or paused), the arrow turns into an X that cancels it.
- **Otherwise the arrow** opens the panel: Video or Audio, quality (with size estimates), codec, container, MP3/M4A and bitrate, Max size and clip times. **Save as default** changes what the main button does.

The download runs in the helper, so you can leave the page or close the tab once it has started. The popup keeps showing it.

### Three-dot menu

On the home page, search results, related videos and playlists, a video's menu has a **Download with VidGrab** item (with the red VidGrab icon, separate from YouTube's own Download). It downloads with your default options; a notice in the bottom-left corner confirms it and progress is in the popup.

### Playlists and channels

A **Download playlist** or **Download channel** button in the page header opens the panel in list mode.

- Choose all items or a range such as `1-10,15`.
- Quality is a maximum; items that don't reach it download at their best.
- A channel downloads the tab you are on when that is Videos, Shorts or Live, and its Videos tab otherwise.
- One request takes at most 1000 items, so use a range for bigger channels.

### Clips

On a single video, fill in **Start** and/or **End** in the panel: `1:30`, `1:02:05` or plain seconds. An empty start means the beginning and an empty end the end of the video. **Now** next to each field takes the player's current time.

- Only that part is downloaded, cut on the exact second (ffmpeg re-encodes the cut).
- It is saved as `Title (1m30s-4m05s).mp4` or `.mp3`, so a clip never replaces the whole video or another clip.
- Clips combine with **Max size**, which then only has to fit the clip. That is the easy way to get a sharp Discord upload out of a long video.
- A clip is never saved as a default.

### Size limit

Set **Max size** in the panel (presets from 10 MB to 1 GB, including 20 MB for Discord, or any custom size). The video is saved as an MP4 that fits, named `Title (20 MB).mp4` so it never replaces a normal download of the same video.

- **Codec**: H.264 plays everywhere. AV1 looks clearly better at the same size but encodes slower, and some phones and older devices cannot play it.
- **Progress**: the job shows "converting" with its own progress while it encodes. Pausing during the conversion keeps the download and restarts the conversion on resume.
- **Too long to fit**: a video that won't fit even at the lowest quality (50 kbps of video at 10 fps, 32 kbps mono sound) fails with the longest length that would fit. Use a clip instead.

How the helper picks the resolution, frame rate, sound and encoder is under [How Max size works](#how-max-size-works).

### Sound in MP4

MP4 downloads prefer AAC sound. When YouTube only has sound that MP4 players handle badly (Opus, Vorbis, FLAC), the sound is re-encoded to AAC (160 kbps stereo, 128 mono, 64 per channel for surround) and the picture is copied untouched.

### Popup

Click the toolbar icon for:

- Helper status, ffmpeg and yt-dlp versions, and an **Update** button when a newer yt-dlp is out.
- Every job (title and channel) with progress, speed and ETA. Click a title to go back to the video.
- Pause, resume, cancel and retry. Finished jobs have **Open** and **Show in folder**.
- **Clear finished** removes finished jobs from the list (the files stay). The list keeps the newest 500 finished jobs, and playlist jobs are grouped with a total.

The queue lives in memory only, so it is empty after the helper restarts.

### Notifications

One per finished or failed single video, and one summary per playlist or channel when its last item finishes. Clicking a notification shows the (first) downloaded file in its folder.

### Saved files

- **Where**: your Downloads folder. Playlist and channel items go into `Downloads\<playlist title>\` or `Downloads\<channel name>\`.
- **Names**: `Title.ext`. An existing file is never replaced: if the name is taken the download is saved as `Title (2).ext`, `Title (3).ext` and so on.
- **While downloading**, files carry the video id (`Title [id].ext`) so two videos with the same title never get mixed up. The id is dropped once it finishes. A file of the old `Title [id].ext` form already in the folder counts as downloaded and keeps its name.
- **Tags**: every file gets the title, channel (as artist), upload date, description and video URL. Video files also get a stream summary such as `1920x1080, 30 fps, avc1.640028` in the comment tag, which Explorer shows under Properties > Details.

## Options

Open them from the popup's gear button or `about:addons`.

| Setting | Default | Notes |
|---|---|---|
| Default download | Video, Best, any codec, MP4 (audio: MP3 192 kbps) | Used by the one-click button |
| Max size | Off | Video only. Up to 4000 MB, where 1 MB = 1,000,000 bytes |
| Parallel downloads | 2 | 1 to 5 |
| Download speed-up | 4 | Fragments fetched at once per download, 1 to 8 |
| Use Firefox cookies | Off | Passes `--cookies-from-browser firefox` to yt-dlp |
| Notifications | On | |
| Auto-update yt-dlp | On | Checks GitHub at most once a day and only updates while nothing is downloading |
| Page selectors | | Where the buttons go on watch pages and Shorts. Change these if YouTube moves its buttons; **Reset selectors** restores the defaults |

**Test backend** shows whether the helper is reachable and which versions it runs.

## Troubleshooting

| Problem | Fix |
|---|---|
| "VidGrab helper not installed" | Download the helper from the [latest release](https://github.com/wobkobi/VidGrab/releases/latest) (the popup and the page button both link there; a fresh install opens it by itself), run it once, then click **Check again** in the popup or restart Firefox |
| "Helper out of date" | Download the latest helper and run it once; it replaces the installed one |
| "Setting up yt-dlp and ffmpeg" stays for a long time | The first run downloads up to about 125 MB. If it shows an error, check your connection and click **Check again**; details are in `data\server.log` |
| "Another program is using the helper's port" | Something other than the helper answered on its port, so the add-on refuses to talk to it. Click **Check again**: the helper then moves to a new random port by itself |
| No button on the page | Reload the tab. If YouTube changed its layout, adjust the selectors in Options |
| "Sign in to confirm your age", members-only or private videos | Turn on **Use Firefox cookies** and make sure you are signed in to YouTube in Firefox |
| HTTP 403 or "Requested format is not available" | Usually an outdated yt-dlp. Click **Update** in the popup |
| "Extension was reloaded - refresh this page" | The add-on was reloaded after the tab opened; refresh the tab |

## Performance notes

- **Parallel downloads** run several videos at once. Two is a good balance; more mainly helps with many small files.
- **Download speed-up** fetches parts of one video at once. This uses more bandwidth, not more CPU. Lower it if downloads slow down the rest of your network.
- **Low priority**: yt-dlp and the ffmpeg it starts run at below-normal priority, so downloads and conversions don't compete with games or other foreground apps.

Rough file sizes per 10 minutes of video (4K is often 1 GB or more). The panel shows an estimate per quality when YouTube reports one.

| Format | Size |
|---|---|
| 1080p (H.264) | 200 to 350 MB |
| 1080p (VP9/AV1) | 100 to 200 MB |
| 720p (H.264) | 100 to 180 MB |
| 720p (VP9/AV1) | 50 to 100 MB |
| MP3 128 kbps | about 10 MB |
| MP3 320 kbps | about 24 MB |

## How Max size works

**Download or convert.** When YouTube already has H.264 + AAC streams small enough, and at least as sharp as a conversion would be, they are downloaded as they are. Otherwise the best source up to a suitable resolution is downloaded and re-encoded at the bitrate that fits. The finished download is measured either way: one that fits is kept, one that came out too big is converted. If the helper exits mid-conversion, the half-done work is deleted.

**Frame rate.** The lower the bitrate, the fewer frames and pixels it is spread over, because a smaller sharp picture beats a bigger smeared one. For H.264:

| Video bitrate | Frame rate |
|---|---|
| Under 200 kbps | up to 10 fps |
| 200 to 400 kbps | up to 15 fps |
| 400 kbps and up | up to 30 fps |

It is never more than the source has; a 60 fps source drops to 30 and a 50 fps one to 25. AV1 keeps up to 30 fps at every bitrate, since it loses far less to motion than H.264 does.

**Resolution.** The tallest of 1080p, 720p, 480p, 360p, 240p and 144p that still gets enough bits for each pixel of each frame, never above the source or the chosen quality. AV1 needs fewer bits per pixel, so it gets a taller picture at the same size. For example, with H.264:

- 20 MB over 23 minutes comes out at 360p 10 fps.
- 20 MB over a 3-minute 24 fps music video comes out at 720p 24 fps.

**Sound.** AAC, never more than the downloaded sound itself has (1.5 times that for HE-AAC or Opus, which need about that much more in plain AAC), since extra kbps could not make it sound better and would only come out of the picture.

| Whole budget | Sound |
|---|---|
| 400 kbps and up | 96 kbps stereo |
| 200 to 400 kbps | 64 kbps stereo |
| Under 200 kbps | 32 kbps mono |

Mono sound stays mono at half the stereo rate, at least 32 kbps. A video with no sound gives the whole budget to the picture.

**AV1 against H.264.** On a music video at 20 MB for 24 minutes, AV1 scored VMAF 29 against 13 for the best H.264, and at 770 kbps 89 against 76. An AV1 download keeps YouTube's own AV1 or H.264 streams when they fit, and otherwise converts with SVT-AV1.

**Encoder.**

- **AV1** always uses SVT-AV1: preset 5 up to 480p, 7 at 720p, 8 above.
- **H.264 under 1500 kbps** always uses two-pass x264, because at low bitrates it looks far better than GPU encoders.
- **H.264 at 1500 kbps and up** uses the first GPU encoder that passes a one-frame test:
  - Windows: NVENC, then Quick Sync, then AMF.
  - Linux: NVENC, Quick Sync, then VA-API (`/dev/dri/renderD128`, AMD and Intel).
  - macOS: VideoToolbox.

  Without one, or if the GPU encode fails, it uses two-pass x264 (and stops using that encoder until the helper restarts). Only NVENC and x264 have been tested on real hardware.

## Where the helper gets its tools

- **yt-dlp**: the standalone build from the latest [yt-dlp release](https://github.com/yt-dlp/yt-dlp/releases), checked against the release's `SHA2-256SUMS`.
- **ffmpeg**: an `ffmpeg` and `ffprobe` already on `PATH` are used as they are. Otherwise the helper downloads the static GPL build from [yt-dlp/FFmpeg-Builds](https://github.com/yt-dlp/FFmpeg-Builds/releases) (Windows, Linux) or [ffmpeg.martin-riedl.de](https://ffmpeg.martin-riedl.de) (macOS), checked against the published SHA-256 sums. Set `VIDGRAB_IGNORE_PATH_FFMPEG=1` to force the download even when ffmpeg is on `PATH`.

Both live in the install folder under `data\tools\`. The folder also holds the backend key (`data\token`), settings, update state and the log (`data\server.log`).

## API reference

The helper listens on `127.0.0.1` only, on a port picked at random (20000 to 32767) the first time it runs and saved in `data\port`. The native messaging host tells the add-on the port along with the key, and picks a new port if another program has taken it.

**Request rules:**

- Every request must send `X-YTDL-Client: 1`, and any `Origin` header must start with `moz-extension://` (403 otherwise).
- Every request except `GET /health` must also send `X-YTDL-Token` with the per-install key in `data\token` (401 otherwise). The native messaging host hands that key to the add-on, so other add-ons and local programs cannot drive the helper.
- `GET /health?nonce=<hex>` adds `proof`, the hex HMAC-SHA256 of the nonce under the key. The add-on and the native host check it before trusting whatever answers on the port.
- Errors are JSON `{"error": "...", "detail": "..."}`.

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

**Option values:**

| Option | Values |
|---|---|
| `kind` | `video` or `audio` |
| `quality` | `best`, `2160`, `1440`, `1080`, `720`, `480` or `360` |
| `codec` | `any`, `h264`, `vp9` or `av1`. With `max_size_mb`, `av1` converts to AV1 and every other value to H.264 |
| `container` | `mp4` or `mkv` |
| `audio_format` | `mp3` or `m4a` |
| `audio_bitrate` | `320`, `192` or `128` |
| `max_size_mb` | `0` (off, the default) or 1 to 4000. Video only; forces MP4 and needs ffmpeg (422 without it) |
| `clip_start`, `clip_end` | Whole seconds, 0 to 360000, both 0 by default. `clip_end` 0 means the end of the video, otherwise it must be after `clip_start`. Needs ffmpeg; single videos only |
| `items` | A 1-based range like `1-10,15` |

`tools.state` is `installing`, `ready` or `error`.

**Job:** `{id, group_id, group_title, url, title, channel, kind, status, stage, percent, speed, eta, filename, filepath, error, detail, options, created_at}`.

- `status` is `queued`, `downloading`, `processing`, `paused`, `complete`, `error` or `cancelled`.
- `stage` is `""` except while a size-limited video is re-encoded, when it is `converting`. The status then stays `processing`, `percent` runs 0-100 for the conversion, `speed` reads like `2.4x` and `eta` is set when known.

## Development

Needs Go 1.22+, Node.js and, for the Windows helper, `windres` from [llvm-mingw](https://github.com/mstorsjo/llvm-mingw) (or `binutils-mingw-w64-x86-64` on Ubuntu) to embed its icon and version details.

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

- `build-helper.mjs` cross-compiles all four helpers into `dist\` and stamps them with the add-on version; `--only windows-amd64` builds one. The Windows icon is `helper/winres/icon.ico`, rendered from `extension/icons/icon.svg`.
- Tests fake the network and yt-dlp. Set `VIDGRAB_TEST_YTDLP` to a yt-dlp binary to also run the real `/info` test.
- `vidgrab serve --port N --data-dir DIR --downloads-dir DIR` runs a throwaway helper that doesn't touch the installed one.
- `web-ext run` starts a throwaway Firefox profile with the add-on loaded and reloads it on change.

### Installing a development build

Temporary add-ons (`about:debugging#/runtime/this-firefox` > **Load Temporary Add-on...** > `extension\manifest.json`) are removed when Firefox restarts. To keep a build installed, either:

- **Unsigned**: in Firefox Developer Edition or Nightly, set `xpinstall.signatures.required` to `false` in `about:config`. Run `node scripts/build-xpi.mjs` and install `dist/vidgrab-<version>.xpi` from `about:addons` > gear menu > **Install Add-on From File...** (regular Firefox refuses unsigned add-ons).
- **Signed**: sign it as an unlisted add-on with your AMO API keys, `npx web-ext sign -s extension --channel unlisted --api-key ... --api-secret ...`, then install the signed `.xpi` in normal Firefox. Each signing needs a new version in `manifest.json`.

The add-on id `yt-dl-firefox@local` must stay the same, because the helper's Firefox registration only allows that id.

### Releases

Merging a new `version` in `extension/manifest.json` into main publishes it. `.github/workflows/release.yml` then:

1. Runs the tests.
2. Builds the four helpers.
3. Submits the add-on to AMO as a listed version.
4. Creates the `v<version>` GitHub release with the helpers, `updates.json` and notes made from `CHANGELOG.md`.

AMO reviews the version, and Firefox updates installed copies from AMO once it is approved. `updates.json` only moves installs from 2.4.x (which updated from GitHub) over to AMO. A push without a version bump releases nothing.

- **Changelog**: every version needs a `## [x.y.z]` section in `CHANGELOG.md`, with each bullet naming its area (`- **Popup:** ...`). The release stops before submitting to AMO when it is missing. `node scripts/release-notes.mjs <version>` prints the notes it would publish.
- **Helper compatibility**: users update the add-on and the helper separately, so the add-on has to keep working with any helper from `MIN_HELPER_VERSION` in `extension/shared/constants.js` on; the popup warns when the running helper is older. Before a change to the add-on, check that it only uses what that helper already has (its `/health`, `/jobs` and job fields), and raise `MIN_HELPER_VERSION` in the same change when it needs something newer.
- **Secrets**: signing needs the repository secrets `WEB_EXT_API_KEY` and `WEB_EXT_API_SECRET` (the API key and secret from addons.mozilla.org > Developer Hub > Manage API Keys).

## Project structure

```
helper/
  main.go                   Entry point: install, uninstall, serve, or native messaging host
  internal/auth/            Per-install key, /health proof and the port probe
  internal/core/            URL validation, request models, yt-dlp arguments, /info summary, progress parsing, size-limit fitting and ffmpeg re-encoding
  internal/server/          HTTP API, request guard, idle timeout, download queue (size-limited jobs in sized.go)
  internal/system/          Install dirs, tool downloads, yt-dlp updates, native host, install/uninstall, OS specifics
scripts/build-helper.mjs    Cross-compile the helper into dist/
scripts/build-xpi.mjs       Package the add-on as an unsigned .xpi in dist/
scripts/release-notes.mjs   Release notes from CHANGELOG.md, grouped by area
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

## Privacy

The add-on talks only to the helper on your own computer. The helper connects to:

- the sites you ask it to download from, through yt-dlp;
- GitHub, to download and update yt-dlp and, on Windows and Linux, ffmpeg;
- ffmpeg.martin-riedl.de, to download ffmpeg on macOS when it isn't already installed.

Neither sends analytics, usage data or personal information anywhere. Cookies are read from Firefox only when you turn that setting on, and are passed to yt-dlp for the download you started.

## Licence

MIT. See [LICENSE](LICENSE).
