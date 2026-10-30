#!/bin/sh
set -eu

version=@VERSION@
sha256=@SHA256@
url=https://github.com/@REPO@/releases/download/v$version/Fevom-v$version.dmg
dest=/Applications/Fevom.app

say() { printf '%s\n' "$*"; }
die() {
	printf 'Fevom: %s\n' "$*" >&2
	exit 1
}

[ "$(uname -s)" = Darwin ] || die "Fevom runs on macOS"
[ "$(uname -m)" = arm64 ] || die "Fevom needs a Mac with Apple silicon"
major=$(sw_vers -productVersion | cut -d. -f1)
[ "$major" -ge 26 ] || die "Fevom needs macOS 26 or later; this Mac has $(sw_vers -productVersion)"
if pgrep -xq Fevom; then
	die "Fevom is running: update it from its menu (Check for Updates), or quit it and run this again"
fi
[ -w /Applications ] || die "cannot write to /Applications: install Fevom as an administrator"

tmp=$(mktemp -d)
mnt=$tmp/mnt
cleanup() {
	[ ! -d "$mnt" ] || hdiutil detach -quiet "$mnt" 2>/dev/null || :
	rm -rf "$tmp"
}
trap cleanup EXIT

say "Downloading Fevom $version..."
curl -fL --progress-bar -o "$tmp/Fevom.dmg" "$url" || die "could not download $url"
[ "$(shasum -a 256 "$tmp/Fevom.dmg" | cut -d' ' -f1)" = "$sha256" ] ||
	die "the download is not the release's disk image (its SHA-256 does not match)"

mkdir -p "$mnt"
hdiutil attach -quiet -readonly -nobrowse -mountpoint "$mnt" "$tmp/Fevom.dmg"
[ -d "$mnt/Fevom.app" ] || die "the disk image has no Fevom.app"
if [ -e "$dest" ]; then
	say "Replacing $dest..."
	rm -rf "$dest"
fi
ditto "$mnt/Fevom.app" "$dest"
codesign --verify --deep --strict "$dest" 2>/dev/null || die "$dest does not verify"

say "Installed Fevom $version in /Applications. Opening it..."
open "$dest"
