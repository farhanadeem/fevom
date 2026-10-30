#!/bin/sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
lock="$here/docker-cli.lock"
locked() { awk -v field="$1" '$1 == "source" && $2 == "docker-cli-darwin" { print $field }' "$lock"; }
sum=$(locked 5)
dir="$here/out/docker-cli/$(locked 3)-$(echo "$sum" | cut -c1-12)"
archive="$dir/docker.tgz"
mkdir -p "$dir"
if [ ! -f "$archive" ]; then
	curl -fsSL --retry 3 -o "$archive.part" "$(locked 6)"
	mv "$archive.part" "$archive"
fi
[ "$(shasum -a 256 "$archive" | cut -d' ' -f1)" = "$sum" ] || {
	echo "$0: checksum mismatch for $archive: it is not what sources.lock pins" >&2
	exit 1
}
if [ ! -x "$dir/docker" ] || [ "$archive" -nt "$dir/docker" ]; then
	tar -xzf "$archive" -C "$dir" --strip-components=1 docker/docker
	touch "$dir/docker"
fi
echo "$dir/docker"
