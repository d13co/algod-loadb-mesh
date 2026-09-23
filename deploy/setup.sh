#!/usr/bin/env bash
# Sets up algod-loadb-mesh on this host as root: the systemd unit, the
# directories it needs, a config (from autoconfig.sh or one you provide) and
# the service itself. install.sh puts it in the PATH as
# algod-loadb-mesh-setup; it re-runs itself under sudo when needed.
#
#   sudo algod-loadb-mesh-setup [options] [autoconfig options] [BUNDLE]
#
#   --config FILE    install FILE as the config instead of running autoconfig
#                    ("-" reads it from stdin)
#   --force          replace an existing config
#   --no-start       install everything, but do not enable or start the service
#   --user USER      user the service runs as (default: algorand, or root when
#                    there is no algorand user)
#
# Anything else is passed to autoconfig, which detects what it can and takes
# the registry app id and sync key from the bundle printed by
# `algod-loadb-mesh registry bundle` on a host that already runs:
#
#   sudo algod-loadb-mesh-setup -          # paste the bundle, then Ctrl-D
#   sudo algod-loadb-mesh-setup --data-dir /var/lib/algorand --tier 2 BUNDLE
set -euo pipefail

main() {
	PREFIX=${PREFIX:-/usr/local/bin}
	SHARE=${SHARE:-/usr/local/share/algod-loadb-mesh}
	CONFIG_DIR=${CONFIG_DIR:-/etc/algod-loadb-mesh}
	REPO=${REPO:-d13co/algod-loadb-mesh}
	REF=${REF:-stable}
	local state_dir=${STATE_DIR:-/var/lib/algod-loadb-mesh}
	local unit=${UNIT:-/etc/systemd/system/algod-loadb-mesh.service}
	local config="" force="" start=1 user="" autoconfig=()

	local arg
	for arg in "$@"; do
		case "$arg" in -h | --help) help; return 0 ;; esac
	done

	if [ "$(id -u)" != 0 ]; then
		[ -r "$0" ] || die "run this with sudo"
		command -v sudo >/dev/null || die "run this as root"
		say "not root; re-running under sudo"
		exec sudo -- "$0" "$@"
	fi

	while [ $# -gt 0 ]; do
		case "$1" in
		--config) config=$2; shift ;;
		--force) force=1 ;;
		--no-start) start="" ;;
		--user) user=$2; shift ;;
		# --data-dir, --address, --app-id, BUNDLE, ...
		*) autoconfig+=("$1") ;;
		esac
		shift
	done

	local mesh=$PREFIX/algod-loadb-mesh
	[ -x "$mesh" ] || mesh=$(command -v algod-loadb-mesh) || die "algod-loadb-mesh is not installed (see install.sh)"

	if [ -z "$user" ]; then
		user=algorand
		id -u "$user" >/dev/null 2>&1 || user=root
	fi
	id -u "$user" >/dev/null 2>&1 || die "no such user: $user"
	local group
	group=$(id -gn "$user")

	# --- directories --------------------------------------------------------

	install -d -m 0750 -o "$user" -g "$group" "$CONFIG_DIR" "$state_dir"

	# --- config -------------------------------------------------------------

	# A config that does not load would only fail later, inside the unit, so
	# it is checked before it replaces anything.
	local target=$CONFIG_DIR/config.yaml
	if [ -e "$target" ] && [ -z "$force" ]; then
		say "keeping the config at $target (--force replaces it)"
		"$mesh" config check -config "$target" || die "fix $target and run this again"
	else
		# Not local: the EXIT trap runs after main returns.
		tmp=$(mktemp)
		chmod 0600 "$tmp"
		trap 'rm -f "${tmp:-}"' EXIT
		if [ -n "$config" ]; then
			if [ "$config" = - ]; then
				cat >"$tmp"
			else
				cat "$config" >"$tmp"
			fi
			[ -s "$tmp" ] || die "the config is empty"
		else
			local auto=$PREFIX/algod-loadb-mesh-autoconfig
			[ -x "$auto" ] || auto=$(command -v algod-loadb-mesh-autoconfig) ||
				die "algod-loadb-mesh-autoconfig is not installed (see install.sh), or pass --config FILE"
			"$auto" -o "$tmp" -f ${autoconfig[@]+"${autoconfig[@]}"}
		fi
		"$mesh" config check -config "$tmp" || die "fix the config and run this again"
		# The config holds the sync key: only the service user reads it.
		install -m 0600 -o "$user" -g "$group" "$tmp" "$target"
		say "wrote $target"
	fi

	# --- service ------------------------------------------------------------

	if [ ! -d /run/systemd/system ]; then
		say "no systemd here; start the agent yourself: $mesh run -config $target"
		return 0
	fi
	local src=$SHARE/algod-loadb-mesh.service
	if [ ! -f "$src" ]; then
		src=$(mktemp)
		fetch "${RAW_URL:-https://raw.githubusercontent.com/$REPO/$REF}/deploy/algod-loadb-mesh.service" "$src" ||
			die "no unit file at $SHARE and none to fetch"
	fi
	# The unit ships with User=algorand and the release path.
	sed -e "s|^User=.*|User=$user|" -e "s|^Group=.*|Group=$group|" \
		-e "s|^ExecStart=.*|ExecStart=$mesh run -config $target|" \
		-e "s|^ReadWritePaths=.*|ReadWritePaths=$state_dir|" "$src" >"$unit.tmp"
	chmod 0644 "$unit.tmp"
	mv "$unit.tmp" "$unit"
	systemctl daemon-reload
	say "installed $unit (User=$user)"

	if [ -z "$start" ]; then
		say "not starting it (--no-start); when you are ready: systemctl enable --now algod-loadb-mesh"
		return 0
	fi
	systemctl enable --now algod-loadb-mesh
	sleep 1
	if systemctl is-active --quiet algod-loadb-mesh; then
		local data_dir listen
		data_dir=$(sed -n 's/^  data_dir: //p' "$target" | head -n1)
		# `config check` prints the resolved listen addresses; take the first.
		listen=$("$mesh" config check -config "$target" | sed -n 's/^listen \([^,]*\).*/\1/p' | head -n1)
		case "$listen" in
		0.0.0.0:*) listen=127.0.0.1:${listen##*:} ;;
		\[::\]:*) listen=[::1]:${listen##*:} ;;
		esac
		say "algod-loadb-mesh is running"
		say "peers: curl -H \"X-Algo-API-Token: \$(sudo cat $data_dir/algod.admin.token)\" http://$listen/loadb/peers"
	else
		systemctl --no-pager --lines=20 status algod-loadb-mesh || true
		die "the service did not start"
	fi
}

# help prints this file's header comment, without the hashes.
help() { sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//'; }

say() { echo "algod-loadb-mesh: $*" >&2; }
die() { say "$*"; exit 1; }

fetch() {
	if command -v curl >/dev/null; then
		curl -fsL --retry 3 -o "$2" "$1"
	else
		wget -qO "$2" "$1"
	fi
}

main "$@"
