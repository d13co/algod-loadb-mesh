#!/bin/sh
# Debian postinst. A fresh install starts nothing (algod-mesh-setup
# does). An upgrade restarts the agents that are running, the default one and
# every named instance (algod-mesh@NAME); they drain on SIGTERM.
set -e
[ "$1" = configure ] || exit 0

# The project was called algod-loadb-mesh. Its units, config, state and
# install.sh files stay behind under that name, and its units point at a
# binary that is gone.
for old in /etc/systemd/system/algod-loadb-mesh.service /etc/systemd/system/algod-loadb-mesh@.service \
	/etc/algod-loadb-mesh /var/lib/algod-loadb-mesh /usr/local/bin/algod-loadb-mesh /usr/local/share/algod-loadb-mesh \
	/etc/apt/apt.conf.d/51algod-loadb-mesh-upgrades; do
	if [ -e "$old" ]; then
		echo "algod-mesh: $old is left over from algod-loadb-mesh;" \
			"see \"Moving from algod-loadb-mesh\" in the README" >&2
	fi
done

[ -d /run/systemd/system ] || exit 0

# A host set up by install.sh runs /usr/local/bin/algod-mesh, which this
# package does not update.
for unit in /etc/systemd/system/algod-mesh.service /etc/systemd/system/algod-mesh@.service; do
	if [ -f "$unit" ] && ! grep -Eq '^ExecStart=/usr/bin/algod-mesh( |$)' "$unit"; then
		echo "algod-mesh: $unit does not run /usr/bin/algod-mesh;" \
			"see \"Moving to the apt package\" in the README" >&2
	fi
done

# Best effort: a failed restart must not fail the upgrade; systemd shows why.
if [ -n "${2:-}" ]; then
	if [ -f /etc/systemd/system/algod-mesh.service ]; then
		systemctl try-restart algod-mesh || echo "algod-mesh: restart failed; see systemctl status algod-mesh" >&2
	fi
	if [ -f /etc/systemd/system/algod-mesh@.service ]; then
		# The pattern matches the instances systemd has loaded: the running ones.
		systemctl try-restart 'algod-mesh@*.service' ||
			echo "algod-mesh: restart failed; see systemctl status 'algod-mesh@*'" >&2
	fi
fi
