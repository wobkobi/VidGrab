# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/). Each bullet starts with the area it changes
(`- **Popup:** ...`); a release's notes on GitHub are its section regrouped by area.

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

