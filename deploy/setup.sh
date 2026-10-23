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
#   --instance NAME  set up a further agent on this host, for another algod:
#                    config NAME.yaml, service algod-loadb-mesh@NAME, id
#                    HOSTNAME-NAME, and the first free client and mesh ports
#                    (4000/4001, else 4002/4003, ...)
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
#
# One agent serves one algod. For a second algod on the host (another
# network, or another role), add a named instance with that network's bundle:
#
#   sudo algod-loadb-mesh-setup --instance testnet --data-dir /var/lib/algorand/testnet -
set -euo pipefail

main() {
	PREFIX=${PREFIX:-/usr/local/bin}
	SHARE=${SHARE:-/usr/local/share/algod-loadb-mesh}
	CONFIG_DIR=${CONFIG_DIR:-/etc/algod-loadb-mesh}
	REPO=${REPO:-d13co/algod-loadb-mesh}
	REF=${REF:-stable}
	local state_dir=${STATE_DIR:-/var/lib/algod-loadb-mesh}
	local unit=${UNIT:-/etc/systemd/system/algod-loadb-mesh.service}
	local config="" force="" start=1 user="" instance="" autoconfig=()

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
		--config) need_value "$@"; config=$2; shift ;;
		--force) force=1 ;;
		--no-start) start="" ;;
		--user) need_value "$@"; user=$2; shift ;;
		--instance) need_value "$@"; instance=$2; shift ;;
		# --data-dir, --address, --app-id, BUNDLE, ...
		*) autoconfig+=("$1") ;;
		esac
		shift
	done

	if [ -n "$instance" ]; then
		[[ "$instance" =~ ^[a-z0-9][a-z0-9-]{0,31}$ ]] ||
			die "--instance \"$instance\": want up to 32 lowercase letters, digits and -, not starting with -"
		[ "$instance" != config ] || die "--instance config: that name is the default agent's config.yaml; pick another"
	fi

	local mesh=$PREFIX/algod-loadb-mesh
	[ -x "$mesh" ] || mesh=$(command -v algod-loadb-mesh) || die "algod-loadb-mesh is not installed (see install.sh)"

	# The default agent is config.yaml and algod-loadb-mesh.service; a named
	# one is NAME.yaml and algod-loadb-mesh@NAME.service.
	local target=$CONFIG_DIR/config.yaml svc=algod-loadb-mesh
	if [ -n "$instance" ]; then
		target=$CONFIG_DIR/$instance.yaml
		svc=algod-loadb-mesh@$instance
	fi
	local others=0 f
	for f in "$CONFIG_DIR"/*.yaml; do
		[ -f "$f" ] && [ "$f" != "$target" ] && others=$((others + 1))
	done

	# Every agent on the host runs as one user: named instances share the
	# template unit, and all share the config and state dirs, which setup
	# hands to that user. With other agents here, another user could no
	# longer read their configs. Without --user, the one already set stays.
	local template u
	template=$(dirname "$unit")/algod-loadb-mesh@.service
	for f in "$unit" "$template"; do
		[ -f "$f" ] || continue
		u=$(sed -n 's/^User=//p' "$f" | head -n1)
		[ -n "$u" ] || continue
		if [ -z "$user" ]; then
			user=$u
		elif [ "$user" != "$u" ] && [ "$others" -gt 0 ]; then
			die "--user $user: $f runs as $u, and the agents on this host share one user"
		fi
	done
	if [ -z "$user" ]; then
		user=algorand
		id -u "$user" >/dev/null 2>&1 || user=root
	fi
	id -u "$user" >/dev/null 2>&1 || die "no such user: $user"
	local group
	group=$(id -gn "$user")

	# --- directories --------------------------------------------------------

	install -d -m 0750 -o "$user" -g "$group" "$CONFIG_DIR" "$state_dir"

	# --- other agents on this host --------------------------------------------

	# What the others serve: their data dirs, every port they bind, their ids
	# and registry caches. A config that does not load would hide what it
	# binds, so it stops the setup.
	local other_dirs=() other_ports=" " other_ids=" " other_caches=" "
	for f in "$CONFIG_DIR"/*.yaml; do
		[ -f "$f" ] && [ "$f" != "$target" ] || continue
		read_instance "$mesh" "$f" ||
			die "$f does not load (algod-loadb-mesh config check -config $f); every *.yaml in $CONFIG_DIR is an agent's config: fix it or move it away"
		[ -z "$I_DIR" ] || other_dirs+=("$I_DIR")
		other_ports+="$I_PORTS "
		other_ids+="$I_ID "
		[ -z "$I_CACHE" ] || other_caches+="$I_CACHE "
	done

	# --data-dir as given to autoconfig, if it was.
	local want_dir="" i
	for i in "${!autoconfig[@]}"; do
		[ "${autoconfig[$i]}" = --data-dir ] && want_dir=${autoconfig[$((i + 1))]:-}
	done
	[ -z "$want_dir" ] || want_dir=$(realpath -m "$want_dir")

	# --- config -------------------------------------------------------------

	# A config that does not load would only fail later, inside the unit, so
	# it is checked before it replaces anything.
	if [ -e "$target" ] && [ -z "$force" ]; then
		"$mesh" config check -config "$target" || die "fix $target and run this again"
		read_instance "$mesh" "$target"
		if [ -n "$want_dir" ] && [ "$I_DIR" != "$want_dir" ]; then
			local hint
			hint=$(instance_name "$CONFIG_DIR" "$want_dir")
			die "$target already serves $I_DIR; to add an agent for $want_dir: --instance ${hint:-NAME} (--force replaces it instead)"
		fi
		check_clash "$target"
		say "keeping the config at $target (--force replaces it; --instance NAME adds an agent for another algod)"
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
			local extra=()
			[ -z "$instance" ] || extra+=(--instance "$instance")
			for f in ${other_dirs[@]+"${other_dirs[@]}"}; do
				extra+=(--skip-data-dir "$f")
			done
			# A replaced config keeps its ports while no other agent took them;
			# otherwise the first pair no other agent binds.
			local client_port="" mesh_port="" k=0
			if [ -e "$target" ] && read_instance "$mesh" "$target" && [ -n "$I_CLIENT" ] && [ -n "$I_MESH" ] &&
				! has_port "$other_ports" "$I_CLIENT" && ! has_port "$other_ports" "$I_MESH"; then
				client_port=$I_CLIENT mesh_port=$I_MESH
			else
				while has_port "$other_ports" $((4000 + 2 * k)) || has_port "$other_ports" $((4001 + 2 * k)); do
					k=$((k + 1))
				done
				client_port=$((4000 + 2 * k)) mesh_port=$((4001 + 2 * k))
			fi
			has_arg --client-port || extra+=(--client-port "$client_port")
			has_arg --mesh-port || extra+=(--mesh-port "$mesh_port")
			"$auto" -o "$tmp" -f ${extra[@]+"${extra[@]}"} ${autoconfig[@]+"${autoconfig[@]}"}
		fi
		"$mesh" config check -config "$tmp" || die "fix the config and run this again"
		check_clash "$tmp"
		# The config holds the sync key: only the service user reads it.
		install -m 0600 -o "$user" -g "$group" "$tmp" "$target"
		say "wrote $target"
	fi

	# --- service ------------------------------------------------------------

	if [ ! -d /run/systemd/system ]; then
		say "no systemd here; start the agent yourself: $mesh run -config $target"
		return 0
	fi
	# A named instance runs from the template unit algod-loadb-mesh@.service.
	[ -z "$instance" ] || unit=$template
	local src=$SHARE/algod-loadb-mesh.service
	# The apt package keeps the unit template under /usr/share.
	[ -f "$src" ] || src=/usr/share/algod-loadb-mesh/algod-loadb-mesh.service
	if [ ! -f "$src" ]; then
		src=$(mktemp)
		fetch "${RAW_URL:-https://raw.githubusercontent.com/$REPO/$REF}/deploy/algod-loadb-mesh.service" "$src" ||
			die "no unit file at $SHARE and none to fetch"
	fi
	# The unit ships with User=algorand and the release path.
	local exec_config=$target
	[ -z "$instance" ] || exec_config=$CONFIG_DIR/%i.yaml
	sed -e "s|^User=.*|User=$user|" -e "s|^Group=.*|Group=$group|" \
		-e "s|^ExecStart=.*|ExecStart=$mesh run -config $exec_config|" \
		-e "s|^ReadWritePaths=.*|ReadWritePaths=$state_dir|" "$src" >"$unit.tmp"
	[ -z "$instance" ] || sed -i 's|^Description=.*|& (%i)|' "$unit.tmp"
	chmod 0644 "$unit.tmp"
	mv "$unit.tmp" "$unit"
	systemctl daemon-reload
	say "installed $unit (User=$user)"

	if [ -z "$start" ]; then
		say "not starting it (--no-start); when you are ready: systemctl enable --now $svc"
		return 0
	fi
	systemctl enable --now "$svc"
	sleep 1
	if systemctl is-active --quiet "$svc"; then
		local summary listen
		summary=$("$mesh" config check -config "$target")
		# `config check` prints the resolved listen addresses; take the first.
		listen=$(printf '%s\n' "$summary" | sed -n 's/^listen \([^,]*\).*/\1/p' | head -n1)
		case "$listen" in
		0.0.0.0:*) listen=127.0.0.1:${listen##*:} ;;
		\[::\]:*) listen=[::1]:${listen##*:} ;;
		esac
		say "$svc is running"
		say "peers: $(peers_hint "$target" "$summary" "http://$listen/loadb/peers")"
	else
		systemctl --no-pager --lines=20 status "$svc" || true
		die "the service did not start"
	fi
}

