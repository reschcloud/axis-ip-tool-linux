#!/bin/sh
# Icon- und Desktop-Caches aktualisieren (auf den meisten Distros erledigen das auch Paket-Trigger).
gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor >/dev/null 2>&1 || true
update-desktop-database -q /usr/share/applications >/dev/null 2>&1 || true
exit 0
