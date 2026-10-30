#!/bin/sh
set -eu
here=$(cd "$(dirname "$0")" && pwd)
app=$(cd "$here/../.." && pwd)
. "$app/dev-env.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
swiftc -O -o "$work/make-icon" "$here/make-icon.swift"
"$work/make-icon" "$work/icon.png"
set_=$work/AppIcon.iconset
mkdir -p "$set_"
for s in 16 32 128 256 512; do
	sips -z $s $s "$work/icon.png" --out "$set_/icon_${s}x${s}.png" >/dev/null
	d=$((s * 2))
	sips -z $d $d "$work/icon.png" --out "$set_/icon_${s}x${s}@2x.png" >/dev/null
done
mkdir -p "$app/Resources"
iconutil -c icns -o "$app/Resources/AppIcon.icns" "$set_"
echo "$app/Resources/AppIcon.icns"
