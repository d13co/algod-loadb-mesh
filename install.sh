#!/usr/bin/env bash
# Installs algod-loadb-mesh on a linux/amd64 or linux/arm64 host:
#
#   curl -fsSL https://raw.githubusercontent.com/d13co/algod-loadb-mesh/stable/install.sh | bash
#
# On Debian and Ubuntu it adds the apt repository at https://apt.d13.co (after
# checking its key's fingerprint when gpg is available) and installs the
# algod-loadb-mesh package, so later upgrades come with `apt upgrade`.
# Elsewhere, or with --no-apt, it fetches the release binary and the deploy
# scripts instead. Either way you get algod-loadb-mesh,
# algod-loadb-mesh-autoconfig and algod-loadb-mesh-setup.
# Run it as your user: it downloads and verifies as you, and uses sudo only to
# install (and to run the setup).
# Nothing is configured or started; that is algod-loadb-mesh-setup, which you
# can run straight away by ending this command with --setup:
#
#   curl -fsSL .../install.sh | bash -s -- --setup BUNDLE
#
# Options (also settable as environment variables):
#
#   --version TAG    release to install (VERSION, default: the latest release)
#   --no-apt         install the release binary even where apt exists (NO_APT=1)
#   --repo OWNER/REPO                       (REPO, default: d13co/algod-loadb-mesh)
#   --setup [ARGS]   run algod-loadb-mesh-setup afterwards; everything after it
#                    is passed on (see algod-loadb-mesh-setup -h)
#
# Without apt only (--prefix or --share imply --no-apt):
#
#   --ref REF        branch or tag the scripts come from (REF, default: stable)
#   --prefix DIR     where the commands go  (PREFIX, default: /usr/local/bin)
#   --share DIR      where the unit file goes (SHARE, default: /usr/local/share/algod-loadb-mesh)
set -euo pipefail

# The apt repository's signing key (primary key fingerprint).
KEY_FPR=F66E77133063650C159F405AC4D2171FB5B60471

main() {
	REPO=${REPO:-d13co/algod-loadb-mesh}
	REF=${REF:-stable}
	VERSION=${VERSION:-latest}
	PREFIX=${PREFIX:-/usr/local/bin}
	SHARE=${SHARE:-/usr/local/share/algod-loadb-mesh}
	APT_URL=${APT_URL:-https://apt.d13.co}
	local asset setup="" setup_args=() no_apt=${NO_APT:-}

	while [ $# -gt 0 ]; do
		case "$1" in
		--version) VERSION=$2; shift ;;
		--ref) REF=$2; shift ;;
		--repo) REPO=$2; shift ;;
		--no-apt) no_apt=1 ;;
		--prefix) PREFIX=$2; no_apt=1; shift ;;
		--share) SHARE=$2; no_apt=1; shift ;;
		--setup) setup=1; shift; setup_args=("$@"); break ;;
		-h | --help) help; return 0 ;;
		*) die "unknown option $1 (--setup passes the rest to the setup script)" ;;
		esac
		shift
	done

	[ "$(uname -s)" = Linux ] || die "$(uname -s) is not supported yet (linux only)"
	case "$(uname -m)" in
	x86_64 | amd64) asset=algod-loadb-mesh-linux-amd64 ;;
	aarch64 | arm64) asset=algod-loadb-mesh-linux-arm64 ;;
	*) die "$(uname -m) is not supported yet (amd64 and arm64 only)" ;;
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

	if [ -z "$no_apt" ] && command -v apt-get >/dev/null; then
		install_apt
	else
		install_release
	fi

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

# install_apt adds the apt repository and installs the package.
install_apt() {
	say "installing from $APT_URL (--no-apt for the release binary instead)"
	fetch "$APT_URL/key.asc" "$tmp/key.asc" || die "cannot fetch $APT_URL/key.asc"
	# The key comes over HTTPS; where gpg exists, also pin it: exactly one
	# primary key, ours (apt trusts every key in the file).
	if command -v gpg >/dev/null; then
		gpg --show-keys --with-colons "$tmp/key.asc" 2>/dev/null |
			awk -F: -v k="$KEY_FPR" '$1 == "pub" { n++; p = 1; next } $1 == "fpr" && p { f = $10 } { p = 0 } END { exit !(n == 1 && f == k) }' ||
			die "$APT_URL/key.asc is not the expected key $KEY_FPR"
		say "repository key ok"
	fi
	$sudo install -d -m 0755 /etc/apt/keyrings
	$sudo install -m 0644 "$tmp/key.asc" /etc/apt/keyrings/d13co.asc
	echo "deb [signed-by=/etc/apt/keyrings/d13co.asc] $APT_URL stable main" >"$tmp/d13co.list"
	$sudo install -m 0644 "$tmp/d13co.list" /etc/apt/sources.list.d/d13co.list
	$sudo apt-get update -qq
	local pkg=algod-loadb-mesh
	[ "$VERSION" = latest ] || pkg=$pkg=${VERSION#v}
	$sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$pkg" >/dev/null
	PREFIX=/usr/bin
	say "installed algod-loadb-mesh $("$PREFIX/algod-loadb-mesh" version | awk '{print $2}'), algod-loadb-mesh-autoconfig and algod-loadb-mesh-setup in $PREFIX"
	if [ -e /usr/local/bin/algod-loadb-mesh ]; then
		say "an older copy is still in /usr/local/bin; see \"Moving to the apt package\" in the README"
	fi
}

# install_release fetches the release binary and the deploy scripts.
install_release() {
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
}

# help prints this file's header comment, without the hashes.
help() { sed -n '2,31p' "$0" | sed 's/^# \{0,1\}//'; }

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
