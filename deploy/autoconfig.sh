#!/usr/bin/env bash
# Writes an algod-loadb-mesh config for this host from what it can detect:
#   local.id             hostname, without a trailing .local
#   local.data_dir       the algod data dir (running algod -d, $ALGORAND_DATA,
#                        common locations); asks when there are several
#   advertise addresses  a 10.112.* interface, then 10.114.*, then a public one
# The registry app id and sync key come from flags or from a bundle printed by
# `algod-loadb-mesh registry bundle` on a host that already runs.
# Everything else keeps the agent's defaults; `algod-loadb-mesh config example`
# lists all options.
set -euo pipefail

usage() {
	cat <<EOF
usage: $0 [options] [BUNDLE]

  BUNDLE                output of \`algod-loadb-mesh registry bundle\`: base64 of
                        app id (8 bytes, big endian) | sync address (32 bytes,
                        only if rekeyed) | sync key seed (32 bytes). The sync key
                        is written inline. "-" reads it from stdin, which keeps
                        it out of shell history and ps.

  -o FILE               write the config to FILE (mode 0600) instead of stdout
  -f                    overwrite FILE if it exists
  --id ID               node id (default: hostname without .local)
  --data-dir DIR        algod data dir (default: detected)
  --address IP          address other agents reach this host on (default: detected)
  --app-id N            registry application id (default: from BUNDLE, else 0)
  --sync-key-file FILE  registry sync key, instead of BUNDLE (default: /etc/algod-loadb-mesh/sync.key)
  --sync-address ADDR   sync account, when it is rekeyed to the sync key (default: from BUNDLE)
  --client-token-file FILE
                        token clients must send (default: algod.token in the data dir)
  --tier N              routing tier, lower is preferred (default: 1)
  --listen HOST:PORT    client listener (default: 0.0.0.0:4000)
  --mesh-port PORT      UDP heartbeat port (default: 4001)
EOF
}

out="" force=0 id="" data_dir="" address="" app_id="" sync_address="" tier=1
sync_key_file="" sync_key="" bundle="" client_token_file=""
listen=0.0.0.0:4000 mesh_port=4001

while [ $# -gt 0 ]; do
	case "$1" in
	-o) out=$2; shift ;;
	-f) force=1 ;;
	--id) id=$2; shift ;;
	--data-dir) data_dir=$2; shift ;;
	--address) address=$2; shift ;;
	--app-id) app_id=$2; shift ;;
	--sync-key-file) sync_key_file=$2; shift ;;
	--sync-address) sync_address=$2; shift ;;
	--client-token-file) client_token_file=$2; shift ;;
	--tier) tier=$2; shift ;;
	--listen) listen=$2; shift ;;
	--mesh-port) mesh_port=$2; shift ;;
	-h | --help) usage; exit 0 ;;
	-?*) usage >&2; exit 2 ;;
	*) [ -z "$bundle" ] || { usage >&2; exit 2; }; bundle=$1 ;;
	esac
	shift
done

warn() { echo "autoconfig: $*" >&2; }
die() { warn "$*"; exit 1; }

[ -z "$out" ] || [ "$force" = 1 ] || [ ! -e "$out" ] || die "$out exists (use -f to overwrite)"

# --- registry bundle --------------------------------------------------------

hex2bin() { printf '%b' "$(printf '%s' "$1" | sed 's/../\\x&/g')"; }

# Algorand address of a hex public key: base32(key | last 4 bytes of
# SHA-512/256(key)), unpadded.
address_of() {
	local sum
	sum=$(hex2bin "$1" | openssl dgst -sha512-256 -binary 2>/dev/null | od -An -v -tx1 | tr -d ' \n') || sum=""
	[ ${#sum} = 64 ] || die "openssl with SHA-512/256 is needed to decode a rekeyed bundle"
	hex2bin "$1${sum:56:8}" | base32 | tr -d '=\n'
}

if [ -n "$bundle" ]; then
	[ -z "$sync_key_file" ] || die "pass either BUNDLE or --sync-key-file"
	if [ "$bundle" = - ]; then
		read -r bundle || [ -n "$bundle" ] || die "no bundle on stdin"
	fi
	b64=$(printf '%s' "$bundle" | tr -d ' \r\n=' | tr '_-' '/+')
	while [ $((${#b64} % 4)) != 0 ]; do b64+="="; done
	hex=$(printf '%s' "$b64" | base64 -d 2>/dev/null | od -An -v -tx1 | tr -d ' \n') || die "bundle is not base64"
	case ${#hex} in
	80) ;;
	144) [ -n "$sync_address" ] || sync_address=$(address_of "${hex:16:64}") ;;
	*) die "bundle is $((${#hex} / 2)) bytes, want 40 or 72" ;;
	esac
	case ${hex:0:1} in [0-7]) ;; *) die "bundle app id out of range" ;; esac
	[ -n "$app_id" ] || app_id=$((16#${hex:0:16}))
	sync_key=${hex: -64}
fi
[ -n "$app_id" ] || app_id=0
[ -n "$sync_key" ] || [ -n "$sync_key_file" ] || sync_key_file=/etc/algod-loadb-mesh/sync.key

# --- id ---------------------------------------------------------------------

if [ -z "$id" ]; then
	id=$(hostname)
	id=${id%.local}
fi
[ -n "$id" ] || die "no hostname; pass --id"
[ "${#id}" -le 48 ] || die "id \"$id\" is longer than 48 bytes; pass --id"

# --- algod data dir ---------------------------------------------------------

is_data_dir() { [ -f "$1/genesis.json" ]; }

detect_data_dirs() {
	local pid args dir
	# Running nodes: `algod -d DIR`. Paths inside containers usually do not
	# exist on the host and are skipped by the check below.
	for pid in $(pgrep -x algod 2>/dev/null || true); do
		args=$(tr '\0' '\n' <"/proc/$pid/cmdline" 2>/dev/null) || continue
		dir=$(printf '%s\n' "$args" | awk 'prev == "-d" { print; exit } { prev = $0 }')
		[ -n "$dir" ] || dir=$(tr '\0' '\n' <"/proc/$pid/environ" 2>/dev/null | sed -n 's/^ALGORAND_DATA=//p')
		[ -n "$dir" ] && echo "$dir"
	done
	[ -n "${ALGORAND_DATA:-}" ] && echo "$ALGORAND_DATA"
	echo /var/lib/algorand
	for dir in /var/lib/algorand/*/ /opt/algorand/node/data "$HOME/node/data" /algod/data; do
		echo "${dir%/}"
	done
}

if [ -z "$data_dir" ]; then
	candidates=()
	while IFS= read -r dir; do
		is_data_dir "$dir" || continue
		dir=$(realpath "$dir")
		case " ${candidates[*]-} " in *" $dir "*) continue ;; esac
		candidates+=("$dir")
	done < <(detect_data_dirs)

	case ${#candidates[@]} in
	0) die "no algod data dir found (looked for genesis.json); pass --data-dir" ;;
	1) data_dir=${candidates[0]} ;;
	*)
		if ! { : </dev/tty; } 2>/dev/null; then
			die "several algod data dirs (${candidates[*]}); pass --data-dir"
		fi
		echo "Several algod data dirs found:" >/dev/tty
		for i in "${!candidates[@]}"; do
			net=$(cat "${candidates[$i]}/algod.net" 2>/dev/null || echo "not running")
			printf '  %d) %s  (%s)\n' $((i + 1)) "${candidates[$i]}" "$net" >/dev/tty
		done
		while :; do
			printf 'Use which? [1-%d] ' ${#candidates[@]} >/dev/tty
			read -r n </dev/tty || die "no choice made"
			if [[ "$n" =~ ^[0-9]+$ ]] && [ "$n" -ge 1 ] && [ "$n" -le ${#candidates[@]} ]; then
				data_dir=${candidates[$((n - 1))]}
				break
			fi
		done
		;;
	esac
fi
is_data_dir "$data_dir" || die "$data_dir is not an algod data dir (no genesis.json)"

# Where algod listens: algod.net of a running node, else config.json, else 8080.
algod_listen=$(cat "$data_dir/algod.net" 2>/dev/null || true)
if [ -z "$algod_listen" ] && [ -f "$data_dir/config.json" ]; then
	algod_listen=$(sed -n 's/.*"EndpointAddress"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$data_dir/config.json" | head -n1)
fi
[ -n "$algod_listen" ] || algod_listen=127.0.0.1:8080
algod_host=${algod_listen%:*}
algod_host=${algod_host#[}
algod_host=${algod_host%]}
algod_port=${algod_listen##*:}
[ -n "$algod_port" ] || algod_port=8080

# --- addresses --------------------------------------------------------------

is_private4() {
	case "$1" in
	10.* | 127.* | 192.168.* | 169.254.*) return 0 ;;
	172.*) local b=${1#172.}; b=${b%%.*}; [ "$b" -ge 16 ] && [ "$b" -le 31 ] ;;
	100.*) local b=${1#100.}; b=${b%%.*}; [ "$b" -ge 64 ] && [ "$b" -le 127 ] ;;
	*) return 1 ;;
	esac
}

detect_address() {
	local v4 v6 ip
	v4=$(ip -4 -o addr show scope global 2>/dev/null | awk '{ sub(/\/.*/, "", $4); print $4 }')
	for prefix in 10.112. 10.114.; do
		ip=$(printf '%s\n' "$v4" | awk -v p="$prefix" 'index($0, p) == 1 { print; exit }')
		[ -n "$ip" ] && { echo "$ip"; return; }
	done
	for ip in $v4; do
		is_private4 "$ip" || { echo "$ip"; return; }
	done
	# Public IPv6: global scope, not unique-local (fc00::/7).
	v6=$(ip -6 -o addr show scope global 2>/dev/null | awk '{ sub(/\/.*/, "", $4); print $4 }')
	for ip in $v6; do
		case "$ip" in [fF][cCdD]*) ;; *) echo "$ip"; return ;; esac
	done
}

if [ -z "$address" ]; then
	address=$(detect_address)
	[ -n "$address" ] || die "no 10.112.*, 10.114.* or public address on any interface; pass --address"
fi

hostport() { case "$1" in *:*) echo "[$1]:$2" ;; *) echo "$1:$2" ;; esac; }

# algod bound to one specific address is only reachable there.
endpoint_host=$address
case "$algod_host" in
"" | 0.0.0.0 | :: | "*") ;;
127.* | ::1 | localhost)
	warn "algod listens on $algod_listen only; other agents cannot reach it at $address:$algod_port (set EndpointAddress in $data_dir/config.json)"
	;;
*)
	if [ "$algod_host" != "$address" ]; then
		warn "algod listens on $algod_host, not $address; advertising $algod_host for algod"
		endpoint_host=$algod_host
	fi
	;;
esac

client_token_file=${client_token_file:-$data_dir/algod.token}

# --- checks -----------------------------------------------------------------

[ -z "$sync_key_file" ] || [ -e "$sync_key_file" ] || warn "$sync_key_file does not exist yet (algod-loadb-mesh registry gen-key)"
[ -e "$client_token_file" ] || warn "$client_token_file does not exist yet (e.g. openssl rand -hex 32 > $client_token_file)"
[ "$app_id" != 0 ] || warn "registry.app_id is 0; set it to the id printed by \`algod-loadb-mesh registry init\`"
[ -r "$data_dir/algod.token" ] || warn "$data_dir/algod.token is not readable by $(id -un); the agent's user needs it"

# --- config -----------------------------------------------------------------

config() {
	cat <<EOF
# Generated by autoconfig.sh on $(hostname) at $(date -u +%Y-%m-%dT%H:%M:%SZ).
# All options: algod-loadb-mesh config example

mode: fallback
listen: $listen
client_token_file: $client_token_file

local:
  id: $id
  data_dir: $data_dir
  advertise_endpoints:
    - http://$(hostport "$endpoint_host" "$algod_port")
  tier: $tier

registry:
  type: algorand
  app_id: $app_id
EOF
	if [ -n "$sync_key" ]; then
		echo "  sync_key: \"$sync_key\""
	else
		echo "  sync_key_file: $sync_key_file"
	fi
	[ -z "$sync_address" ] || echo "  sync_address: $sync_address"
	cat <<EOF
  cache: /var/lib/algod-loadb-mesh/registry.cache

mesh:
  listen: $(hostport "$([[ "$address" == *:* ]] && echo :: || echo 0.0.0.0)" "$mesh_port")
  advertise: $(hostport "$address" "$mesh_port")
EOF
}

if [ -z "$out" ]; then
	config
else
	(umask 077 && config >"$out")
	warn "wrote $out"
fi
