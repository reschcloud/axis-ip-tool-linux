# CLAUDE.md – Axis IP Tool

GNOME-App + CLI zum Finden und Grundkonfigurieren von Axis-Kameras (Vorbild: AXIS IP Utility).
Sprache für UI-Texte, Kommentare und Commit-Messages: **Deutsch**.

## Befehle

```sh
make                 # alles bauen -> build/
make test            # go vet + go test (ohne GUI, schnell)
make gui|cli         # einzelne Binaries
sudo make install    # nach /usr
make packages        # rpm/deb/arch nach dist/ (braucht nfpm)
make flatpak         # Flatpak-Bundle nach dist/
./build/axisctl list -t 5
```

Build-Abhängigkeiten GUI: `gtk4-devel libadwaita-devel gobject-introspection-devel` (Fedora)
bzw. `libgtk-4-dev libadwaita-1-dev libgirepository1.0-dev` (Debian/Ubuntu).
**Der erste GUI-Build dauert 10–15 Minuten** (cgo-Bindings für GTK); danach ist er gecacht.
`axisctl` ist reines Go (`CGO_ENABLED=0`) und baut in Sekunden.
Achtung: `go build` ohne `-trimpath` hat einen eigenen Cache → GTK-Bindings werden neu gebaut.
Für schnelle Prüfungen immer `make gui` verwenden.

## Architektur

- `internal/axis` – alles Geräte-Spezifische, ohne GUI-Abhängigkeit:
  - `discovery.go`: `Discover(ctx, found, warn)` – Avahi über D-Bus (`_axis-video._tcp`,
    `_vapix-http._tcp`) + SSDP-M-SEARCH alle 30 s; jedes Ergebnis wird per
    `basicdeviceinfo.cgi`/`pwdgrp.cgi` angereichert. Callbacks kommen aus Goroutinen.
  - `registry.go`: führt Teilergebnisse zusammen (Schlüssel = Seriennummer, sonst IP;
    SSDP-Einträge ohne Seriennummer werden umgeschlüsselt). Von CLI und GUI genutzt.
  - `vapix.go`: `Client` mit eigener Digest-Auth (`digest.go`, MD5 + SHA-256), JSON-APIs mit
    Fallbacks auf `param.cgi`/`date.cgi` für alte Firmware. `ErrAuth` signalisiert „neu anmelden“.
    Protokoll: `ProbeHTTPS` (TLS-Handshake auf 443, 2 min gecacht) → HTTPS, sonst HTTP-Port;
    `SetHTTPSOnly(true)` → `ErrNoHTTPS` statt Rückfall. Zertifikate werden bewusst NICHT geprüft
    und auch nicht gemerkt (Entscheidung des Nutzers).
- `cmd/axis-ip-tool`: GUI. Liste = `gtk.StringList` mit Geräteschlüsseln → Filter/Sort-Modelle →
  `ColumnView`. Zellen werden live aktualisiert über `window.cells` (gebundene Labels je Schlüssel),
  ohne Modelländerung (Auswahl bleibt erhalten). Statuszelle „Gesperrt“ ist klickbar → `unlock()`.
  Netzwerkaufrufe immer in Goroutinen, Rückkehr in den GTK-Thread über `glib.IdleAdd` (`ui()`/`async()`/`withLogin()` in actions.go).
- `internal/axis/batch.go`: Mehrfachkonfiguration. `PlanBatch` (IP-Bereich, überspringt belegte
  IPs/Gateway/Netz-/Broadcast, Hostname-Muster `{nr:02}` `{serial}` `{ip}`), `CheckAssignments`
  (nach Handänderungen), `ParsePlanCSV`/`ApplyPlan`, `AddressInUse` (TCP 80/443), `RunBatch`
  (sequenziell, IP zuletzt, Fehler stoppen den Rest nicht). GUI: `cmd/axis-ip-tool/batch.go`
  (Assistent mit `adw.NavigationView`), CLI: `axisctl batch`.
- `cmd/axisctl`: CLI mit Unterbefehlen; Passwort aus `$AXIS_PASSWORD` oder Terminal-Abfrage.

## Wichtige Entscheidungen / Fallstricke

- **gotk4 ist auf den libadwaita-1.5-Zweig gepinnt** (`gotk4-adwaita` Branch `adw-1.5`,
  `gotk4 v0.3.2-0.20250703…`), damit der Build auf Ubuntu 24.04 (GTK 4.14, GLib 2.80) läuft.
  Neuere gotk4-Versionen verlangen GLib ≥ 2.86 und brechen dort. Keine Adw-APIs > 1.5 verwenden
  (z. B. kein `Adw.Spinner`, `Adw.ToggleGroup`) – sonst CI/Ubuntu-Pakete anpassen.
