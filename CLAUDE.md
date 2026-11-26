# CLAUDE.md – IP Tool for AXIS (Linux)

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
./build/ip-tool-for-axis-cli list -t 5
IP_TOOL_FOR_AXIS_SCREENSHOT=$PWD/data/screenshots/main-window.png ./build/ip-tool-for-axis   # README screenshot
```

GUI build dependencies: `gtk4-devel libadwaita-devel gobject-introspection-devel` (Fedora)
or `libgtk-4-dev libadwaita-1-dev libgirepository1.0-dev` (Debian/Ubuntu).
**The first GUI build takes 10–15 minutes** (cgo bindings for GTK); it's cached afterwards.
`ip-tool-for-axis-cli` is pure Go (`CGO_ENABLED=0`) and builds in seconds.
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
- `cmd/ip-tool-for-axis`: GUI. List = `gtk.StringList` of device keys → filter/sort models →
  `ColumnView`. Cells are updated live via `window.cells` (bound labels per key) without model
  changes (selection is kept). Status cell "Not logged in"/"Wrong password" is clickable → `unlock()`.
  Network calls always in goroutines, back to the GTK thread via `glib.IdleAdd`
  (`ui()`/`async()`/`withLogin()` in actions.go). Status strings are constants `axis.Status*`.
- `internal/axis/batch.go`: batch configuration. `PlanBatch` (IP range, skips used
  IPs/gateway/network/broadcast, hostname pattern: `#` number, `##` two digits, `@` last IP octet),
  `CheckAssignments` (after manual edits), `ParsePlanCSV` (English and German headers)/`ApplyPlan`,
  `AddressInUse` (TCP 80/443), `RunBatch` (sequential, IP last, errors don't stop the rest).
  GUI: `cmd/ip-tool-for-axis/batch.go` (wizard with `adw.NavigationView`), CLI: `ip-tool-for-axis-cli batch`.
- `cmd/ip-tool-for-axis-cli`: CLI with subcommands; password from `$AXIS_PASSWORD` or terminal prompt.
- Firmware update (modeled on github.com/Cacsjep/oadm, Apache-2.0 – only the approach, no code):
  `internal/axis/firmware.go`: catalog = public HTML listings `axis.com/ftp/pub_soft/{MPQT,PACS}/
  <Product>/<12_11_77>/` (regex parser, name/size/SHA-256), model match via normalized
  ProdNbr (exact, else longest prefix), `FirmwareChoices` (current track: LTS → same
  major.minor, else same major; latest LTS; latest). Download only with cookie
  `axis_connect_session_sid` (else 302 to login), SHA-256 check, cache by hash.
  **Own User-Agent on purpose:** axis.com serves .bin files to the default
  `Go-http-client` UA *without* login – don't rely on/exploit that.
  `fwmgr.go`: `firmwaremanagement.cgi` apiVersion 1.0 `status`/`upgrade` (multipart json+file,
  streamed via `sendBody`, status call first primes digest), `WaitForUpgrade` polls BasicInfo.
  Session stored in `~/.config/ip-tool-for-axis/myaxis-session.json` (0600) or `$AXIS_MYAXIS_SESSION`.
  GUI: `cmd/ip-tool-for-axis/firmware.go` (dialog for 1…n devices, list hint `fwHints` in the
  firmware column), `myaxis.go`: My Axis sign-in with **WebKitGTK 6.0 loaded via dlopen**
  (no build dependency; packages only "recommend" it), reads the cookie via the cookie manager.
  CLI: `ip-tool-for-axis-cli firmware`.
- Live view (`cmd/ip-tool-for-axis/liveview.go`): MJPEG via `Client.Stream` (`stream.go`, own
  multipart parser using Content-Length – `mime/multipart` delivers a frame only when the next
  boundary arrives; no 8 s client timeout), fallback snapshot polling on `ErrNotSupported`.
  Fixed sidebar (no toggle), capabilities are loaded together with the first snapshot
  (`loadPreview`) so it is there when the dialog opens. From `GetLensControls` (`ptz.go`):
  `ptz.cgi` (caps from `PTZ.Support.S1.*`, only if `Properties.PTZ.PTZ=yes`; without continuous
  zoom/focus step buttons via `rzoom`/`rfocus` or absolute `zoom`/`focus` + `query=position`) and `opticscontrol.cgi` (apiVersion `"1"`, zoom/focus/
  autofocus/`calibrate`; admin only, error 2105 = "Allow anonymous viewers" is on). Device
  commands go through one ordered worker queue so a stop never overtakes its start; hold
  buttons use `GestureDrag` (GestureClick emits "stopped" on long press). PTZ stop on close.

## Important decisions / pitfalls

- **gotk4 is pinned to the libadwaita 1.5 branch** (`gotk4-adwaita` branch `adw-1.5`,
  `gotk4 v0.3.2-0.20250703…`) so the build works on Ubuntu 24.04 (GTK 4.14, GLib 2.80).
  Newer gotk4 versions require GLib ≥ 2.86 and break there. Don't use Adw APIs > 1.5
  (e.g. no `Adw.Spinner`, `Adw.ToggleGroup`) – otherwise adapt CI/Ubuntu packages.
- CI builds with the **latest stable Go** (`setup-go` `go-version: stable`), not the `go 1.24.0`
  minimum from go.mod: govulncheck fails on the old standard library (net/http CVEs).
- In gotk4 the ColumnView factory delivers `*gtk.ColumnViewCell`, not `*gtk.ListItem` →
  use interface `listCell`. Compare widgets via `coreglib.InternObject(x).Native()`.
- gotk4 can't pass `GskRenderNode`s (not GObjects) → `screenshot.go` renders the window content
  via cgo/C API. GNOME blocks screenshots from other programs (also in a headless gnome-shell).
- `go.mod` contains `replace` directives to the GitHub mirrors of `golang.org/x/*` and
  `go4.org/…` (the development environment had no access to proxy.golang.org). They are
  harmless; remove them and run `go mod tidy` if you like.
- App ID: `io.github.reschcloud.IPToolForAXIS`, repo `github.com/reschcloud/ip-tool-for-axis-linux`
  (file names in `data/`, `packaging/`, constant in `main.go`, `APP_ID` in Makefile/install.sh).
  License: AGPL-3.0-or-later (since v0.3.0; before GPL-3.0-or-later).
- Packages: one package `ip-tool-for-axis` (GUI + CLI) for x86_64 and arm64. Release builds natively
  on Ubuntu 24.04 (`ubuntu-24.04` / `ubuntu-24.04-arm`, oldest glibc), so they also run on
  Fedora ≥ 40 and Arch. nfpm architecture comes from `$ARCH` (Makefile: `go env GOARCH`).
  Release files have fixed names (`ip-tool-for-axis.<arch>.rpm` etc.) that `install.sh` expects.
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
- **Motorized lens on older firmware** (P3225-VE Mk II, 9.80): no `opticscontrol.cgi` (404); the
  web UI uses `opticssetup.cgi` (found in its JS bundle): `monitor=poll` (XML `opticsSetupState`,
  `focusPosition`/`zoomPosition` 0…1, `*OperationInProgress`), `azoom`/`afocus=0…1`,
  `rzoom`/`rfocus=±minstep|±bigstep` (**literal `+`**, `%2B` → 400), `autofocus=perform`,
  `calibrate=focus,zoom`, `stop=focus|zoom`, `action=resetfocus`. Calibrate/autofocus block until
  done (~50 s) → always `&nonblocking=yes` and poll. `Optics.Legacy` selects this API; zoom is then
  a position 0…1 (slider in %), not a magnification.
  `ptz.cgi` on such fixed cameras is only **digital PTZ** of view areas: `PTZ.Support.S1.*`
  claims pan/tilt/zoom, but channel 1 (overview) answers "PTZ disabled" and an empty
  `query=position` → `PTZCapabilities` only reports PTZ if the position query returns values.
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
  polkit, `internal/arpping`, the old `axisctl assign` command). Re-implement if needed.

