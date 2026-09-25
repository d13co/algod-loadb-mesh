#!/bin/sh
# Debian postinst. A fresh install starts nothing (algod-loadb-mesh-setup
# does). An upgrade restarts the agent if it is running; it drains on SIGTERM.
set -e
[ "$1" = configure ] || exit 0
[ -d /run/systemd/system ] || exit 0

unit=/etc/systemd/system/algod-loadb-mesh.service
# A host set up by install.sh runs /usr/local/bin/algod-loadb-mesh, which this
# package does not update.
if [ -f "$unit" ] && ! grep -Eq '^ExecStart=/usr/bin/algod-loadb-mesh( |$)' "$unit"; then
	echo "algod-loadb-mesh: $unit does not run /usr/bin/algod-loadb-mesh;" \
		"see \"Moving to the apt package\" in the README" >&2
fi

# Best effort: a failed restart must not fail the upgrade; systemd shows why.
if [ -n "${2:-}" ]; then
	systemctl try-restart algod-loadb-mesh || echo "algod-loadb-mesh: restart failed; see systemctl status algod-loadb-mesh" >&2
fi
