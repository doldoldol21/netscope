# Changelog

What changed for people using netscope, newest first. Each release on
[GitHub Releases](https://github.com/doldoldol21/netscope/releases) opens with
its section from this file, followed by the full list of pull requests.
Releases before 0.30.0 are described there only.

## Unreleased

- The name peek opens beside the name instead of below it, so it no longer
  covers the next app in the list and moving down the list reaches it.

## 0.30.6 — 2026-09-29

- The name peek in the app lists is smaller: "click to copy" is now a copy
  icon beside the name, so the peek no longer covers the row below it.

## 0.30.5 — 2026-09-29

- Settings open at their full height. The popover grows down to fit them and
  shrinks back when they close, instead of scrolling half the settings out of
  view.

## 0.30.4 — 2026-09-29

- Updating with `brew upgrade` or `install.sh` now takes effect on its own: the
  running app notices its bundle was replaced and relaunches into the new
  version. It used to keep running the old build and offer an update to the
  version already installed.
- The app bundle reports its real version (Finder, `defaults read`) instead
  of 0.1.0.

## 0.30.3 — 2026-09-29

- The usage figures and app list no longer show through the settings.

## 0.30.2 — 2026-09-29

- Hovering an app name shows its details sooner (after 0.18 s instead of
  0.55 s), and at once when moving from one name to the next.

## 0.30.1 — 2026-09-28

- Fixed the capture helper never starting on a Homebrew install, with the
  admin prompt coming back on every launch.

## 0.30.0 — 2026-09-28

- The dashboard is drawn the way macOS draws its own windows: system font,
  the sidebar material, segmented controls and Finder-style tables. It
  follows the light/dark theme setting.
