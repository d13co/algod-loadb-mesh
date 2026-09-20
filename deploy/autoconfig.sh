#!/usr/bin/env bash
# Writes an algod-loadb-mesh config for this host from what it can detect:
#   local.id             hostname, without a trailing .local
#   local.data_dir       the algod data dir (running algod -d, $ALGORAND_DATA,
#                        common locations); asks when there are several
#   addresses            every 10.112.* and 10.114.* interface, else a public
#                        one. Clients and gossip are served on all of them and
#                        peers are told all of them (the mesh binds what it
#                        advertises); the first hosts the algod endpoint. With
#                        no such address clients and gossip are served on every
#                        interface.
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
  --address IP[,IP]     addresses other agents reach this host on, preferred
                        first; repeatable (default: every 10.112.*/10.114.*
                        address, else a public one)
  --app-id N            registry application id (default: from BUNDLE, else 0)
  --sync-key-file FILE  registry sync key, instead of BUNDLE (default: /etc/algod-loadb-mesh/sync.key)
  --sync-address ADDR   sync account, when it is rekeyed to the sync key (default: from BUNDLE)
  --client-token-file FILE
                        token clients must send (default: algod.token in the data dir)
  --tier N              routing tier, lower is preferred (default: 1)
  --listen ADDR[,ADDR]  client listeners, each HOST or HOST:PORT; repeatable
                        (default: the 10.112.*/10.114.* addresses, else 0.0.0.0)
  --client-port PORT    port for listeners given without one (default: 4000)
  --mesh-port PORT      UDP heartbeat port (default: 4001)
  --nodely              add Nodely's public API for the node's network as the
                        last-resort external tier (asked on a terminal when
                        neither --nodely nor --no-nodely is given)
  --no-nodely           do not
EOF
}

out="" force=0 id="" data_dir="" address="" app_id="" sync_address="" tier=1
sync_key_file="" sync_key="" bundle="" client_token_file=""
listen="" client_port=4000 mesh_port=4001 nodely=""

while [ $# -gt 0 ]; do
	case "$1" in
	-o) out=$2; shift ;;
	-f) force=1 ;;
	--id) id=$2; shift ;;
	--data-dir) data_dir=$2; shift ;;
	--address) address="${address:+$address,}$2"; shift ;;
	--app-id) app_id=$2; shift ;;
	--sync-key-file) sync_key_file=$2; shift ;;
	--sync-address) sync_address=$2; shift ;;
	--client-token-file) client_token_file=$2; shift ;;
	--tier) tier=$2; shift ;;
	--listen) listen="${listen:+$listen,}$2"; shift ;;
	--client-port) client_port=$2; shift ;;
	--mesh-port) mesh_port=$2; shift ;;
	--nodely) nodely=1 ;;
	--no-nodely) nodely=0 ;;
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

# global_v4 prints the IPv4 address of every global-scope interface.
global_v4() { ip -4 -o addr show scope global 2>/dev/null | awk '{ sub(/\/.*/, "", $4); print $4 }'; }

detect_address() {
	local v4 v6 ip
	v4=$(global_v4)
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

# Every mesh address, 10.112.* before 10.114.*: what the client listener binds
# when nothing is passed.
detect_listen() {
	local v4 a prefix found=""
	v4=$(global_v4)
	for prefix in 10.112. 10.114.; do
		for a in $v4; do
			case "$a" in "$prefix"*) found+="${found:+ }$a" ;; esac
		done
	done
	echo "$found"
}

# The mesh binds what it advertises: every --address given, else every
# detected mesh address. Only the no-WireGuard fallback (a public address)
# differs: gossip listens on every interface and advertises that one.
mesh_bind=1
if [ -z "$address" ]; then
	address=$(detect_listen | tr ' ' ',')
	if [ -z "$address" ]; then
		address=$(detect_address)
		mesh_bind=0
	fi
	[ -n "$address" ] || die "no 10.112.*, 10.114.* or public address on any interface; pass --address"
fi
# Addresses hold no spaces, so splitting the list on them is safe.
mesh_addrs=()
for a in $(printf '%s' "$address" | tr ',' ' '); do
	mesh_addrs+=("$a")
done
address=${mesh_addrs[0]}

hostport() { case "$1" in *:*) echo "[$1]:$2" ;; *) echo "$1:$2" ;; esac; }

