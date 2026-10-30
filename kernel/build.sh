#!/bin/sh
# Build the guest kernel in a pinned container on the Linux build host
# (tests/lib/buildhost.sh: a scratch fev engine, never the Mac's default
# Docker context; FEV_BUILD_DOCKER_HOST names another Linux Docker host,
# which the first kernel, before any fev runs, needs).
#
#   kernel/build.sh             build kernel/out/Image (incremental)
#   kernel/build.sh debug       the same sources with lock proving -> kernel/out/debug/Image
#   kernel/build.sh extra DIR [debug]
#                               the same plus DIR's src/, patches/*.patch and config/*.config
#                               (DIR a directory under kernel/), in a tree of its own
#                               -> kernel/out/NAME/Image (Image.debug), NAME DIR's last part
#   kernel/build.sh clean [debug | extra DIR]
#                               delete an object directory (the release build's by default)
#   kernel/build.sh repro       two builds from clean; fail if the Image hashes differ
#   kernel/build.sh menuconfig  edit the config interactively (result is not saved to fragments)
#   kernel/build.sh shell       shell in the builder with the source volume mounted
#
# What is built: upstream linux-$KERNEL_VERSION with the files under src/
# added and patches/*.patch applied (builder/build-inner.sh says how), and
# config/*.config merged onto allnoconfig. The debug build adds
# debug.config; the release lint forbids every option in it.
#
# Source and objects live in the Docker volume `fev-kbuild` so the build
# runs on a Linux filesystem, not through the Mac file share; the build
# host's home is kept between its runs, and the volume with it. Builds in that
# volume take turns (a lock), so this is safe to call from two places.
#
# The build number is pinned. Left alone it counts builds in the object
# directory, so an incremental build's Image differs from a clean one's; and
# a build that is given no number carries an empty one in the first of the
# two version strings an Image holds, so it differs from a pinned build even
# when both report "#1".
set -eu

here=$(cd "$(dirname "$0")" && pwd)
. "$here/VERSION"

case "${1:-build}" in
build | debug | extra | clean | repro | menuconfig | shell) ;;
*)
	sed -n '7,17p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
	;;
esac

. "$here/../tests/lib/buildhost.sh"
build_host_docker
image="fev-kbuild:$("$here/../tools/dockerfile-id.sh" "$here/builder/Dockerfile")"
if ! docker image inspect "$image" >/dev/null 2>&1; then
	docker build -q -t "$image" "$here/builder" >/dev/null
fi

mkdir -p "$here/out"

tty=
[ -t 0 ] && tty=-t

run() {
	docker run --rm -i $tty \
		-v "${FEV_KBUILD_VOLUME:-fev-kbuild}":/work \
		-v "$here":/kernel:ro \
		-v "$here/out":/out \
		-e KERNEL_VERSION="$KERNEL_VERSION" \
		-e KERNEL_SHA256="$KERNEL_SHA256" \
		-e KBUILD_BUILD_TIMESTAMP="$KBUILD_BUILD_TIMESTAMP" \
		-e KBUILD_BUILD_USER=fev \
		-e KBUILD_BUILD_HOST=fev \
		-e KBUILD_BUILD_VERSION="${KBUILD_BUILD_VERSION-1}" \
		-e KERNEL_EXTRA="${extra:-}" \
		-e KERNEL_EXTRA_NAME="${extra_name:-}" \
		"$image" "$@"
}

extra_dir() {
	[ -n "${1:-}" ] && [ -d "$here/$1" ] || {
		echo "kernel/build.sh extra: ${1:-(none)} is not a directory under $here" >&2
		exit 2
	}
	top=$(cd "$here" && pwd -P)
	abs=$(cd "$here/$1" && pwd -P)
	case "$abs" in
	"$top"/*) rel=${abs#"$top"/} ;;
	*)
		echo "kernel/build.sh extra: $1 is not inside $here" >&2
		exit 2
		;;
	esac
	case "$rel" in
	src | src/* | patches | patches/* | config | config/* | builder | builder/* | out | out/*)
		echo "kernel/build.sh extra: $rel is the release build's own; name a directory of its own" >&2
		exit 2
		;;
	esac
	extra=/kernel/$rel
	extra_name=$(basename "$rel")
	case "$extra_name" in
	debug | tree | config | obj* | linux-* | Image* | .* | *[!a-z0-9-]*)
		echo "kernel/build.sh extra: $extra_name cannot name a build (kernel/out/$extra_name)" >&2
		exit 2
		;;
	esac
}

case "${1:-build}" in
build)
	run /kernel/builder/build-inner.sh build release
	"$here/../tools/kernel-manifest.sh" "$here/out/Image"
	;;
debug)
	run /kernel/builder/build-inner.sh build debug
	;;
extra)
	extra_dir "${2:-}"
	case "${3:-}" in
	'') run /kernel/builder/build-inner.sh build extra ;;
	debug) run /kernel/builder/build-inner.sh build extra-debug ;;
	*)
		echo "usage: $0 extra DIR [debug]" >&2
		exit 2
		;;
	esac
	;;
clean)
	case "${2:-release}" in
	extra)
		extra_dir "${3:-}"
		run /kernel/builder/build-inner.sh clean extra
		run /kernel/builder/build-inner.sh clean extra-debug
		;;
	*) run /kernel/builder/build-inner.sh clean "${2:-release}" ;;
	esac
	;;
repro)
	run /kernel/builder/build-inner.sh clean release
	run /kernel/builder/build-inner.sh build release
	first=$(shasum -a 256 "$here/out/Image" | cut -d' ' -f1)
	run /kernel/builder/build-inner.sh clean release
	run /kernel/builder/build-inner.sh build release
	second=$(shasum -a 256 "$here/out/Image" | cut -d' ' -f1)
	echo "build 1: $first"
	echo "build 2: $second"
	[ "$first" = "$second" ] || {
		echo "kernel build is not reproducible" >&2
		exit 1
	}
	echo "reproducible"
	;;
menuconfig)
	run /kernel/builder/build-inner.sh menuconfig release
	;;
shell)
	run sh
	;;
esac
