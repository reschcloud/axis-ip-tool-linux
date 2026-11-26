# IP Tool for AXIS (Linux)

## Build and verify

- Start with `make test`: it vets `./internal/... ./cmd/ip-tool-for-axis-cli` and tests `./internal/...`. It does **not** compile the GUI. The default suite uses loopback fixtures, not cameras or Avahi; run one workflow with `go test ./internal/axis -run '^TestWorkflow$' -count=1`. The live catalog check is opt-in: `AXIS_ONLINE_TEST=1 go test ./internal/axis -run '^TestFirmwareCatalogOnline$' -count=1` (contacts axis.com).
- `make cli` builds `build/ip-tool-for-axis-cli` with `CGO_ENABLED=0`. Use `make gui` for GTK changes (or `make build` for both); keep `-trimpath` when building manually or Go will rebuild the expensive GTK bindings in a separate cache.
- CI uses latest stable Go, not the `go.mod` minimum, for patched stdlib and govulncheck. The GUI compatibility floor is Ubuntu 24.04 (GTK 4.14, GLib 2.80, libadwaita 1.5); keep the gotk4 pins and avoid newer libadwaita APIs unless changing that floor. Ubuntu build deps: `libgtk-4-dev libadwaita-1-dev libgirepository1.0-dev`.
- For desktop metadata changes, CI runs `desktop-file-validate data/*.desktop` and `appstreamcli validate --no-net data/*.metainfo.xml`.

## Code paths and invariants

- Keep device logic in `internal/axis`, shared by the GTK app (`cmd/ip-tool-for-axis`) and the cgo-free CLI (`cmd/ip-tool-for-axis-cli`). `Discover` combines Avahi (system D-Bus) and SSDP and emits partial updates from goroutines. Serialize `Registry` access; updates can re-key an IP-only entry when its serial becomes known.
- In the GUI, keep network work off the GTK thread and marshal results back with `ui()` (`glib.IdleAdd`); reuse `async()`/`withLogin()` in `actions.go`. ColumnView factories use `listCell`, not a `*gtk.ListItem` assertion. Refresh bound labels via `window.cells` rather than replacing model items on routine updates, so selection survives.
- In `internal/axis/vapix.go`, use `CheckLogin` to verify credentials: device-info endpoints can answer anonymously. Keep `context` in JSON requests for older firmware; a `pwdgrp.cgi` 401 in the factory-state fallback means `ErrSetupUnknown`, not “already configured.”
- My Axis sign-in loads WebKitGTK 6.0 at runtime (`cmd/ip-tool-for-axis/myaxis.go`); it is optional, not a build dependency.
- UI text, code comments, docs, and commit messages are in English. See `CLAUDE.md` for camera-specific compatibility notes (especially legacy optics and live view); use the code and workflow for current behavior.

## Packaging

- `make packages` uses `nfpm` to bundle both binaries into RPM/DEB/Arch packages in `dist/`; `make flatpak` uses `flatpak-builder`. Tagged CI renames artifacts to the fixed filenames consumed by `packaging/install.sh`; change both together. The AppStream screenshot URL is pinned to a release tag.
