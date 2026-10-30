#!/bin/sh
# The SHA-256 of every file of a built Fevom.app, its code signatures taken
# out, so that a build of a release's tag can be compared with the release
# whoever signed each.
#
#   tools/bundle-hashes.sh APP|DMG                print "HASH  PATH" for each file, and "link  PATH -> TARGET"
#   tools/bundle-hashes.sh --check LIST APP|DMG   compare with LIST (a release's Fevom-vX.Y.Z.sha256); fail on a difference
#
# DMG is a release's disk image, whose Fevom.app is read.
set -eu

usage() {
	sed -n '6,7p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
}
list=
if [ "${1:-}" = --check ]; then
	[ $# -eq 3 ] || usage
	list=$2
	[ -f "$list" ] || usage
	shift 2
fi
[ $# -eq 1 ] || usage

tmp=$(mktemp -d)
mnt=
cleanup() {
	[ -z "$mnt" ] || hdiutil detach -quiet "$mnt" 2>/dev/null || :
	rm -rf "$tmp"
}
trap cleanup EXIT
app=$1
case "$app" in
*.dmg)
	mnt=$tmp/mnt
	mkdir -p "$mnt"
	hdiutil attach -quiet -readonly -nobrowse -mountpoint "$mnt" "$app"
	app=$mnt/Fevom.app
	;;
esac
[ -d "$app/Contents" ] || usage

ditto "$app" "$tmp/app"
find "$tmp/app" -type f | while IFS= read -r f; do
	case "$(head -c 4 "$f" | od -An -tx1 | tr -d ' \n')" in
	cffaedfe | cefaedfe | cafebabe) codesign --remove-signature "$f" ;;
	esac
done
(
	cd "$tmp/app"
	find . \( -type f -o -type l \) ! -path '*/_CodeSignature/*' ! -name CodeResources | sed 's|^\./||' | LC_ALL=C sort |
		while IFS= read -r f; do
			if [ -L "$f" ]; then
				printf 'link  %s -> %s\n' "$f" "$(readlink "$f")"
			else
				printf '%s  %s\n' "$(shasum -a 256 "$f" | cut -d' ' -f1)" "$f"
			fi
		done
) >"$tmp/hashes"

if [ -z "$list" ]; then
	cat "$tmp/hashes"
	exit 0
fi
if diff "$list" "$tmp/hashes" >"$tmp/diff"; then
	echo "the same: $(wc -l <"$tmp/hashes" | tr -d ' ') files and links"
else
	echo "different (< $list, > this build):" >&2
	grep '^[<>]' "$tmp/diff" >&2
	exit 1
fi
