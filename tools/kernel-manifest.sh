#!/bin/sh
set -eu

check=
if [ "${1:-}" = --check ]; then
	check=1
	shift
fi
[ $# -eq 1 ] || {
	echo "usage: $0 [--check] IMAGE" >&2
	exit 2
}
image=$1
[ -f "$image" ] || {
	echo "kernel-manifest: no $image" >&2
	exit 1
}

version=$(LC_ALL=C strings -n 16 "$image" | grep -m1 '^Linux version ' || true)
[ -n "$version" ] || {
	echo "kernel-manifest: $image has no \"Linux version\" banner; is it a kernel?" >&2
	exit 1
}
release=$(printf '%s\n' "$version" | cut -d' ' -f3)
sum=$(shasum -a 256 "$image" | cut -c1-64)
want=$(printf '# fev kernel manifest (tools/kernel-manifest.sh): the Image beside it\nrelease %s\nversion %s\nsha256 %s\n' "$release" "$version" "$sum")

if [ -n "$check" ]; then
	if [ "$(cat "$image.manifest" 2>/dev/null)" = "$want" ]; then
		echo "kernel manifest: release $release, sha256 ${sum%"${sum#????????????}"}"
		exit 0
	fi
	if [ ! -f "$image.manifest" ]; then
		echo "kernel-manifest: no $image.manifest" >&2
	else
		echo "kernel-manifest: $image.manifest is not $image's (release $release, sha256 $sum): $(grep -v '^#' "$image.manifest" | tr '\n' ' ')" >&2
	fi
	exit 1
fi
tmp=$(mktemp "$image.manifest.XXXXXX")
printf '%s\n' "$want" >"$tmp"
mv "$tmp" "$image.manifest"
