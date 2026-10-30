#!/bin/bash
set -eu
die() { echo "guest/debug/remote/build.sh: $*" >&2; exit 1; }
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
out=$root/guest/out/debug-agent
arches=${*:-arm64 amd64}
stage_src=$here/stage
sysimage=$root/guest/sysimage

build=${FEV_BUILD_ID:-$("$root/tools/build-id.sh" 2>/dev/null || echo dev)}

. "$root/tests/lib/buildhost.sh"
build_host_docker
image="fev-sysimage:$("$root/tools/dockerfile-id.sh" "$sysimage/Dockerfile")"
if ! docker image inspect "$image" >/dev/null 2>&1; then
	docker build -q -t "$image" -f "$sysimage/Dockerfile" "$sysimage" >/dev/null || die "the builder image $image"
fi

for arch in $arches; do
	d=$out/$arch
	rm -rf "$d"
	mkdir -p "$d"
	echo "== debug-agent/$arch"
	case $arch in
	arm64) lock=$root/guest/debug/defaultset.lock cc=gcc strip=strip ;;
	amd64) lock=$root/guest/debug/defaultset-x86_64.lock cc=x86_64-linux-gnu-gcc strip=x86_64-linux-gnu-strip ;;
	*) die "no architecture $arch" ;;
	esac
	[ -f "$lock" ] || die "no $lock (tools/debug-set.sh -arch ...)"

	(cd "$root/guest" && GOOS=linux GOARCH=$arch CGO_ENABLED=0 "${GO:-go}" build \
		-trimpath -ldflags "-s -w -X fev/guest/debug/remote.Build=$build" \
		-o "$d/fev-debug-agent" ./cmd/fev-debug-agent)

	(cd "$root/guest" && GOOS=linux GOARCH=$arch CGO_ENABLED=0 "${GO:-go}" build \
		-trimpath -ldflags "-s -w" -o "$d/fev-debug" ./cmd/fev-debug)

	docker run --rm -i -v "$stage_src":/src:ro -v "$d":/dest "$image" sh -eu -c "
		$cc -static -nostdlib -ffreestanding -fno-builtin -fno-stack-protector -fno-pie -no-pie \
			-fno-asynchronous-unwind-tables -Os -Wall -Wextra -Werror -Wl,--build-id=none \
			-o /tmp/stage /src/stage.c
		$strip -o /dest/fev-debug-stage /tmp/stage
	" || die "stage: compiling $stage_src/stage.c for $arch"

	cp "$lock" "$d/defaultset.lock"
	(cd "$root/guest" && "${GO:-go}" run ./cmd/fev-debug stamp -lock "$d/defaultset.lock") >"$d/stamp"

	echo "   -> $d: $(ls "$d" | tr '\n' ' ')"
done
