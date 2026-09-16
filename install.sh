#!/usr/bin/env bash
# Installs algod-loadb-mesh on a linux/amd64 host:
#
#   curl -fsSL https://raw.githubusercontent.com/d13co/algod-loadb-mesh/stable/install.sh | sudo bash
#
# It fetches the release binary and the deploy scripts, and installs them as
# algod-loadb-mesh, algod-loadb-mesh-autoconfig and algod-loadb-mesh-setup.
# Nothing is configured or started; that is algod-loadb-mesh-setup, which you
# can run straight away by ending this command with --setup:
#
#   curl -fsSL .../install.sh | sudo bash -s -- --setup BUNDLE
#
# Options (also settable as environment variables):
#
#   --version TAG    release to install (VERSION, default: the latest release)
#   --ref REF        branch or tag the scripts come from (REF, default: stable)
#   --repo OWNER/REPO                       (REPO, default: d13co/algod-loadb-mesh)
#   --prefix DIR     where the commands go  (PREFIX, default: /usr/local/bin)
#   --share DIR      where the unit file goes (SHARE, default: /usr/local/share/algod-loadb-mesh)
#   --setup [ARGS]   run algod-loadb-mesh-setup afterwards; everything after it
#                    is passed on (see algod-loadb-mesh-setup -h)
set -euo pipefail

main() {
	REPO=${REPO:-d13co/algod-loadb-mesh}
	REF=${REF:-stable}
	VERSION=${VERSION:-latest}
	PREFIX=${PREFIX:-/usr/local/bin}
	SHARE=${SHARE:-/usr/local/share/algod-loadb-mesh}
	local asset=algod-loadb-mesh-linux-amd64 setup="" setup_args=()

	while [ $# -gt 0 ]; do
		case "$1" in
		--version) VERSION=$2; shift ;;
		--ref) REF=$2; shift ;;
		--repo) REPO=$2; shift ;;
		--prefix) PREFIX=$2; shift ;;
		--share) SHARE=$2; shift ;;
		--setup) setup=1; shift; setup_args=("$@"); break ;;
		-h | --help) help; return 0 ;;
		*) die "unknown option $1 (--setup passes the rest to the setup script)" ;;
		esac
		shift
	done

	[ "$(uname -s)" = Linux ] || die "$(uname -s) is not supported yet (linux/amd64 only)"
	case "$(uname -m)" in
	x86_64 | amd64) ;;
	*) die "$(uname -m) is not supported yet (linux/amd64 only)" ;;
	esac
	command -v curl >/dev/null || command -v wget >/dev/null || die "curl or wget is required"

	local sudo=""
	if [ "$(id -u)" != 0 ]; then
		command -v sudo >/dev/null || die "run as root (or install sudo)"
		sudo=sudo
		say "not root; using sudo"
	fi

	# Not local: the EXIT trap runs after main returns.
	tmp=$(mktemp -d)
	trap 'rm -rf "${tmp:-}"' EXIT

	# --- binary -------------------------------------------------------------

	local base=${RELEASES_URL:-https://github.com/$REPO/releases}
	if [ "$VERSION" = latest ]; then
		base=$base/latest/download
	else
		base=$base/download/$VERSION
	fi
	say "downloading $asset ($VERSION)"
	fetch "$base/$asset" "$tmp/$asset" || die "no $asset in the $VERSION release of $REPO"
	if fetch "$base/checksums.txt" "$tmp/checksums.txt" && grep -q " $asset\$" "$tmp/checksums.txt"; then
		(cd "$tmp" && grep " $asset\$" checksums.txt | sha256sum -c --status) ||
			die "$asset does not match its checksum"
		say "checksum ok"
	else
		say "no checksums.txt in the release; skipping verification"
	fi
	chmod 0755 "$tmp/$asset"
	"$tmp/$asset" version >/dev/null || die "the downloaded binary does not run"

	# --- scripts ------------------------------------------------------------

	local raw=${RAW_URL:-https://raw.githubusercontent.com/$REPO/$REF}
	local f
	for f in autoconfig.sh setup.sh algod-loadb-mesh.service; do
		fetch "$raw/deploy/$f" "$tmp/$f" || die "cannot fetch deploy/$f from $REF"
	done

	# --- install ------------------------------------------------------------

	$sudo install -d -m 0755 "$PREFIX" "$SHARE"
	$sudo install -m 0755 "$tmp/$asset" "$PREFIX/algod-loadb-mesh"
	$sudo install -m 0755 "$tmp/autoconfig.sh" "$PREFIX/algod-loadb-mesh-autoconfig"
	$sudo install -m 0755 "$tmp/setup.sh" "$PREFIX/algod-loadb-mesh-setup"
	$sudo install -m 0644 "$tmp/algod-loadb-mesh.service" "$SHARE/algod-loadb-mesh.service"
	say "installed algod-loadb-mesh $("$tmp/$asset" version | awk '{print $2}'), algod-loadb-mesh-autoconfig and algod-loadb-mesh-setup in $PREFIX"

	if [ -n "$setup" ]; then
		exec $sudo env PREFIX="$PREFIX" SHARE="$SHARE" REPO="$REPO" REF="$REF" RAW_URL="${RAW_URL:-}" \
			"$PREFIX/algod-loadb-mesh-setup" ${setup_args[@]+"${setup_args[@]}"}
	fi

	cat <<EOF

Next, set the host up (unit file, config, service). On a host that already
runs the mesh, print its registry bundle to paste into the setup:

    algod-loadb-mesh registry bundle -config /etc/algod-loadb-mesh/config.yaml

    sudo algod-loadb-mesh-setup -        # paste the bundle, then Ctrl-D

On the first host of a fleet there is no bundle yet. Create the sync account,
fund it with a few ALGO, put the mnemonic in a file, and set up with it:

    algod-loadb-mesh registry gen-key
    sudo install -m 0600 /dev/null /etc/algod-loadb-mesh/sync.key   # paste the mnemonic
    sudo algod-loadb-mesh-setup --no-start --sync-key-file /etc/algod-loadb-mesh/sync.key
    sudo algod-loadb-mesh registry init -config /etc/algod-loadb-mesh/config.yaml

Put the app id it prints into the config (registry.app_id), then start it:

    sudo systemctl enable --now algod-loadb-mesh

algod-loadb-mesh-setup -h lists its options, including --config FILE to
install a config you already have.
EOF
}

# help prints this file's header comment, without the hashes.
help() { sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//'; }

say() { echo "algod-loadb-mesh: $*" >&2; }
die() { say "$*"; exit 1; }

# fetch URL FILE, quietly, failing on a 404.
fetch() {
	if command -v curl >/dev/null; then
		curl -fsL --retry 3 -o "$2" "$1"
	else
		wget -qO "$2" "$1"
	fi
}

main "$@"