# with_port ADDR PORT appends the port to an address that has none.
with_port() {
	case "$1" in
	\[*\]:*) echo "$1" ;;          # [fd00::1]:4000
	\[*\]) echo "$1:$2" ;;         # [fd00::1]
	*:*:*) hostport "$1" "$2" ;; # a bare IPv6
	*:*) echo "$1" ;;              # host:port
	*) echo "$1:$2" ;;             # a bare IPv4 or hostname
	esac
}

listen_addrs=()
for a in $(printf '%s' "${listen:-$(detect_listen)}" | tr ',' ' '); do
	listen_addrs+=("$(with_port "$a" "$client_port")")
done
if [ ${#listen_addrs[@]} = 0 ]; then
	listen_addrs=("$(hostport 0.0.0.0 "$client_port")")
	warn "no 10.112.* or 10.114.* address; serving clients on every interface (${listen_addrs[0]})"
fi

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

# --- nodely fallback --------------------------------------------------------

# Network from genesis.json ("mainnet", "testnet", "betanet", ...).
network=$(sed -n 's/.*"network"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$data_dir/genesis.json" | head -n1)
nodely_url=""
case "$network" in
mainnet | testnet | betanet) nodely_url=https://$network-api.4160.nodely.dev ;;
esac
if [ -z "$nodely" ] && [ -n "$nodely_url" ] && { : </dev/tty; } 2>/dev/null; then
	printf 'Use Nodely (%s) as the last-resort fallback? [Y/n] ' "$nodely_url" >/dev/tty
	read -r yn </dev/tty || yn=""
	case "$yn" in [nN]*) nodely=0 ;; *) nodely=1 ;; esac
fi
if [ "$nodely" = 1 ] && [ -z "$nodely_url" ]; then
	warn "no Nodely API for network \"${network:-unknown}\"; skipping --nodely"
	nodely=0
fi

# --- checks -----------------------------------------------------------------

[ -z "$sync_key_file" ] || [ -e "$sync_key_file" ] || warn "$sync_key_file does not exist yet (algod-loadb-mesh registry gen-key)"
[ -e "$client_token_file" ] || warn "$client_token_file does not exist yet (e.g. openssl rand -hex 32 > $client_token_file)"
[ "$app_id" != 0 ] || warn "registry.app_id is 0; set it to the id printed by \`algod-loadb-mesh registry init\`"
[ -r "$data_dir/algod.token" ] || warn "$data_dir/algod.token is not readable by $(id -un); the agent's user needs it"

# --- config -----------------------------------------------------------------

# addr_list_yaml KEY INDENT ADDR... prints an address option: one address
# inline, several as a list.
addr_list_yaml() {
	local key=$1 indent=$2
	shift 2
	if [ $# = 1 ]; then
		echo "${indent}${key}: $1"
	else
		echo "${indent}${key}:"
		printf "${indent}  - %s\n" "$@"
	fi
}

mesh_advertise=() mesh_listen=()
for a in "${mesh_addrs[@]}"; do
	mesh_advertise+=("$(hostport "$a" "$mesh_port")")
done
if [ "$mesh_bind" = 1 ]; then
	mesh_listen=("${mesh_advertise[@]}")
else
	mesh_listen=("$(hostport "$([[ "$address" == *:* ]] && echo :: || echo 0.0.0.0)" "$mesh_port")")
	warn "no 10.112.* or 10.114.* address; gossip listens on every interface (${mesh_listen[0]}) and advertises $address"
fi

config() {
	cat <<EOF
# Generated by autoconfig.sh on $(hostname) at $(date -u +%Y-%m-%dT%H:%M:%SZ).
# All options: algod-loadb-mesh config example

mode: fallback
$(addr_list_yaml listen "" "${listen_addrs[@]}")
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
$(addr_list_yaml listen "  " "${mesh_listen[@]}")
$(addr_list_yaml advertise "  " "${mesh_advertise[@]}")
EOF
	[ "$nodely" = 1 ] || return 0
	cat <<EOF

tiers:
  external:
    - name: nodely
      url: $nodely_url
      capabilities: { archival: { kind: full } }
EOF
}

if [ -z "$out" ]; then
	config
else
	(umask 077 && config >"$out")
	warn "wrote $out"
fi
