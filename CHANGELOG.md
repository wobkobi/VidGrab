# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/). Each bullet starts with the area it changes
(`- **Popup:** ...`); a release's notes on GitHub are its section regrouped by area.

## [2.7.0] - 2026-11-22

### Added

- **Popup:** When the helper is older than this version of the add-on needs, the popup says so and
  links to the download page.
- **Popup:** Clicking a job's title goes back to its video, switching to the tab it is open in
  or opening a new one.
- **Size limit:** 10 MB, 25 MB, 100 MB, 250 MB and 1 GB presets.
- **Settings:** Advanced lists other page selectors to add to or remove from each field when YouTube
  moves its buttons.

### Changed

- **Popup:** Open and Show in folder sit beside a finished job's bar instead of on their own line.
- **Settings:** A new look: a header with the version and links to the guide, changelog, helper
  download and problem reports, the sections stacked in one panel with Save below, Advanced folded
  away until opened, and notes and warnings in tinted boxes.
- **Settings:** Video and audio defaults no longer look greyed out when they don't match the default
  type; the panel starts from all of them. MP3 bitrate is only greyed out for M4A.

## [2.6.1] - 2026-11-22

### Changed

- **Three-dot menu:** The item reads "Download with VidGrab" and has the red VidGrab icon, so it
  stands apart from YouTube's own Download.
- **Popup:** Finished jobs no longer repeat the file name under the bar; hovering Open shows it.
- **Installing:** Firefox 142 or newer is needed.
- **Helper:** Downloads are for Windows x64 (which also runs on Windows on ARM), macOS on Apple
  silicon or Intel, and Linux x64.

## [2.6.0] - 2026-11-21

### Added

- **Three-dot menu:** Videos on the home page, in search results, related videos and playlists have
  a Download item in their three-dot menu, which downloads with your default options.
- **Popup:** Each job shows the video's channel under its title.
- **Popup:** The list keeps the newest 500 finished jobs.

### Fixed

- **Download button:** Stays in the like and share row instead of landing in the Wikipedia or
  fact-check box above the title.
- **Download button:** Keeps its width while a download runs instead of shrinking to the percentage.
- **Download button:** Shows "Preparing" until the first progress report instead of sitting at 0%.
- **Download button:** A failed download shows "Failed - retry", with the whole error on hover,
  instead of a cut-off message.
- **Popup:** Open brings the video's window to the front instead of opening it behind Firefox.
- **Popup:** A long list scrolls inside the popup instead of pushing the footer off the screen.
- **Notifications:** A finished download notifies once.

## [2.5.1] - 2026-11-21

### Changed

- **Installing:** VidGrab is listed on addons.mozilla.org and updates from there once each version
  is approved. Copies installed from GitHub move over to AMO by themselves.

### Fixed

- **Popup:** When the helper isn't installed, the message links to its download page.
- **Download button:** Clicking it while the helper isn't installed opens the download page instead
  of retrying.
- **Installing:** A fresh install with no helper opens the download page in a new tab.

## [2.4.2] - 2026-11-21

### Fixed

- **Helper:** Builds again after Go version updates. Nothing changes in how it works.

## [2.4.1] - 2026-11-21

### Added

- **Download button:** A split button next to Like on watch pages and in the Shorts action bar.
  The main part downloads with your default options and shows live progress; the arrow opens a
  panel for video or audio, quality with size estimates, codec, container, MP3 or M4A and bitrate.
- **Download button:** While a download is queued, running, converting or paused, the arrow turns
  into an X that cancels it.
- **Playlists and channels:** A Download playlist or Download channel button in the page header
  downloads every item or a range such as 1-10,15, up to 1000 at a time.
- **Clips:** Start and End times in the panel download only that part of a video, saved under its
  own name so it never replaces the whole video.
- **Size limit:** Max size (such as 20 MB for Discord) saves an MP4 that fits, in H.264 for any
  player or AV1 for a sharper picture at the same size.
- **Popup:** Every job with progress, speed and ETA. Pause, resume, cancel and retry downloads,
  open finished files or show them in their folder, and update yt-dlp.
- **Notifications:** One when a video finishes or fails, and one summary per playlist or channel.
- **Files:** Saved to Downloads as Title.ext without ever replacing a file, with the title,
  channel, upload date, description and link embedded.
- **Settings:** Can use your Firefox cookies for age-restricted, members-only or private videos.
- **Helper:** One program for Windows, macOS and Linux that installs itself, fetches yt-dlp and
  ffmpeg on first run and starts whenever Firefox needs it.
