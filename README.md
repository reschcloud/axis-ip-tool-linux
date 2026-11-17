# Axis IP Tool

GNOME-App und Kommandozeilenwerkzeug zum Finden und Grundkonfigurieren von
Axis-Netzwerkkameras – angelehnt an die **AXIS IP Utility**.
Kein offizielles Produkt der Axis Communications AB.

## Funktionen

| Funktion | Umsetzung |
|---|---|
| Geräte finden | Bonjour/mDNS über Avahi (D-Bus) + SSDP/UPnP |
| Modell, Seriennummer, Firmware | `basicdeviceinfo.cgi` (ohne Anmeldung, bei gesperrten Geräten mit) |
| Werkszustand erkennen / Erstpasswort | `pwdgrp.cgi` |
| IPv4 (DHCP/statisch), Hostname, DNS | `network_settings.cgi`, Fallback `param.cgi` |
| Zeitzone + Uhrzeit | `time.cgi`, Fallback `date.cgi` |
| NTP-Server | `ntp.cgi`, Fallback `param.cgi` |
| Vorschaubild | `jpg/image.cgi` (1 Bild/s) |
| Webseite öffnen, Neustart | Standardbrowser, `restart.cgi` |

## Installation

**Ein Befehl** (Fedora, Ubuntu/Debian, Arch; x86_64 und arm64; Silverblue & Co. bekommen automatisch
die Flatpak-Variante):

```sh
curl -fsSL https://github.com/reschcloud/axis-ip-tool-linux/releases/latest/download/install.sh | sh
```

Entfernen:

```sh
curl -fsSL https://github.com/reschcloud/axis-ip-tool-linux/releases/latest/download/install.sh | sh -s -- --uninstall
```

**Manuell:** Paket von der Release-Seite laden und installieren, z. B.
`sudo dnf install ./axis-ip-tool*.rpm` bzw. `sudo apt install ./axis-ip-tool*.deb`.
Abhängigkeiten (GTK 4, libadwaita ≥ 1.5) zieht der Paketmanager selbst nach.

**Aus dem Quellcode:**

```sh
# Fedora: sudo dnf install golang gtk4-devel libadwaita-devel gobject-introspection-devel
# Ubuntu: sudo apt install golang libgtk-4-dev libadwaita-1-dev libgirepository1.0-dev
make            # erster Build dauert wegen der GTK-Bindings ~10–15 min
sudo make install
sudo make uninstall
```

## Aktualisieren

Die App aktualisiert sich nicht selbst. Welche Version installiert ist, zeigt
Menü → „Über Axis IP Tool“ bzw. `axisctl version`; neue Versionen stehen auf der
[Release-Seite](https://github.com/reschcloud/axis-ip-tool-linux/releases).

**Mit dem Installationsbefehl installiert** (Paket oder Flatpak): einfach denselben Befehl erneut
ausführen – er lädt die neueste Version und ersetzt die installierte:

```sh
curl -fsSL https://github.com/reschcloud/axis-ip-tool-linux/releases/latest/download/install.sh | sh
```

**Manuell installiert:** das neue Paket von der Release-Seite laden und genauso installieren wie
beim ersten Mal (`sudo dnf install ./axis-ip-tool*.rpm`, `sudo apt install ./axis-ip-tool*.deb`
bzw. `sudo pacman -U ./axis-ip-tool*.pkg.tar.zst`) – der Paketmanager ersetzt die alte Version.

**Aus dem Quellcode:**

```sh
git pull
make
sudo make install
```

## Kommandozeile

```sh
axisctl list                                   # Geräte suchen
axisctl init 192.168.0.90                      # ersten Admin anlegen
axisctl net 192.168.0.90 -ip 10.0.0.20/24 -gw 10.0.0.1 -hostname kamera-hof
axisctl time 192.168.0.90 -tz Europe/Vienna -ntp pool.ntp.org
```

## Aufbau

```
cmd/axis-ip-tool     GNOME-App (Go, gotk4 + libadwaita)
cmd/axisctl          CLI (reines Go)
internal/axis        Suche, VAPIX-Client, Datenmodell
data/                Desktop-Datei, Metainfo, Icons
packaging/           nfpm-Konfiguration, Flatpak-Manifest, install.sh, Paketskripte
```

## Hinweise

- Gefunden werden Geräte im selben Netzsegment (Multicast wird nicht geroutet).
- SSDP-Antworten kommen auf einem zufälligen UDP-Port zurück. Eine strenge Firewall kann sie
  blockieren; die mDNS-Suche über Avahi ist davon nicht betroffen.
- Kommunikation mit den Kameras per **HTTPS**, wenn das Gerät es anbietet, sonst per HTTP
  (Spalte „Verbindung“). Die selbst ausgestellten Zertifikate der Kameras werden ohne Prüfung
  akzeptiert: verschlüsselt, aber kein Schutz gegen einen aktiven Angreifer im Netz.
  Menü → „Nur HTTPS verwenden“ bzw. `axisctl --https-only` verbietet den Rückfall auf HTTP.

## Lizenz

Copyright © 2026 Michael Resch

Dieses Programm ist freie Software: Sie können es unter den Bedingungen der
[GNU General Public License](LICENSE), Version 3 oder (nach Ihrer Wahl) jeder späteren Version,
weitergeben und/oder verändern. Es wird ohne jede Gewährleistung bereitgestellt.
