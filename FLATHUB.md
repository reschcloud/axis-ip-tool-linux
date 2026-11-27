# Flathub release plan

Status: the local Flatpak manifest in `packaging/io.github.reschcloud.iptoolforaxis.yml` builds a
bundle with `make flatpak`; the Flathub submission manifest is prepared in `packaging/flathub/`
with vendored Go modules. Remaining work is the release tag/commit pin, the sandbox test matrix
and the actual Flathub submission (target release: **v0.5.0**).

## 1. Release preparation (upstream)

### App ID (done)

The app ID was changed to lowercase reverse-DNS **before** the release:

- Flatpak ID, desktop file, AppStream ID and icon names:
  `io.github.reschcloud.iptoolforaxis` (the visible name stays "IP Tool for AXIS (Linux)").
- `APP_ID` in the `Makefile`, package scripts and CI artifact names were updated.
- The CLI/package name `ip-tool-for-axis` and the settings directory
  `~/.config/ip-tool-for-axis` stay unchanged.
- Existing local Flatpak installs (if any) use the old ID and need a reinstall; no Flathub
  release exists yet, so the impact is limited.

### Remaining preparation

- [x] Finish the pending fixes on `main` and run the automated verification (final live smoke test at tag time):
  - `make test`
  - `go test -race ./internal/... -count=1`
  - `make gui`, `make cli`
  - one live run: discovery, manual camera (hostname/URL/range), ping v4/v6, route, login,
    change password, anonymous live view, web interface, restart/reset dialogs (read-only
    where possible)
- [x] Bump the version to **0.5.0** in the AppStream release notes (already added), update
  `data/io.github.reschcloud.iptoolforaxis.metainfo.xml` screenshot URL to
  `…/v0.5.0/data/screenshots/main-window.png`.
- [x] Regenerate the screenshot with the current UI (1400×788, 16:9, current Route/Ping
  columns and action-bar styling). The developer screenshot works with
  `GDK_BACKEND=x11 GSK_RENDERER=cairo` when the Wayland renderer returns no node.
  The AppStream URL is pinned to `v0.5.0`.
- [x] Run metadata checks:
  - `desktop-file-validate data/*.desktop`
  - `appstreamcli validate --no-net data/*.metainfo.xml`
  - The desktop category and AppStream category now match (`Network;`).
- [ ] Confirm the icon meets Flathub expectations (scalable SVG is installed; optionally add a
  256×256 PNG export).
- [ ] Tag the release: `git tag -s v0.5.0`, push the tag, create the GitHub release with source
  tarball and its SHA256.

## 2. Make the Flatpak build offline-capable

Flathub builders have **no network**. The current manifest uses `type: dir` and
`build-args: --share=network`; both must go.

### Source

Replace the directory source with a pinned tag/commit:

```yaml
sources:
  - type: git
    url: https://github.com/reschcloud/ip-tool-for-axis-linux.git
    tag: v0.5.0
    commit: <tag commit sha>
```

(or `type: archive` with the released tarball and SHA256).

### Go modules (choose one)

**Recommended: generated module sources** (`flatpak-gomod-gen` from
`flathub/flatpak-builder-tools`) because the gotk4 dependencies are large. Add the generated
module list to the Flathub repo; set:

```yaml
build-options:
  append-path: /usr/lib/sdk/golang/bin
  env:
    GOPATH: /run/build/ip-tool-for-axis/go
    GOMODCACHE: /run/build/ip-tool-for-axis/go/pkg/mod
    GOCACHE: /run/build/ip-tool-for-axis/go-cache
    GOTOOLCHAIN: local
    GOPROXY: "off"
```

**Alternative: vendor in the release tag.** Run `go mod vendor`, commit `vendor/` (or include
it only in the release tarball), and set `GOFLAGS: -mod=vendor`, `GOPROXY: off`.
This is simpler but adds a large vendor tree to the repository/release.

Remove `build-options.build-args: --share=network`. The build must succeed with
`flatpak-builder --disable-download`.

### Manifest cleanup

- [x] Use the exact runtime branch available on Flathub (verified `org.gnome.Platform//51`; the Go
  SDK extension branch must match).
- [x] Keep `command: ip-tool-for-axis`.
- [x] Keep the finish args, with justification:
  - `--share=network` (cameras, SSDP, DNS, firmware)
  - `--system-talk-name=org.freedesktop.Avahi` (mDNS via the system Avahi service)
  - `--share=ipc`, `--socket=wayland`, `--socket=fallback-x11`, `--device=dri`
- [ ] Test whether opening the web interface works through the XDG desktop portal (GTK
  `URILauncher`). Usually no extra permission is needed; add a portal talk-name only if the
  test shows otherwise.
- [ ] Verify `LICENSE` is installed to `/app/share/licenses/io.github.reschcloud.iptoolforaxis/`
  (added to `make install-app`).
