#!/bin/sh
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
out="$root/guest/out/emu/rosetta-probe"
check=
while [ $# -gt 0 ]; do
 case "$1" in
 --check) check=1 ;;
 --out) out=$2; shift ;;
 *) echo "usage: $0 [--check] [--out FILE]" >&2; exit 2 ;;
 esac
 shift
done
builder="$root/kernel/builder"
fresh=1
[ -x "$out" ] || fresh=
for input in "$here/rosetta-probe.S" "$here/build-probe.sh" "$builder/Dockerfile"; do
 [ -f "$out" ] && [ "$out" -nt "$input" ] || fresh=
done
[ -z "$fresh" ] || exit 0
[ -z "$check" ] || exit 1
. "$root/tests/lib/buildhost.sh"
build_host_docker
trap 'rm -f "$out.tmp.$$"; build_host_release' EXIT
image="fev-kbuild:$("$root/tools/dockerfile-id.sh" "$builder/Dockerfile")"
if ! docker image inspect "$image" >/dev/null 2>&1; then docker build -q -t "$image" "$builder" >/dev/null; fi
mkdir -p "$(dirname "$out")"
dest=$(cd "$(dirname "$out")" && pwd)
file=$(basename "$out")
docker run --rm -v "$here":/src:ro -v "$dest":/dest "$image" \
 clang --target=x86_64-linux-gnu -fuse-ld=lld -nostdlib -static -Wl,--build-id=none \
 -o "/dest/$file.tmp.$$" /src/rosetta-probe.S
chmod 755 "$out.tmp.$$"
mv "$out.tmp.$$" "$out"