# peers_hint CONFIG SUMMARY URL prints a command that fetches URL with the
# admin token the agent uses. SUMMARY (from `config check`) says whether there
# is one; the config says where it comes from, in the agent's order:
# admin_token, then admin_token_file, then algod.admin.token in the data dir.
peers_hint() {
	case "$2" in
	*"no tokens: open to anyone"*)
		echo "curl $3"
		return
		;;
	*"admin token none"*)
		echo "none: $1 has no admin token, so only /loadb/health is open; set admin_token_file to read the rest"
		return
		;;
	esac
	local file
	if [ -n "$(yaml_value admin_token "$1")" ]; then
		echo "curl -H \"X-Algo-API-Token: <admin_token from $1>\" $3"
		return
	fi
	file=$(yaml_value admin_token_file "$1")
	[ -n "$file" ] || file=$(printf '%s\n' "$2" | sed -n 's/.*, data dir \(.*\)$/\1/p' | head -n1)/algod.admin.token
	echo "curl -H \"X-Algo-API-Token: \$(sudo cat $file)\" $3"
}

# yaml_value KEY FILE prints the value of top-level KEY in FILE, without
# quotes or a trailing comment.
yaml_value() {
	sed -n "s/^$1:[[:space:]]*//p" "$2" | head -n1 |
		sed -e 's/[[:space:]]#.*//' -e 's/[[:space:]]*$//' -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'$/\1/"
}