- [ ] Verify ping works inside the sandbox (unprivileged ICMP). The app already falls back to
  HTTP if ICMP is blocked; document that in the app/README if the Flatpak environment blocks it.
- [x] WebKitGTK 6.0 availability in the runtime: `org.gnome.Platform//51` ships
  `/usr/lib/x86_64-linux-gnu/libwebkitgtk-6.0.so.4` (and the WebKitWebDriver), so the
  embedded My Axis sign-in works without bundling WebKit. Re-check on a runtime bump.
- [x] Confirm the CLI note: inside the Flatpak only `flatpak run --command=ip-tool-for-axis-cli
  io.github.reschcloud.iptoolforaxis …` is available; do not change the host README install
  instructions until the app is accepted.
- [ ] Add the Flathub manifest to a dedicated repository
  `flathub/io.github.reschcloud.iptoolforaxis` (or the new-app submission branch), never mix it
  with the local `type: dir` manifest.

## 3. Local Flathub-equivalent test

Build in a clean state, without network:

```sh
rm -rf build/flathub-flathub
flatpak-builder --force-clean --disable-download --repo=build/flathub-repo \
  build/flathub-flathub packaging/flathub/io.github.reschcloud.iptoolforaxis.yml
flatpak build-update-repo build/flathub-repo
flatpak build-bundle build/flathub-repo /tmp/ip-tool.flatpak \
  io.github.reschcloud.iptoolforaxis
flatpak install --user -y /tmp/ip-tool.flatpak
```

Then run `flatpak run io.github.reschcloud.iptoolforaxis` and test the sandbox matrix:

- [ ] mDNS/Avahi discovery
- [ ] SSDP discovery (multicast)
- [ ] Ping v4 and IPv6 fallback; manual hostname/URL/range
- [ ] Route column (🌐) and resolved IPv4/IPv6/interface
- [ ] Login, wrong password, factory state, change password
- [ ] Anonymous live view (if enabled on the camera) and authenticated live view
- [ ] Open website via the default browser/portal
- [ ] Network/time/NTP/restart/reset confirmation dialogs
- [ ] Firmware dialog: file chooser (portal), axis.com download, My Axis sign-in (or the
  documented fallback)
- [ ] Settings and manual-camera list survive a restart (inside `~/.var/app/…`)
- [ ] CLI via `flatpak run --command=ip-tool-for-axis-cli … list`
- [ ] `flatpak-builder-lint` (Flathub linter) passes

## 4. Submission

1. Confirm the Flathub app ID is free (`io.github.reschcloud.iptoolforaxis`) and that the
   GitHub user `reschcloud` owns the project. The ID follows the required reverse-DNS pattern.
2. Open a new-app request / PR in the Flathub process for `flathub/flathub`
   (new-app template) with:
   - app ID, name, summary, license (AGPL-3.0-or-later), homepage, screenshots,
   - the manifest and generated Go module sources,
   - a short note that `--share=network` and Avahi system D-Bus access are required for
     local network camera discovery,
   - the trademark disclaimer ("Not an official product of Axis Communications AB").
3. Address the Flathub review:
   - no network during build; pinned sources and checksums,
   - no `type: dir`, no `--share=network` build arg,
   - `flatpak-builder-lint` clean,
   - AppStream/desktop validation clean,
   - screenshot aspect ratio and URL reachable,
   - permission justification.
4. After merge, test the official install:
   `flatpak install flathub io.github.reschcloud.iptoolforaxis`
5. Update the README/manual install section with the Flathub command (keep the package/install.sh
   instructions for distributions), and re-check the WebKitGTK 6.0 availability on runtime bumps.
   It is present in `org.gnome.Platform//51`.

## 5. Open questions / blockers

- Flathub review of `--share=network` and `--system-talk-name=org.freedesktop.Avahi`
  (both are necessary for this app; prepare the justification).
- Runtime branch and Go SDK extension version must match the Flathub runtime at submission time.
- Fill the `commit:` placeholder in `packaging/flathub/io.github.reschcloud.iptoolforaxis.yml`
  after tagging v0.5.0 (`packaging/flathub/prepare.sh v0.5.0`).
- The vendored Go modules (21 MB) are part of the release source; keep `go mod vendor` in the
  release checklist whenever dependencies change.

## 6. Suggested timeline

| Phase | Work | Exit criterion |
|---|---|---|
| 1 | Finish fixes, tests, metainfo 0.5.0, tag | `make test`, race tests green, tag pushed |
| 2 | Flathub manifest with vendored Go modules | `flatpak-builder --disable-download` succeeds |
| 3 | Sandbox test matrix | all core features work (incl. WebKitGTK/My Axis) |
| 4 | Submit and review | Flathub CI/linter green, reviewer approval |
| 5 | Post-release | `flatpak install flathub …` tested, README updated |
