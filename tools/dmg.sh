#!/bin/sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
app=$root/app/out/Fevom.app
out=$root/out
while [ $# -gt 0 ]; do
	case "$1" in
	--out)
		out=$2
		shift
		;;
	-*)
		echo "usage: $0 [APP] [--out DIR]" >&2
		exit 2
		;;
	*) app=$1 ;;
	esac
	shift
done
die() {
	echo "dmg: $*" >&2
	exit 1
}
[ -d "$app/Contents" ] || die "no bundle at $app: build it (tools/build.sh)"
app=$(cd "$app" && pwd)
"$root/tools/verify-bundle.sh" --for-others "$app" >/dev/null || die "$app fails tools/verify-bundle.sh --for-others: a build given away is ad-hoc throughout (FEV_SIGN_IDENTITY unset)"
build=$(plutil -extract FevomBuild raw -o - "$app/Contents/Info.plist")

tmp=$(mktemp -d "${TMPDIR:-/tmp}/dmg.XXXXXX")
mnt=$tmp/mnt
cleanup() {
	[ ! -d "$mnt" ] || hdiutil detach -quiet "$mnt" 2>/dev/null || true
	rm -rf "$tmp"
}
trap cleanup EXIT
stage=$tmp/Fevom
mkdir -p "$stage"
ditto "$app" "$stage/Fevom.app"
ln -s /Applications "$stage/Applications"
sed "s/@BUILD@/$build/g" "$root/tools/dmg/FIRST-OPEN.txt" >"$stage/FIRST-OPEN.txt"

mkdir -p "$out"
dmg=$out/Fevom-$build.dmg
rm -f "$dmg" "$tmp/Fevom.dmg"
hdiutil create -quiet -volname "Fevom" -srcfolder "$stage" -fs APFS -format UDZO "$tmp/Fevom.dmg"
mv "$tmp/Fevom.dmg" "$dmg"

mkdir -p "$mnt"
hdiutil attach -quiet -readonly -nobrowse -mountpoint "$mnt" "$dmg"
for f in FIRST-OPEN.txt Fevom.app/Contents/Info.plist; do
	[ -e "$mnt/$f" ] || die "the image has no $f"
done
[ "$(readlink "$mnt/Applications")" = /Applications ] || die "the image's Applications link does not lead to /Applications"
"$root/tools/verify-bundle.sh" --for-others "$mnt/Fevom.app" >"$tmp/verify" || {
	cat "$tmp/verify" >&2
	die "the app in $dmg fails verify-bundle"
}
hdiutil detach -quiet "$mnt"
echo "$dmg ($(du -h "$dmg" | cut -f1 | tr -d ' ')): Fevom.app (verified from the image), Applications, FIRST-OPEN.txt"
