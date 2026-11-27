# IP Tool for AXIS (Linux) – agent guide

GNOME app + CLI for finding Axis cameras and doing their basic configuration (model: AXIS IP
Utility). Repository artifacts (UI text, comments, docs, ADRs, commit messages) are in **English**;
the maintainer may talk to the agent in German.

**This file is the single source of truth for agents.** `CLAUDE.md` only links here. Architecture
principles are recorded as ADRs in [`docs/adr/`](docs/adr/README.md).

## Commands and build

```sh
make                 # build everything -> build/
make test            # go vet + go test (no GUI, fast)
make gui|cli         # single binaries
sudo make install    # to /usr
make packages        # rpm/deb/arch into dist/ (needs nfpm)
make flatpak         # Flatpak bundle into dist/
./build/ip-tool-for-axis-cli list -t 5
IP_TOOL_FOR_AXIS_SCREENSHOT=$PWD/data/screenshots/main-window.png ./build/ip-tool-for-axis
```

- Start with `make test`: it vets `./internal/... ./cmd/ip-tool-for-axis-cli` and tests
  `./internal/...`. It does **not** compile the GUI. The default suite uses loopback fixtures, not
  cameras or Avahi; run one workflow with `go test ./internal/axis -run '^TestWorkflow$' -count=1`.
  The live catalog check is opt-in: `AXIS_ONLINE_TEST=1 go test ./internal/axis -run
  '^TestFirmwareCatalogOnline$' -count=1` (contacts axis.com).
- GUI build deps: `gtk4-devel libadwaita-devel gobject-introspection-devel` (Fedora) or
  `libgtk-4-dev libadwaita-1-dev libgirepository1.0-dev` (Debian/Ubuntu). The first GUI build
  takes 10–15 minutes (cgo GTK bindings); it is cached afterwards. The CLI is pure Go
  (`CGO_ENABLED=0`) and builds in seconds.
- Keep `-trimpath` when building manually; a plain `go build` has a separate cache and rebuilds
  the expensive GTK bindings. Use `make gui` for quick checks.
- CI uses the latest stable Go, not the `go.mod` minimum, for a patched stdlib and govulncheck.
  The GUI compatibility floor is Ubuntu 24.04 (GTK 4.14, GLib 2.80, libadwaita 1.5); keep the
  gotk4 pins and avoid newer libadwaita APIs unless the floor changes.
- For desktop metadata changes, validate with `desktop-file-validate data/*.desktop` and
  `appstreamcli validate --no-net data/*.metainfo.xml`.
- The screenshot developer mode needs `GDK_BACKEND=x11 GSK_RENDERER=cairo` on some Wayland/GTK
  setups; otherwise the renderer may return no node. Use 1400×788 (16:9) for Flathub.
- Go modules are vendored (`vendor/`); offline builds use `GOFLAGS=-mod=vendor GOPROXY=off`.
  Re-run `go mod vendor` when dependencies change.

## Architecture

`internal/axis` contains everything device-specific and has no GUI dependency:

- `discovery.go`: `Discover(ctx, found, warn)` – Avahi via D-Bus (`_axis-video._tcp`,
  `_vapix-http._tcp`) + SSDP M-SEARCH; every result is enriched via
  `basicdeviceinfo.cgi`/`systemready.cgi`. Callbacks come from goroutines. `monitor` pings and
  resolves hostnames.
- `registry.go`: merges partial results (key = serial number, otherwise address incl. non-default
  port; SSDP entries without serial are re-keyed). Used by CLI and GUI.
- `vapix.go`: `Client` with its own digest auth (`digest.go`, MD5 + SHA-256), JSON APIs with
  fallbacks to `param.cgi`/`date.cgi` for old firmware. `ErrAuth` means "log in again";
  `CheckLogin` is the only credential check.
- `manual.go`: parsing/expansion of manual devices (addresses, hostnames, URLs, CIDR, ranges,
  wildcards; comma/semicolon/whitespace separated). `localnet.go`: local/routed classification and
  interface lookup.