## Test status

Tested (Ubuntu 24.04 container, Xvfb, fake cameras as Python HTTP servers + `avahi-publish`):
- `go test ./internal/...`: VAPIX flow against a fake camera incl. digest auth, locked device
  info, NTP, snapshot, registry (link-local precedence, locked status, expiry), parsers, IPv6
  selection/netlink parsing, forwarder.
- Real cameras AXIS P3225-VE Mk II (firmware 9.80.49) and Q2101-TE (AXIS OS 11.11): discovery,
  initial password, locked detection, HTTPS detection. P3225 (2026-11-18): factory state
  detection, CLI `init` with power line frequency/mounting position, CLI `reset`, IPv6
  detection via neighbor table + forwarding to the link-local address (without GUI click),
  ping/status stable over several check cycles.
- `ip-tool-for-axis-cli list` finds devices announced via Avahi and enriches them.
- GUI: list, status, password dialog (creates user), network dialog loads the configuration;
  English UI rendered for the README screenshot.

**Not tested:** applying network/time settings in the GUI, NTP/preview on a real camera,
SSDP against real devices, `make packages` (nfpm) locally, Flatpak build, install.sh,
going offline (unplugging a camera) in the GUI, double-click IPv6 in the GUI.
Live view MJPEG stream, PTZ (`ptz.cgi`) and optics control/lens calibration
(`opticscontrol.cgi`) only against the fake camera – not yet on real devices.
P3225-VE Mk II (9.80.132, 2026-11-19): lens via `opticssetup.cgi` tested – detection, zoom
  (absolute/steps), focus, autofocus, calibration (~50 s) via the CLI's `ptz -zoom/-af/-calibrate`;
GUI live view on that device not yet clicked through.

## Open points / ideas

1. Test on more real cameras (ideally AXIS OS 12 and older firmware 9.x).
2. rpm installation/uninstallation tested on Fedora; install.sh and arm64 untested.
3. Optionally store passwords in the GNOME keyring (Secret Service/portal) instead of memory only.
4. Move the UI to Blueprint (`.blp`)? Currently built in code (gotk4 templates are cumbersome).
5. Batch configuration: implemented (v0.2.0), GUI wizard not yet run through on real cameras.
   Idea: preview image per device in the preview table; restart for several.
6. Firmware update implemented (see above) – untested on real cameras and with a real
   My Axis account (only fake camera/fake axis.com + real public listings; sign-in dialog only
   smoke-tested). (Factory reset implemented: `FactoryReset`, GUI "Reset", CLI `reset`;
   tested via CLI on P3225.) (NTP via `ntp.cgi` implemented, untested on P3225-VE Mk II.)
7. Translations (gettext) – currently English only (was German until v0.2.0).