- In gotk4 liefert die ColumnView-Factory `*gtk.ColumnViewCell`, nicht `*gtk.ListItem` →
  Interface `listCell` verwenden. Vergleiche von Widgets über `coreglib.InternObject(x).Native()`.
- `go.mod` enthält `replace`-Direktiven auf die GitHub-Spiegel von `golang.org/x/*` und
  `go4.org/…` (die Entwicklungsumgebung hatte keinen Zugriff auf proxy.golang.org). Sie sind
  harmlos; wer will, entfernt sie und führt `go mod tidy` aus.
- App-ID: `io.github.reschcloud.AxisIpTool`, Repo `github.com/reschcloud/axis-ip-tool-linux`
  (Dateinamen in `data/`, `packaging/`, Konstante in `main.go`, `APP_ID` in Makefile/install.sh).
- Pakete: ein Paket `axis-ip-tool` (GUI + CLI) für x86_64 und arm64. Release-Builds nativ auf
  Ubuntu 24.04 (`ubuntu-24.04` / `ubuntu-24.04-arm`, älteste glibc), laufen damit auch auf
  Fedora ≥ 40 und Arch. nfpm-Architektur kommt aus `$ARCH` (Makefile: `go env GOARCH`).
  Release-Dateien haben feste Namen (`axis-ip-tool.<arch>.rpm` usw.), die `install.sh` erwartet. openSUSE: Paketnamen der Abhängigkeiten passen
  nicht (nfpm-rpm nutzt Fedora-Namen) → dort aus dem Quellcode bauen.
- Neuere AXIS OS (gesehen an P3225-VE Mk II) beantworten `basicdeviceinfo.cgi` nach dem Anlegen
  eines Benutzers nur noch mit Anmeldung (401). Modell kommt dann aus dem mDNS-Namen; Firmware lädt
  die GUI per `refreshInfo()` nach, sobald Zugangsdaten bekannt sind (`getAllProperties`).
- **ARP/Ping-IP-Zuweisung wurde am 2026-11-17 vorerst entfernt** (Systemhelfer, D-Bus-Dienst,
  polkit, `internal/arpping`, `axisctl assign`). Bei Bedarf neu umsetzen.

## Teststand

Getestet (Ubuntu 24.04-Container, Xvfb, Fake-Kameras als Python-HTTP-Server + `avahi-publish`):
- `go test ./internal/...`: VAPIX-Ablauf gegen Fake-Kamera inkl. Digest-Auth, gesperrte
  Geräteinfos, NTP, Snapshot, Registry (Link-Local-Vorrang, Gesperrt-Status), Parser.
- Echte Kameras AXIS P3225-VE Mk II und Q2101-TE (AXIS OS 11.11): Suche, Erstpasswort,
  Gesperrt-Erkennung, HTTPS-Erkennung.
- `axisctl list` findet per Avahi angekündigte Geräte und reichert sie an.
- GUI: Liste, Status, Passwort-Dialog (legt Benutzer an), Netzwerk-Dialog lädt Konfiguration.

**Nicht getestet:** echte Axis-Kamera (alle VAPIX-Aufrufe sind nach Doku implementiert),
Übernehmen der Netzwerk-/Zeiteinstellungen in der GUI, NTP/Vorschau an echter Kamera,
SSDP gegen echte Geräte, `make packages` (nfpm), Flatpak-Build, install.sh.
Es gibt (noch) keinen GitHub-Actions-Workflow im Projekt.

## Offene Punkte / Ideen

1. An echter Kamera testen (am besten AXIS OS 11/12 und eine ältere Firmware 9.x).
2. GitHub-Repo anlegen, pushen, Tag `v0.1.0` (Workflow `.github/workflows/release.yml` baut Pakete
   + Release). rpm-Installation/-Deinstallation auf Fedora getestet; install.sh und arm64 ungetestet.
3. Passwörter optional im GNOME-Schlüsselbund speichern (Secret-Service/Portal) statt nur im Speicher.
4. UI in Blueprint (`.blp`) auslagern? Aktuell im Code gebaut (gotk4-Templates sind umständlich).
5. Mehrfachkonfiguration: umgesetzt (v0.2.0), GUI-Assistent noch nicht an echten Kameras
   durchgespielt. Idee: Vorschaubild je Gerät in der Vorschau-Tabelle; Neustart für mehrere.
6. Firmware-Update, Werksreset. (NTP über `ntp.cgi` ist umgesetzt, an P3225-VE Mk II noch ungetestet.)
7. Übersetzung (gettext) – derzeit nur Deutsch.
8. –
