# IP Tool for AXIS (Linux)

GNOME app and command line tool for finding Axis network cameras and doing their basic
configuration – modeled on the **AXIS IP Utility**.
Not an official product of Axis Communications AB.

![Main window: discovered cameras with model, serial number, hostname, IPv4/IPv6 address, interface, firmware, ping and login status](data/screenshots/main-window.png)

## Features

| Feature | Implementation |
|---|---|
| Discover devices | Bonjour/mDNS via Avahi (D-Bus) + SSDP/UPnP |
| Add camera manually | address, hostname, http(s) URL, CIDR or range; monitored like a discovered device |
| Device behind a router | 🌐 in the "Route" column when the address is outside the directly attached networks |
| Continuous monitoring | ping every 2 s (IPv4 first, IPv6 fallback), query every 10 s; offline after 10 s without a reply (25 s if it never answers pings) |
| Model, serial number, firmware | `basicdeviceinfo.cgi` (without login, with login for locked devices) |
| IPv6 address and interface | Avahi + kernel neighbor table; link-local preferred, EUI-64 from the MAC |
| Open web interface via IPv6 | link-local addresses via a local forwarding (browsers can't open `fe80::…%iface` URLs) |
| Detect factory state / initial password | `systemready.cgi`, `pwdgrp.cgi` |
| Change an existing password | `pwdgrp.cgi` after login; all actions show a lock and open the login first |
| Commissioning: power line frequency, mounting position | `param.cgi` (50/60 Hz; ceiling/wall/desk) |
| IPv4 (DHCP/static), hostname, DNS | `network_settings.cgi`, fallback `param.cgi` |
| Time zone + time | `time.cgi`, fallback `date.cgi` |
| NTP servers | `ntp.cgi`, fallback `param.cgi` |
| Live view | `mjpg/video.cgi` (MJPEG stream, fallback `jpg/image.cgi` 1 image/s) |
| PTZ: pan/tilt, zoom, focus, presets, click to center | `com/ptz.cgi` |
| Motorized lens: zoom, focus, autofocus, lens calibration | `opticscontrol.cgi` (Optics Control API) |
| Several devices at once | password, sequential IPs, hostname pattern, time/NTP; CSV plan; log |
| Open website, restart | default browser, `restart.cgi` |
| Factory defaults (optionally including IP) | `factorydefault.cgi` / `hardfactorydefault.cgi` |
| Firmware: compare with axis.com, download, update (several devices) | AXIS OS listings on axis.com (SHA-256), My Axis sign-in, `firmwaremanagement.cgi` |

## Installation

Flathub availability is planned; the packaging/release checklist is in [FLATHUB.md](FLATHUB.md).

**One command** (Fedora, Ubuntu/Debian, Arch; x86_64 and arm64; Silverblue & co. automatically
get the Flatpak variant):

```sh
curl -fsSL https://github.com/reschcloud/ip-tool-for-axis-linux/releases/latest/download/install.sh | sh
```

Remove:

```sh
curl -fsSL https://github.com/reschcloud/ip-tool-for-axis-linux/releases/latest/download/install.sh | sh -s -- --uninstall
```

**Manually:** download the package from the release page and install it, e.g.
`sudo dnf install ./ip-tool-for-axis*.rpm` or `sudo apt install ./ip-tool-for-axis*.deb`.
The package manager pulls in the dependencies (GTK 4, libadwaita ≥ 1.5).

**From source:**

```sh
# Fedora: sudo dnf install golang gtk4-devel libadwaita-devel gobject-introspection-devel
# Ubuntu: sudo apt install golang libgtk-4-dev libadwaita-1-dev libgirepository1.0-dev
make            # the first build takes ~10–15 min because of the GTK bindings
sudo make install
sudo make uninstall
```

## Updating

At startup, the app checks GitHub for a newer stable release. If one is available, a small
**Update available** button beside the app menu opens its release page. The app doesn't update
itself. Find the installed version under Menu → "About IP Tool for AXIS (Linux)" or with
`ip-tool-for-axis-cli version`.
You can also browse the [release history](https://github.com/reschcloud/ip-tool-for-axis-linux/releases).

**Installed with the install command** (package or Flatpak): just run the same command again –
it downloads the latest version and replaces the installed one:

```sh
curl -fsSL https://github.com/reschcloud/ip-tool-for-axis-linux/releases/latest/download/install.sh | sh
```

**Installed manually:** download the new package from the release page and install it the same
way as the first time (`sudo dnf install ./ip-tool-for-axis*.rpm`, `sudo apt install ./ip-tool-for-axis*.deb`
or `sudo pacman -U ./ip-tool-for-axis*.pkg.tar.zst`) – the package manager replaces the old version
when the package name matches.

**From source:**

```sh
git pull
make
sudo make install
```

### Moving from the old package

The new package (`ip-tool-for-axis`) and Flatpak ID (`io.github.reschcloud.iptoolforaxis`) do not
replace the old `axis-ip-tool` package or `io.github.reschcloud.AxisIpTool` Flatpak automatically.
Remove the old installation separately if you no longer need it. Scripts that used `axisctl`
should call `ip-tool-for-axis-cli` instead.

To keep your preferences, close both apps and copy `settings.json` from the old `axis-ip-tool/`
config directory to the new `ip-tool-for-axis/` directory (`~/.config` by default; Flatpak uses
`~/.var/app/<app-id>/config` for each app ID). Sign in to My Axis again if you use firmware downloads.

## Command line

```sh
ip-tool-for-axis-cli list                     # discover devices
ip-tool-for-axis-cli list -ip 192.168.0.90 -ip fe80::1234%eth0   # include manual targets
ip-tool-for-axis-cli list -ip 'https://cam.example.com:8443, camera.local, 10.0.0.20-30'
ip-tool-for-axis-cli init 192.168.0.90        # create the first admin (50 Hz, ceiling mount)
ip-tool-for-axis-cli init 192.168.0.90 -freq 60 -mount wall
ip-tool-for-axis-cli net 192.168.0.90 -ip 10.0.0.20/24 -gw 10.0.0.1 -hostname camera-yard
ip-tool-for-axis-cli time 192.168.0.90 -tz Europe/Vienna -ntp pool.ntp.org
ip-tool-for-axis-cli reset 192.168.0.90       # factory defaults (IP is kept; -all: IP too)

# Several cameras: sequential IPs from .20, hostnames camera-01, camera-02 …
ip-tool-for-axis-cli batch -init -ip 10.0.0.20/24 -gw 10.0.0.1 -hostname 'camera-##' \
    -ntp pool.ntp.org ACCC8E000001 ACCC8E000002 ACCC8E000003
ip-tool-for-axis-cli batch -ip 10.0.0.1/24 -csv plan.csv -dry-run   # preview CSV plan
ip-tool-for-axis-cli firmware 192.168.0.90                        # compare with axis.com
ip-tool-for-axis-cli firmware 192.168.0.90 -version track         # update within the track
ip-tool-for-axis-cli firmware 192.168.0.90 -file P3225-VE_Mk_II_9_80_132.bin
```

### Batch configuration

Select several devices in the list (Ctrl/Shift + click, Ctrl+A) → **Configure multiple**.
A wizard asks for the shared settings, shows a preview with the planned addresses and hostnames
(editable individually, conflicts are highlighted) and configures the devices one after another.
At the end a log can be saved as CSV.

Hostnames: fixed text plus `#` for a sequential number (`camera-##` → camera-01, camera-02 …,
start value configurable) and/or `@` for the last octet of the IP address
(`cam-@` → cam-20 for 10.0.0.20).

Instead of an address range, a **CSV file** can define the assignment (separator `;`, `,` or tab,
header row optional):

```csv
Serial;IP;Hostname
ACCC8E000001;10.0.0.20;camera-entrance
ACCC8E000002;10.0.0.21;camera-yard
```

## Structure

```
cmd/ip-tool-for-axis      GNOME app (Go, gotk4 + libadwaita)
cmd/ip-tool-for-axis-cli  CLI (pure Go)
internal/axis             discovery, VAPIX client, data model
data/                     desktop file, metainfo, icons, screenshots
packaging/                nfpm configuration, Flatpak manifests, install.sh, package scripts
docs/adr/                 architecture decision records
vendor/                   vendored Go modules (offline/Flathub builds)
FLATHUB.md                Flathub release checklist
```

## Notes

- Devices are found in the same network segment (multicast is not routed). Cameras in other
  subnets or with a known address can be added manually (button **+** / menu → "Manual
  cameras") as an address, hostname, complete `http(s)://` URL, CIDR prefix, range
  (`192.168.0.10-20`) or wildcard (`192.168.0.*`); several entries may be separated by
  comma, semicolon and/or spaces. They are pinged and queried exactly like discovered
  devices; hostnames are resolved, their IPv4/IPv6 addresses and interface are shown, and
  the ping tries IPv4 first, then IPv6 (hover over the Ping column to see the address
  used). A 🌐 in the "Route" column marks devices outside the directly attached networks
  – their traffic goes through a router.
- When a camera has a user but the app is not logged in, the action buttons show a small
  padlock and open the login dialog first. After login, "Change password" changes an
  existing password via `pwdgrp.cgi`.
- SSDP replies arrive on a random UDP port. A strict firewall may block them; the mDNS
  discovery via Avahi is not affected.
- Ping uses unprivileged ICMP sockets (`net.ipv4.ping_group_range`, allowed by default on
  current distributions). Without them, reachability is checked via HTTP only.
- A manually added hostname that cannot be resolved shows **Hostname not resolved**; a
  reachable service that doesn't speak VAPIX (e.g. a web server on the same address) shows
  **No VAPIX response**.
- Communication with the cameras uses **HTTPS** if the device offers it, otherwise HTTP
  (column "Connection"). The cameras' self-signed certificates are accepted without
  verification: encrypted, but no protection against an active attacker on the network.
  Menu → "Use HTTPS only" or `ip-tool-for-axis-cli --https-only` forbids the fallback to HTTP.
- **Firmware update:** the firmware column shows when axis.com has a newer version on the
  device's track (LTS tracks stay on their track; menu → "Check axis.com for firmware
  updates" turns this off). Downloading needs a free Axis account: "Firmware" → "Sign in"
  opens the Axis sign-in page in an embedded browser (WebKitGTK 6.0, optional package);
  the app keeps only the session cookie (`~/.config/ip-tool-for-axis/myaxis-session.json`, mode
  0600, valid for about four weeks), never the password. `ip-tool-for-axis-cli` uses the same session or
  `$AXIS_MYAXIS_SESSION`. Downloads are checked against the SHA-256 from axis.com and cached
  in `~/.cache/ip-tool-for-axis/firmware`; the device verifies the signature itself.
- Screenshot for this README: `IP_TOOL_FOR_AXIS_SCREENSHOT=data/screenshots/main-window.png ip-tool-for-axis`
  renders the window after a few seconds and quits.

## License

Copyright © 2026 Michael Resch

This program is free software: you can redistribute it and/or modify it under the terms of the
[GNU Affero General Public License](LICENSE) as published by the Free Software Foundation,
either version 3 of the License, or (at your option) any later version. It is distributed
without any warranty.
