#!/usr/bin/env bash
# Builds an AUR package with makepkg, then pushes its PKGBUILD and .SRCINFO.
# makepkg downloads every source and checks its sha256sum, so a PKGBUILD that
# does not build, or does not match its release assets, is never published.
#
# Usage: publish.sh <pkgname> <PKGBUILD>
# Env:   AUR_SSH_PRIVATE_KEY, AUR_USERNAME, AUR_EMAIL
#        DRY_RUN=1 builds and prints the files without pushing.
# Runs as a non-root user that may run pacman through sudo (makepkg -s).
set -euo pipefail
CARCH=$(uname -m)

pkgname=$1
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp "$2" "$work/PKGBUILD"
cd "$work"

makepkg --syncdeps --force --noconfirm
# A -git pkgver() rewrites pkgver in PKGBUILD; .SRCINFO must match it.
makepkg --printsrcinfo >.SRCINFO
pkgver=$(sed -n 's/^\tpkgver = //p' .SRCINFO)

# makepkg only fetches the sources of this machine's arch: check the others too.
while read -r arch; do
	if [[ $arch == "$CARCH" ]] || ! grep -q "^source_$arch=" PKGBUILD; then
		continue
	fi
	{ cat /etc/makepkg.conf; echo "CARCH=$arch"; } >"makepkg-$arch.conf"
	makepkg --config "makepkg-$arch.conf" --verifysource --noconfirm
done < <(sed -n 's/^\tarch = //p' .SRCINFO)

# pacman versions use _ where the tag has - (0.2.0-rc1 -> 0.2.0_rc1).
got=$("pkg/$pkgname/usr/bin/neferafk" version)
if [[ $got != "$pkgver" && $got != "${pkgver//_/-}" ]]; then
	echo "packaged neferafk reports version $got, want $pkgver" >&2
	exit 1
fi

if [[ ${DRY_RUN:-} == 1 ]]; then
	cat PKGBUILD .SRCINFO
	exit 0
fi

ssh_dir=$HOME/.ssh
key=$ssh_dir/aur
install -d -m 700 "$ssh_dir"
(umask 077 && printf '%s\n' "$AUR_SSH_PRIVATE_KEY" >"$key")
# Pinned host key. Fingerprint SHA256:RFzBCUItH9LZS0cKB5UE6ceAYhBD5C8GeOBip8Z11+4
# is published on https://aur.archlinux.org.
echo 'aur.archlinux.org ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuBKrPzbawxA/k2g6NcyV5jmqwJ2s+zpgZGZ7tpLIcN' >"$ssh_dir/known_hosts"
export GIT_SSH_COMMAND="ssh -i $key -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$ssh_dir/known_hosts"

git clone "ssh://aur@aur.archlinux.org/$pkgname.git" aur
cp PKGBUILD .SRCINFO aur/
cd aur
git add PKGBUILD .SRCINFO
if git diff --cached --quiet; then
	echo "$pkgname $pkgver is already on the AUR"
	exit 0
fi
# Commits are signed with the AUR SSH key.
git -c user.name="$AUR_USERNAME" -c user.email="$AUR_EMAIL" \
	-c gpg.format=ssh -c user.signingkey="$key" \
	commit -S -m "Update to $pkgver"
git push origin HEAD:master
