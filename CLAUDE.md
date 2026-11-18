# CLAUDE.md – Axis IP Tool

GNOME app + CLI for finding Axis cameras and doing their basic configuration (model: AXIS IP Utility).
Language for UI texts, code comments, docs and commit messages: **English** (switched from German
in v0.3.0). The user talks to Claude in German.

## Commands

```sh
make                 # build everything -> build/
make test            # go vet + go test (no GUI, fast)
make gui|cli         # single binaries
sudo make install    # to /usr
make packages        # rpm/deb/arch into dist/ (needs nfpm)
make flatpak         # Flatpak bundle into dist/
./build/axisctl list -t 5
AXIS_IP_TOOL_SCREENSHOT=$PWD/data/screenshots/main-window.png ./build/axis-ip-tool   # README screenshot
```

GUI build dependencies: `gtk4-devel libadwaita-devel gobject-introspection-devel` (Fedora)
or `libgtk-4-dev libadwaita-1-dev libgirepository1.0-dev` (Debian/Ubuntu).
**The first GUI build takes 10–15 minutes** (cgo bindings for GTK); it's cached afterwards.
`axisctl` is pure Go (`CGO_ENABLED=0`) and builds in seconds.
Caution: `go build` without `-trimpath` has its own cache → GTK bindings get rebuilt.
Always use `make gui` for quick checks.

## Architecture

