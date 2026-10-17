#!/bin/sh
# Debian postinst. A fresh install starts nothing (algod-loadb-mesh-setup
# does). An upgrade restarts the agents that are running, the default one and
# every named instance (algod-loadb-mesh@NAME); they drain on SIGTERM.
set -e
[ "$1" = configure ] || exit 0
[ -d /run/systemd/system ] || exit 0

# A host set up by install.sh runs /usr/local/bin/algod-loadb-mesh, which this
# package does not update.
for unit in /etc/systemd/system/algod-loadb-mesh.service /etc/systemd/system/algod-loadb-mesh@.service; do
	if [ -f "$unit" ] && ! grep -Eq '^ExecStart=/usr/bin/algod-loadb-mesh( |$)' "$unit"; then
		echo "algod-loadb-mesh: $unit does not run /usr/bin/algod-loadb-mesh;" \
			"see \"Moving to the apt package\" in the README" >&2
	fi
done

# Best effort: a failed restart must not fail the upgrade; systemd shows why.
if [ -n "${2:-}" ]; then
	if [ -f /etc/systemd/system/algod-loadb-mesh.service ]; then
		systemctl try-restart algod-loadb-mesh || echo "algod-loadb-mesh: restart failed; see systemctl status algod-loadb-mesh" >&2
	fi
	if [ -f /etc/systemd/system/algod-loadb-mesh@.service ]; then
		# The pattern matches the instances systemd has loaded: the running ones.
		systemctl try-restart 'algod-loadb-mesh@*.service' ||
			echo "algod-loadb-mesh: restart failed; see systemctl status 'algod-loadb-mesh@*'" >&2
	fi
fi
