#!/bin/sh
# IP Tool for AXIS (Linux) – installation with one command
#
#   curl -fsSL https://github.com/reschcloud/ip-tool-for-axis-linux/releases/latest/download/install.sh | sh
#
# Options (append after "sh -s --"):
#   --uninstall     remove everything again
#   --flatpak       Flatpak instead of system package (default on Silverblue/Bazzite & co.)
set -eu

REPO="${IP_TOOL_FOR_AXIS_REPO:-reschcloud/ip-tool-for-axis-linux}"
BASE="https://github.com/$REPO/releases/latest/download"
APP_ID=io.github.reschcloud.iptoolforaxis
PKG=ip-tool-for-axis

mode=install app=pkg
for a in "$@"; do
  case "$a" in
    --uninstall) mode=uninstall ;;
    --flatpak) app=flatpak ;;
    -h|--help) sed -n '2,8p' "$0" 2>/dev/null || true; exit 0 ;;
    *) echo "Unknown option: $a" >&2; exit 2 ;;
  esac
done

say() { printf '\033[1m%s\033[0m\n' "$*"; }
die() { printf 'Error: %s\n' "$*" >&2; exit 1; }

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
  else die "sudo is required"; fi
fi

# ---------------------------------------------------------------- Uninstall
if [ "$mode" = uninstall ]; then
  say "Removing IP Tool for AXIS (Linux) …"
  case $fam in
    dnf)    $SUDO dnf remove -y "$PKG" || true ;;
    apt)    $SUDO apt-get remove -y "$PKG" || true ;;
    pacman) $SUDO pacman -R --noconfirm "$PKG" || true ;;
  esac
  if command -v flatpak >/dev/null && flatpak info "$APP_ID" >/dev/null 2>&1; then
    flatpak uninstall -y "$APP_ID" || true
  fi
  say "Done."
  exit 0
fi

# ---------------------------------------------------------------- Installation
case "$(uname -m)" in
  x86_64)        arch=x86_64  debarch=amd64 ;;
  aarch64|arm64) arch=aarch64 debarch=arm64 ;;
  *) die "prebuilt packages are only available for x86_64 and aarch64 – please build from source (make && sudo make install)" ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
get() { say "Downloading $1 …"; curl -fL --proto '=https' -o "$tmp/$1" "$BASE/$1"; }

if [ "$app" = flatpak ]; then
  command -v flatpak >/dev/null || die "flatpak is not installed"
  flatpak remote-add --if-not-exists --user flathub https://dl.flathub.org/repo/flathub.flatpakrepo
  get "$APP_ID.$arch.flatpak"
  flatpak install --user -y --reinstall "$tmp/$APP_ID.$arch.flatpak" # --reinstall = update
else
  case $fam in
    dnf)    ext=$arch.rpm ;;
    apt)    ext=$debarch.deb ;;
    pacman) ext=$arch.pkg.tar.zst ;;
  esac
  get "$PKG.$ext"
  say "Installing package (administrator password required) …"
  case $fam in
    dnf)    $SUDO dnf install -y "$tmp/$PKG.$ext" ;;
    apt)    $SUDO apt-get install -y "$tmp/$PKG.$ext" ;;
    pacman) $SUDO pacman -U --noconfirm "$tmp/$PKG.$ext" ;;
  esac
fi

say "Done! IP Tool for AXIS (Linux) is now in the app menu."
