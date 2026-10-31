#!/bin/sh
# Refresh the icon and desktop-entry caches so the k8scli icons show up in menus right away.
# Both tools are optional (headless servers do not have them), so a missing one is fine.
set -e

if command -v gtk-update-icon-cache >/dev/null 2>&1 && [ -d /usr/share/icons/hicolor ]; then
	gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor || true
fi
if command -v update-desktop-database >/dev/null 2>&1 && [ -d /usr/share/applications ]; then
	update-desktop-database -q /usr/share/applications || true
fi
exit 0
