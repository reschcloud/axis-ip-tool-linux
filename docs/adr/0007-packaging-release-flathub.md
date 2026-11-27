# ADR 0007: Packaging, fixed artifacts and Flathub preparation

- Status: Accepted
- Date: 2026-11-27

## Context

The app ships as distribution packages and, from v0.5.0, is prepared for Flathub. Users need
predictable artifact names; Flathub builds offline in a sandbox and expects a lowercase app ID.

## Decision

- Ship one package `ip-tool-for-axis` containing both GUI and CLI; version from the Git tag.
- Build release binaries natively on Ubuntu 24.04 (oldest glibc) for x86_64 and arm64. Rename
  artifacts to the fixed names consumed by `packaging/install.sh`.
- Use the lowercase app ID `io.github.reschcloud.iptoolforaxis` for Flatpak, desktop, AppStream
  and icons; keep the visible name unchanged.
- Vendor Go modules (`vendor/`) so Flatpak/Flathub builds with `GOFLAGS=-mod=vendor`,
  `GOPROXY=off` and no network. Keep the local `type: dir` manifest separate from the Flathub
  manifest with a pinned tag/commit.
- Install the license under `/app/share/licenses/<app-id>/`; pin the AppStream screenshot URL to
  the release tag; validate desktop/AppStream metadata in CI.
- The Flatpak has network and Avahi system-D-Bus access, justified by camera discovery/VAPIX.

## Consequences

- Releases are reproducible offline; Flathub review can verify permissions and metadata.
- Existing local Flatpak installs of an older app ID require a reinstall (no Flathub release yet).
- The release checklist lives in `FLATHUB.md`.