# help prints this file's header comment, without the hashes.
help() { awk 'NR > 1 && !/^#/ { exit } NR > 1 { sub(/^# ?/, ""); print }' "$0"; }

# read_instance MESH CONFIG sets, from what `config check` resolves for a
# config: I_DIR, the algod data dir it serves (empty for a balancer); I_PORTS,
# every port it binds (client listeners, gossip, pass-through); I_CLIENT and
# I_MESH, its first client and gossip ports; I_ID and I_CACHE, its node id and
# registry cache (empty without one). It fails when the config does not load.
read_instance() {
	local out listen gossip passthrough
	I_DIR="" I_PORTS="" I_CLIENT="" I_MESH="" I_ID="" I_CACHE=""
	out=$("$1" config check -config "$2" 2>/dev/null) || return 1
	I_DIR=$(printf '%s\n' "$out" | sed -n 's/.*, data dir \(.*\)$/\1/p' | head -n1)
	[ -z "$I_DIR" ] || I_DIR=$(realpath -m "$I_DIR")
	I_ID=$(printf '%s\n' "$out" | sed -n 's/^id \([^,]*\),.*/\1/p' | head -n1)
	I_CACHE=$(printf '%s\n' "$out" | sed -n 's/^registry .*, cache \([^,]*\),.*/\1/p' | head -n1)
	listen=$(printf '%s\n' "$out" | sed -n -e 's/^listen \(.*\), advertising .*/\1/p' -e '/, advertising /!s/^listen \(.*\)/\1/p')
	gossip=$(printf '%s\n' "$out" | sed -n 's/^gossip on \(.*\), heartbeats to .*/\1/p')
	passthrough=$(printf '%s\n' "$out" | sed -n 's/^passthrough \(.*\)/\1/p')
	I_CLIENT=$(ports_of "$listen" | head -n1)
	I_MESH=$(ports_of "$gossip" | head -n1)
	I_PORTS=$({ ports_of "$listen"; ports_of "$gossip"; ports_of "$passthrough"; } | sort -un | tr '\n' ' ')
}

