#!/bin/sh
# Axis IP Tool – Installation mit einem Befehl
#
#   curl -fsSL https://github.com/OWNER/axis-ip-tool/releases/latest/download/install.sh | sh
#
# Optionen (nach „sh -s --“ anhängen):
#   --uninstall     alles wieder entfernen
#   --flatpak       Flatpak statt Systempaket (Standard auf Silverblue/Bazzite & Co.)
set -eu

REPO="${AXIS_IP_TOOL_REPO:-OWNER/axis-ip-tool}"
BASE="https://github.com/$REPO/releases/latest/download"
APP_ID=io.github.axisiptool.AxisIpTool

mode=install app=pkg
for a in "$@"; do
  case "$a" in
    --uninstall) mode=uninstall ;;
    --flatpak) app=flatpak ;;
    -h|--help) sed -n '2,8p' "$0" 2>/dev/null || true; exit 0 ;;
    *) echo "Unbekannte Option: $a" >&2; exit 2 ;;
  esac
done

say() { printf '\033[1m%s\033[0m\n' "$*"; }
die() { printf 'Fehler: %s\n' "$*" >&2; exit 1; }

if [ -e /run/ostree-booted ]; then fam=ostree
elif command -v dnf >/dev/null; then fam=dnf
elif command -v apt-get >/dev/null; then fam=apt
elif command -v pacman >/dev/null; then fam=pacman
else fam=unknown; fi

case $fam in ostree|unknown) app=flatpak ;; esac

SUDO=""
if [ "$app" = pkg ] || { [ "$mode" = uninstall ] && [ "$fam" != ostree ] && [ "$fam" != unknown ]; }; then
  if [ "$(id -u)" -eq 0 ]; then SUDO=""
  elif command -v sudo >/dev/null; then SUDO=sudo
  elif command -v run0 >/dev/null; then SUDO=run0
  else die "sudo wird benötigt"; fi
fi

# ---------------------------------------------------------------- Deinstallation
if [ "$mode" = uninstall ]; then
  say "Entferne Axis IP Tool …"
  case $fam in
    dnf)    $SUDO dnf remove -y axis-ip-tool || true ;;
    apt)    $SUDO apt-get remove -y axis-ip-tool || true ;;
    pacman) $SUDO pacman -R --noconfirm axis-ip-tool || true ;;
  esac
  if command -v flatpak >/dev/null && flatpak info "$APP_ID" >/dev/null 2>&1; then
    flatpak uninstall -y "$APP_ID" || true
  fi
  say "Fertig."
  exit 0
fi

# ---------------------------------------------------------------- Installation
[ "$(uname -m)" = x86_64 ] || die "fertige Pakete gibt es nur für x86_64 – bitte aus dem Quellcode bauen (make && sudo make install)"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
get() { say "Lade $1 …"; curl -fL --proto '=https' -o "$tmp/$1" "$BASE/$1"; }

if [ "$app" = flatpak ]; then
  command -v flatpak >/dev/null || die "flatpak ist nicht installiert"
  flatpak remote-add --if-not-exists --user flathub https://dl.flathub.org/repo/flathub.flatpakrepo
  get "$APP_ID.flatpak"
  flatpak install --user -y "$tmp/$APP_ID.flatpak"
else
  case $fam in
    dnf)    ext=x86_64.rpm ;;
    apt)    ext=amd64.deb ;;
    pacman) ext=x86_64.pkg.tar.zst ;;
  esac
  get "axis-ip-tool.$ext"
  say "Installiere Paket (Administratorpasswort erforderlich) …"
  case $fam in
    dnf)    $SUDO dnf install -y "$tmp/axis-ip-tool.$ext" ;;
    apt)    $SUDO apt-get install -y "$tmp/axis-ip-tool.$ext" ;;
    pacman) $SUDO pacman -U --noconfirm "$tmp/axis-ip-tool.$ext" ;;
  esac
fi

say "Fertig! Axis IP Tool ist jetzt im App-Menü."
