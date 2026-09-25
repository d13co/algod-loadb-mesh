#!/bin/sh
# Add .debs to the apt repo at https://apt.d13.co, then rebuild and sign its index.
# Usage: apt-publish.sh [pkg.deb ...]    (no args: just re-index and re-sign)
#
# The release workflow runs it on every tag; ~/code/infra/apt/publish.sh runs it by hand
# (the algorand .debs). Either way it first pulls the bucket's pool, so the index lists
# every package in the bucket, not only the ones on this machine.
#
#   APT_REPO             local copy of the repo tree (default: ./apt-repo)
#   R2_ACCOUNT_ID        and aws credentials for the "apt" bucket (AWS_PROFILE=r2 by hand,
#                        AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY in CI)
#   GPG_PASSPHRASE_FILE  signing subkey passphrase, for gpg without a pinentry (CI)
set -eu
KEY=5B708A29E1D79C75016BBCE4B4C52CB3D4B85B22!   # CI signing subkey
REPO=${APT_REPO:-apt-repo}
BUCKET=s3://apt   # R2 bucket, served at https://apt.d13.co
R2="--endpoint-url https://${R2_ACCOUNT_ID:?set R2_ACCOUNT_ID}.r2.cloudflarestorage.com --only-show-errors"
export AWS_DEFAULT_REGION=auto

mkdir -p "$REPO/pool"
# ponytail: pulls the whole pool (algorand .debs are ~160MB each); merge Packages stanzas
# instead once that gets slow.
aws s3 sync $BUCKET/pool "$REPO/pool" $R2
# apt-ftparchive --arch filters on the filename, so name each .deb from its own fields.
for f in "$@"; do
  cp "$f" "$REPO/pool/$(dpkg-deb -f "$f" Package)_$(dpkg-deb -f "$f" Version)_$(dpkg-deb -f "$f" Architecture).deb"
done

cd "$REPO"
d=dists/stable
ARCHES="amd64 arm64"
for a in $ARCHES; do   # --arch also takes Architecture: all packages
  mkdir -p $d/main/binary-$a
  apt-ftparchive --arch $a packages pool > $d/main/binary-$a/Packages
  gzip -9kf $d/main/binary-$a/Packages
done
rm -f $d/Release $d/InRelease   # else the new Release hashes the old one
apt-ftparchive \
  -o APT::FTPArchive::Release::Suite=stable \
  -o APT::FTPArchive::Release::Codename=stable \
  -o APT::FTPArchive::Release::Components=main \
  -o APT::FTPArchive::Release::Architectures="$ARCHES" \
  release $d > Release.tmp
mv Release.tmp $d/Release
gpg --yes ${GPG_PASSPHRASE_FILE:+--batch --pinentry-mode loopback --passphrase-file "$GPG_PASSPHRASE_FILE"} \
  -u "$KEY" --clearsign -o $d/InRelease $d/Release

# Pool first, so the new index never points at a .deb that isn't up yet.
# The index gets a short cache so Cloudflare serves a new release within a minute.
# ponytail: never deletes old .debs from the bucket; prune there by hand if it grows.
aws s3 sync pool $BUCKET/pool $R2
aws s3 sync dists $BUCKET/dists $R2 --cache-control max-age=60
# key.asc changes only when the subkey is renewed, and that's done by hand.
if [ -f key.asc ]; then aws s3 cp key.asc $BUCKET/key.asc $R2; fi