# ports_of "ADDR, ADDR..." prints each address's port, one per line.
ports_of() { printf '%s\n' "$1" | tr ',' '\n' | sed -n 's/.*:\([0-9][0-9]*\) *$/\1/p'; }

# check_clash CONFIG dies when the config would share an algod, a port, a node
# id or a registry cache with another agent on this host (read_instance of
# every other config filled other_*; main's locals, seen through bash's
# dynamic scope).
check_clash() {
	read_instance "$mesh" "$1" || die "$1 does not load"
	local f p
	for f in ${other_dirs[@]+"${other_dirs[@]}"}; do
		[ "$f" != "$I_DIR" ] || die "another agent in $CONFIG_DIR already serves $f"
	done
	for p in $I_PORTS; do
		! has_port "$other_ports" "$p" || die "port $p is already bound by another agent in $CONFIG_DIR; pass --client-port/--mesh-port or fix --listen"
	done
	! has_port "$other_ids" "$I_ID" || die "id $I_ID is already another agent's in $CONFIG_DIR; pass --id, or set local.id"
	[ -z "$I_CACHE" ] || ! has_port "$other_caches" "$I_CACHE" ||
		die "registry cache $I_CACHE is already another agent's in $CONFIG_DIR"
}

# has_port " WORD... " WORD reports whether WORD is in the list.
has_port() { case "$1" in *" $2 "*) return 0 ;; esac; return 1; }

# need_value "$@" dies when the option in $1 has no value after it.
need_value() { [ $# -ge 2 ] || die "$1 needs a value"; }

# has_arg FLAG reports whether FLAG is among the arguments for autoconfig.
has_arg() {
	local a
	for a in ${autoconfig[@]+"${autoconfig[@]}"}; do
		[ "$a" != "$1" ] || return 0
	done
	return 1
}

# instance_name CONFIG_DIR DIR suggests an instance name for a data dir: its
# genesis network (testnet, betanet, ...), else the dir's name, with -2, -3,
# ... when CONFIG_DIR already has a config by that name. Empty when nothing
# usable is left.
instance_name() {
	local n i=1 name
	n=$(sed -n 's/.*"network"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$2/genesis.json" 2>/dev/null | head -n1)
	[ -n "$n" ] || n=$(basename "$2")
	n=$(printf '%s' "$n" | tr 'A-Z_.' 'a-z--' | tr -cd 'a-z0-9-' | sed 's/^-*//' | cut -c1-28)
	[ -n "$n" ] || return 0
	name=$n
	while [ "$name" = config ] || [ -e "$1/$name.yaml" ]; do
		i=$((i + 1))
		name=$n-$i
	done
	printf '%s' "$name"
}

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