- `batch.go`: batch configuration. `PlanBatch` (IP range, skips used IPs/gateway/network/
  broadcast; hostname pattern `#`/`##`/`@`), `CheckAssignments`, `ParsePlanCSV` (English/German
  headers)/`ApplyPlan`, `AddressInUse`, `RunBatch` (sequential, IP last, errors don't stop others).
- `firmware.go` / `fwmgr.go`: axis.com catalog (public listings, SHA-256, track choices; download
  needs the My Axis session cookie), `firmwaremanagement.cgi` upgrade and `WaitForUpgrade`.
  Own User-Agent on purpose; do not rely on the default Go UA being served without login.
- `ptz.go` / `stream.go`: PTZ/optics APIs and the MJPEG stream (own multipart parser using
  Content-Length; no 8 s client timeout; snapshot polling fallback).

`cmd/ip-tool-for-axis` is the GTK4/libadwaita GUI; `cmd/ip-tool-for-axis-cli` is the CLI with
subcommands. Both use the same core types.

## Invariants and pitfalls

- Keep device logic in `internal/axis`; do not add GTK imports there. The CLI must stay cgo-free.
- `Discover` emits partial updates from goroutines. Serialize `Registry` access; updates can
  re-key an address-only entry when its serial becomes known. See ADR 0002.
- In the GUI, keep network/DNS/ICMP work off the GTK thread and marshal results back with `ui()`
  (`glib.IdleAdd`); reuse `async()`/`withLogin()` in `actions.go`. ColumnView factories use the
  `listCell` interface, not a `*gtk.ListItem` assertion. Refresh bound labels via `window.cells`
  instead of replacing model items on routine updates, so selection survives. See ADR 0003.
- Login/action gating: when a user exists but no valid login is known, actions show a lock and open
  the login first; after login the original action continues. Open website and live view are
  exempt; live view tries anonymous access first. Guard against duplicate login dialogs.
- `CheckLogin` (not device-info) verifies credentials: device-info endpoints can answer
  anonymously even with a wrong password. Keep `context` in JSON requests for older firmware; a
  heuristic `pwdgrp.cgi` 401 means `ErrSetupUnknown`, not "already configured".
- Distinguish `ErrNoUser` from `ErrAuth` before showing "wrong password"; handle `ErrHasUser` by
  switching to login. See ADR 0004.
- HTTPS is probed on 443 (cached for `CheckInterval`); fall back to HTTP unless HTTPS-only.
  Certificates are deliberately not verified or pinned. A manual URL's scheme/port wins. Warn
  before sending passwords over HTTP. See ADR 0005.
- Background monitoring: ping every 2 s (unprivileged ICMP v4/v6, IPv4 first, IPv6 fallback for
  hostnames), re-query every `CheckInterval` (10 s), IPv6 neighbors/SSDP every 10 s. Offline after
  `MaxAge` (10 s) or 25 s for devices that never answer pings. `Merge` only sets `Online` true or
  false on `Gone`; `Registry.Expire` clears it. Avoid status flicker: spinner only on first check or
  user actions, routine updates silent (`infoRunning` vs `infoPending`).
- IPv6: Axis often announces only IPv4 via mDNS. `ipv6.go` pings `ff02::1` on interfaces with
  devices and reads the neighbor table via netlink (no root), matched by MAC = serial. Prefer
  link-local (EUI-64 from MAC when possible). Browsers cannot open `fe80::…%iface` URLs; the GUI
  forwards a local port via `axis.Forwarder`. Keep the zone out of URLs.
- `mDNS _vapix-http._tcp` may lack the MAC suffix; take the serial from TXT `sn=`.
- gotk4 is pinned to the libadwaita 1.5 branch (`gotk4-adwaita` `adw-1.5`, gotk4
  `v0.3.2-0.20250703…`) so Ubuntu 24.04 (GTK 4.14/GLib 2.80) works. Newer gotk4 requires GLib
  ≥ 2.86. Do not use Adw APIs > 1.5 (e.g. no `Adw.Spinner`, `Adw.ToggleGroup`).
- gotk4 cannot pass `GskRenderNode`s (not GObjects); `screenshot.go` renders the window via cgo/C.
  GNOME blocks screenshots from other programs.
- The `go.mod` `replace` directives point to GitHub mirrors of `golang.org/x/*` and `go4.org/…`
  (the development environment had no proxy access). They are harmless.
- App ID: `io.github.reschcloud.iptoolforaxis` (lowercase) in `main.go`, `APP_ID` in the Makefile/
  install.sh and file names in `data/`/`packaging/`. License: AGPL-3.0-or-later (GPL-3.0 before
  v0.3.0). The tool is unofficial; do not ship Axis logos.
- Settings and the manual-device list are stored in `~/.config/ip-tool-for-axis/settings.json`
  (atomic write). The My Axis session lives in
  `~/.config/ip-tool-for-axis/myaxis-session.json` (0600) or `$AXIS_MYAXIS_SESSION`.

## Camera compatibility notes

- **Factory state**: detect via `systemready.cgi` (`needsetup`, no login). AXIS OS 11 answers
  `pwdgrp.cgi?action=get` with 401 even in factory state; the old heuristic is only a fallback.
  P3225-VE Mk II (9.80.49) rejects `systemready.cgi` without `"context"` ("Invalid POST data");
  `jsonCall` always sends it. This firmware also locks `basicdeviceinfo.cgi`/`apidiscovery.cgi` in
  factory state, so the model may come from mDNS until login.
- **Locked device info**: some firmware answers `basicdeviceinfo.cgi` only with login (401). The
  GUI loads firmware/model via `refreshInfo()` once credentials are known.
- **Commissioning** (`setup.go`, `ApplyInitialSetup`): power line frequency
  `ImageSource.I<n>.CaptureFrequency` (`50Hz`/`60Hz`) + `System.CaptureFrequencySet=yes`; mounting
  position `ImageSource.I<n>.CameraTiltOrientation` (`-90` ceiling, `0` wall, `90` desk). Missing
  parameters are skipped (`SettingPowerFrequency`/`SettingMount`). Defaults: 50 Hz/ceiling; the
  GUI remembers the last choice, CLI `init`/`batch -init` use `-freq`/`-mount`.
- **Live view**: MJPEG via `Client.Stream` (own multipart parser; fallback to snapshot polling on
  `ErrNotSupported`). Fixed sidebar; capabilities loaded with the first snapshot so they are
  available when the dialog opens. Lens info from `ptz.cgi` (caps `PTZ.Support.S1.*`, only if
  `Properties.PTZ.PTZ=yes`) and `opticscontrol.cgi` (apiVersion `"1"`; admin only; error 2105 means
  "Allow anonymous viewers" is on).
- **Motorized lens on old firmware** (P3225-VE Mk II, 9.80): no `opticscontrol.cgi`; use
  `opticssetup.cgi`: `monitor=poll` (XML state), `azoom`/`afocus=0…1`, `rzoom`/`rfocus=±minstep|
  ±bigstep` (literal `+`; `%2B` → 400), `autofocus=perform`, `calibrate=focus,zoom`,
  `stop=focus|zoom`, `action=resetfocus`. Calibrate/autofocus block (~50 s); always send
  `&nonblocking=yes` and poll. `Optics.Legacy` selects this API; zoom is a 0…1 position.
  `ptz.cgi` on fixed cameras is only digital PTZ of view areas: `PTZ.Support.S1.*` may claim
  support, but channel 1 (overview) answers "PTZ disabled"; only report PTZ when the position
  query returns values.
- Device commands go through one ordered worker queue so a stop never overtakes its start; hold
  buttons use `GestureDrag` (GestureClick emits "stopped" on long press). PTZ stop on close.
- **Firmware / My Axis**: catalog from public `axis.com/ftp/pub_soft/{MPQT,PACS}/<Product>/<ver>/`
  listings; model match via normalized ProdNbr (exact, else longest prefix); `FirmwareChoices`
  (current track: LTS stays on major.minor; latest LTS/latest). Download requires the
  `axis_connect_session_sid` cookie (else 302 to login); SHA-256 checked; cache by hash.
  `firmwaremanagement.cgi` 1.0 `status`/`upgrade` (multipart json+file, streamed; status call
  primes digest); `WaitForUpgrade` polls BasicInfo. The GUI embeds the My Axis sign-in with
  WebKitGTK 6.0 via dlopen (optional; packages only recommend it); CLI uses the same session.
- **ARP/ping IP assignment** was removed on 2026-11-17 (helper, D-Bus service, polkit,
  `internal/arpping`, old `axisctl assign`). Re-implement if needed.

## Manual devices and displayed information

Manual input supports addresses (IPv4/IPv6, link-local with `%iface`), hostnames, complete
`http(s)://` URLs, CIDR prefixes, ranges and last-octet wildcards, in mixed lists separated by
commas, semicolons and/or whitespace. Expand atomically, cap at 1024 addresses, reject host-bit
CIDRs/loopback/broadcast/malformed URLs, and keep hostnames lowercased. Hostnames are resolved in
the background; the table shows resolved IPv4/IPv6 and the outgoing interface, the Route column
(🌐) distinguishes local vs routed per family, and the Ping tooltip names the address (IPv4/IPv6)
used. DNS failures show "Hostname not resolved"; reachable non-VAPIX targets show "No VAPIX
response". See ADR 0006.

## Testing status

- Automated: VAPIX flow against a fake camera (digest auth, locked info, NTP, snapshot, registry,
  link-local precedence, expiry, parsers, IPv6 selection/netlink, forwarder), manual input/ranges,
  ping fallback, password change, discovery/registry races (`go test -race ./internal/...`).
- Real cameras used during development: AXIS P3225-VE Mk II (9.80.x) and Q2101-TE (AXIS OS 11.11),
  plus an AXIS I8116-E (12.11.x) for anonymous live view/hostname/route/status checks.
- **Not fully tested**: applying network/time settings in the GUI on real cameras, NTP/preview on
  all models, SSDP against many real devices, `make packages`/Flatpak build/install.sh in this
  environment, GUI offline transitions, IPv6 double-click in the GUI, My Axis sign-in with a real
  account. Live view/PTZ/optics were tested via the fake camera and partly via CLI on a P3225.
- Open ideas: test more AXIS OS 12 and 9.x devices, GNOME keyring for credentials, Blueprint for
  the UI, batch GUI on real cameras, gettext translations (currently English only).

## Packaging and release

- One package `ip-tool-for-axis` (GUI + CLI) for x86_64/arm64. Release builds natively on Ubuntu
  24.04 (`ubuntu-24.04`/`ubuntu-24.04-arm`, oldest glibc) so they also run on Fedora ≥ 40/Arch.
  nfpm architecture comes from `$ARCH` (`go env GOARCH`). Fixed release filenames
  (`ip-tool-for-axis.<arch>.rpm` etc.) are consumed by `packaging/install.sh`; change both together.
  openSUSE dependency names differ → build from source.
- `make packages` uses `nfpm` for RPM/DEB/Arch in `dist/`; `make flatpak` uses `flatpak-builder`.
  The AppStream screenshot URL and release notes are pinned to the release tag.
- Flathub preparation: lowercase app ID, vendored Go modules, offline build
  (`GOFLAGS=-mod=vendor`, `GOPROXY=off`), license in `/app/share/licenses/<app-id>/`, separate
  Flathub manifest in `packaging/flathub/`, screenshot 16:9. See [`FLATHUB.md`](FLATHUB.md) and
  ADR 0007 for the checklist and remaining submission steps.

## Architecture decisions

Read the ADRs before changing fundamental behavior:

- [0001 – shared device core](docs/adr/0001-shared-device-core.md)
- [0002 – discovery, monitoring, registry](docs/adr/0002-discovery-and-monitoring.md)
- [0003 – GTK thread model and login gating](docs/adr/0003-gui-thread-and-model.md)
- [0004 – VAPIX compatibility and credentials](docs/adr/0004-vapix-compatibility-and-auth.md)
- [0005 – HTTPS, IPv6 and forwarding](docs/adr/0005-https-and-ipv6.md)
- [0006 – manual devices and input](docs/adr/0006-manual-devices-and-input.md)
- [0007 – packaging, release and Flathub](docs/adr/0007-packaging-release-flathub.md)
- [0008 – language, branding, license](docs/adr/0008-language-branding-license.md)