- `internal/axis` – everything device-specific, no GUI dependency:
  - `discovery.go`: `Discover(ctx, found, warn)` – Avahi via D-Bus (`_axis-video._tcp`,
    `_vapix-http._tcp`) + SSDP M-SEARCH; every result is enriched via
    `basicdeviceinfo.cgi`/`systemready.cgi`. Callbacks come from goroutines. `monitor` pings.
  - `registry.go`: merges partial results (key = serial number, otherwise IP; SSDP entries
    without serial are re-keyed). Used by CLI and GUI.
  - `vapix.go`: `Client` with its own digest auth (`digest.go`, MD5 + SHA-256), JSON APIs with
    fallbacks to `param.cgi`/`date.cgi` for old firmware. `ErrAuth` signals "log in again".
    Protocol: `ProbeHTTPS` (TLS handshake on 443, cached for `CheckInterval`) → HTTPS, otherwise
    HTTP port; `SetHTTPSOnly(true)` → `ErrNoHTTPS` instead of fallback. Certificates are
    deliberately NOT verified and not pinned either (user's decision).
- `cmd/axis-ip-tool`: GUI. List = `gtk.StringList` of device keys → filter/sort models →
  `ColumnView`. Cells are updated live via `window.cells` (bound labels per key) without model
  changes (selection is kept). Status cell "Not logged in"/"Wrong password" is clickable → `unlock()`.
  Network calls always in goroutines, back to the GTK thread via `glib.IdleAdd`
  (`ui()`/`async()`/`withLogin()` in actions.go). Status strings are constants `axis.Status*`.
- `internal/axis/batch.go`: batch configuration. `PlanBatch` (IP range, skips used
  IPs/gateway/network/broadcast, hostname pattern: `#` number, `##` two digits, `@` last IP octet),
  `CheckAssignments` (after manual edits), `ParsePlanCSV` (English and German headers)/`ApplyPlan`,
  `AddressInUse` (TCP 80/443), `RunBatch` (sequential, IP last, errors don't stop the rest).
  GUI: `cmd/axis-ip-tool/batch.go` (wizard with `adw.NavigationView`), CLI: `axisctl batch`.
- `cmd/axisctl`: CLI with subcommands; password from `$AXIS_PASSWORD` or terminal prompt.

## Important decisions / pitfalls

- **gotk4 is pinned to the libadwaita 1.5 branch** (`gotk4-adwaita` branch `adw-1.5`,
  `gotk4 v0.3.2-0.20250703…`) so the build works on Ubuntu 24.04 (GTK 4.14, GLib 2.80).
  Newer gotk4 versions require GLib ≥ 2.86 and break there. Don't use Adw APIs > 1.5
  (e.g. no `Adw.Spinner`, `Adw.ToggleGroup`) – otherwise adapt CI/Ubuntu packages.
- In gotk4 the ColumnView factory delivers `*gtk.ColumnViewCell`, not `*gtk.ListItem` →
  use interface `listCell`. Compare widgets via `coreglib.InternObject(x).Native()`.
- gotk4 can't pass `GskRenderNode`s (not GObjects) → `screenshot.go` renders the window content
  via cgo/C API. GNOME blocks screenshots from other programs (also in a headless gnome-shell).
- `go.mod` contains `replace` directives to the GitHub mirrors of `golang.org/x/*` and
  `go4.org/…` (the development environment had no access to proxy.golang.org). They are
  harmless; remove them and run `go mod tidy` if you like.
- App ID: `io.github.reschcloud.AxisIpTool`, repo `github.com/reschcloud/axis-ip-tool-linux`
  (file names in `data/`, `packaging/`, constant in `main.go`, `APP_ID` in Makefile/install.sh).
  License: AGPL-3.0-or-later (since v0.3.0; before GPL-3.0-or-later).
- Packages: one package `axis-ip-tool` (GUI + CLI) for x86_64 and arm64. Release builds natively
  on Ubuntu 24.04 (`ubuntu-24.04` / `ubuntu-24.04-arm`, oldest glibc), so they also run on
  Fedora ≥ 40 and Arch. nfpm architecture comes from `$ARCH` (Makefile: `go env GOARCH`).
  Release files have fixed names (`axis-ip-tool.<arch>.rpm` etc.) that `install.sh` expects.
  openSUSE: dependency package names don't match (nfpm rpm uses Fedora names) → build from source.
  Metainfo screenshot URL points to the tag (`raw.githubusercontent.com/…/vX.Y.Z/…`) – update per release.
- Some firmware (seen on P3225-VE Mk II, 9.80.49 – there already in factory state) answers
  `basicdeviceinfo.cgi` only with login (401). The model then comes from the mDNS name; the GUI
  loads the firmware via `refreshInfo()` once credentials are known (`getAllProperties`).
- Detect factory state via `systemready.cgi` (`needsetup`, no login). AXIS OS 11 answers
  `pwdgrp.cgi?action=get` with 401 even in factory state – the old heuristic (like AXIS IP
  Utility) is only a fallback for old firmware. `ErrNoUser`/`ErrHasUser`.
  **`jsonCall` always sends `"context"`**: P3225-VE Mk II (firmware 9.80.49) rejects
  `systemready.cgi` without it with "Invalid POST data". A heuristic 401 only means "unknown"
  (`ErrSetupUnknown`), not "has a user". This firmware also locks `basicdeviceinfo.cgi`/
  `apidiscovery.cgi` (401) in factory state.
- Commissioning (`setup.go`, `ApplyInitialSetup`): power line frequency
  `ImageSource.I<n>.CaptureFrequency` (`50Hz`/`60Hz`) + `System.CaptureFrequencySet=yes`, mounting
  position `ImageSource.I<n>.CameraTiltOrientation` (`-90` ceiling, `0` wall, `90` desk) – like the
  web interface wizard. Missing parameters are skipped (`SettingPowerFrequency`/`SettingMount`).
  Default 50 Hz/ceiling, the GUI remembers the last choice in `settings.json`; CLI `init`/`batch -init`
  with `-freq`/`-mount` (`ceiling|wall|desk|none`).
- **Background monitoring**: ping of all devices every second (`ping.go`, unprivileged ICMP
  v4/v6, `discovery.go/monitor`); everything else every `CheckInterval` (10 s): re-query (`enrich`),
  IPv6 neighbors (`IPv6Full` replaces the list), SSDP, HTTPS cache, GUI login check (`refreshInfo`).
  `Device.LastSeen` = last reply; `Registry.Expire` (GUI tick 1 s) sets offline after `MaxAge` (10 s)
  – devices that never answered a ping (`Pingable`) only after 25 s.
  `Merge` only sets `Online` to true (`u.Online`) or false on `u.Gone` (mDNS sign-off).
  **No status flicker:** spinner in the status cell only on the first check or user actions;
  repetitions run silently (`infoRunning` instead of `infoPending`). The header bar shows
  "Discovery running" (spinner, tooltip with last reply). Pure ping updates only re-render the row.
- IPv6 (`ipv6.go`): Axis mostly announces only IPv4 via mDNS (AAAA is tried anyway). Discovery
  therefore pings `ff02::1` every 10 s on every interface with devices (unprivileged ICMPv6
  socket) and reads the neighbor table via netlink (`syscall.NetlinkRIB`, no root); matched by
  MAC = serial number. Display `PickIPv6`: link-local before others, with several the EUI-64 from
  the MAC. `Device.Iface` = interface of the discovery (Avahi index or IPv4 network for SSDP).
  Browsers don't open `fe80::…%iface` URLs → the GUI forwards a local port (127.0.0.1) via
  `axis.Forwarder` to `[fe80::…%iface]:443/80` (double-click on the IPv6 column).
- mDNS `_vapix-http._tcp` has no MAC suffix in the name on some devices → serial number from
  TXT `sn=` (otherwise duplicate entry without serial number).
- Verify credentials only via `CheckLogin` (`pwdgrp.cgi?action=get`, always requires admin) –
  `basicdeviceinfo.cgi`/`param.cgi` may answer anonymously depending on the device, i.e. also with
  a wrong password. Status: "Not logged in" (also locked devices, `IsLocked` only in the tooltip)
  → "Logged in" (`Unlocked`) or "Wrong password" (`AuthFailed`, set on every `ErrAuth`, GUI:
  `window.setAuth`).
- **ARP/ping IP assignment was removed for now on 2026-11-17** (system helper, D-Bus service,
  polkit, `internal/arpping`, `axisctl assign`). Re-implement if needed.

## Test status

Tested (Ubuntu 24.04 container, Xvfb, fake cameras as Python HTTP servers + `avahi-publish`):
- `go test ./internal/...`: VAPIX flow against a fake camera incl. digest auth, locked device
  info, NTP, snapshot, registry (link-local precedence, locked status, expiry), parsers, IPv6
  selection/netlink parsing, forwarder.
- Real cameras AXIS P3225-VE Mk II (firmware 9.80.49) and Q2101-TE (AXIS OS 11.11): discovery,
  initial password, locked detection, HTTPS detection. P3225 (2026-11-18): factory state
  detection, `axisctl init` with power line frequency/mounting position, `axisctl reset`, IPv6
  detection via neighbor table + forwarding to the link-local address (without GUI click),
  ping/status stable over several check cycles.
- `axisctl list` finds devices announced via Avahi and enriches them.
- GUI: list, status, password dialog (creates user), network dialog loads the configuration;
  English UI rendered for the README screenshot.

**Not tested:** applying network/time settings in the GUI, NTP/preview on a real camera,
SSDP against real devices, `make packages` (nfpm) locally, Flatpak build, install.sh,
going offline (unplugging a camera) in the GUI, double-click IPv6 in the GUI.

## Open points / ideas

1. Test on more real cameras (ideally AXIS OS 12 and older firmware 9.x).
2. rpm installation/uninstallation tested on Fedora; install.sh and arm64 untested.
3. Optionally store passwords in the GNOME keyring (Secret Service/portal) instead of memory only.
4. Move the UI to Blueprint (`.blp`)? Currently built in code (gotk4 templates are cumbersome).
5. Batch configuration: implemented (v0.2.0), GUI wizard not yet run through on real cameras.
   Idea: preview image per device in the preview table; restart for several.
6. Firmware update. (Factory reset implemented: `FactoryReset`, GUI "Reset", `axisctl reset`;
   tested via CLI on P3225.) (NTP via `ntp.cgi` implemented, untested on P3225-VE Mk II.)
7. Translations (gettext) – currently English only (was German until v0.2.0).
