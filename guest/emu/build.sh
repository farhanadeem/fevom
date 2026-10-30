#!/bin/sh
set -eu

usage() {
	cat <<'EOF' >&2
Build FEX-Emu for fev's guest: guest/out/emu/FEX, one static-pie executable.

  guest/emu/build.sh                  FEX -> guest/out/emu/FEX, if its inputs changed
  guest/emu/build.sh --clean          the same from an empty source tree and build volume
  guest/emu/build.sh --check          build nothing: say whether guest/out/emu/FEX is built
                                      from these inputs (exit 1 if not)
  guest/emu/build.sh --lock FILE --out DIR [--clean]
                                      another chain: FILE's fex, submodule and patch lines,
                                      the patches beside FILE, into DIR

  FEV_EMU_VOLUME=NAME   the build volume (default fev-emu-build)
  JOBS=N                  ninja's jobs (default 4)
EOF
}
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
lock=$here/sources.lock
patches=$here/patches
out=$root/guest/out/emu
clean= check=
while [ $# -gt 0 ]; do
	case "$1" in
	--clean) clean=1 ;;
	--check) check=1 ;;
	--lock)
		lock=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
		patches=$(dirname "$lock")
		shift
		;;
	--out)
		mkdir -p "$2"
		out=$(cd "$2" && pwd)
		shift
		;;
	*)
		usage
		exit 2
		;;
	esac
	shift
done
volume=${FEV_EMU_VOLUME:-fev-emu-build}
pristine=$root/guest/out/emu/pristine
src=$out/src

tag=$(awk '$1 == "fex" { print $2 }' "$lock")
commit=$(awk '$1 == "fex" { print $3 }' "$lock")
[ -n "$tag" ] && [ -n "$commit" ] || {
	echo "$0: $lock has no fex line" >&2
	exit 1
}
chain() { grep -E '^(fex|submodule|patch) ' "$lock"; }
for p in $(awk '$1 == "patch" { print $2 }' "$lock"); do
	[ -f "$patches/$p" ] || {
		echo "$0: $patches/$p is named in $lock but missing" >&2
		exit 1
	}
done
stamp=$({
	chain
	for p in $(awk '$1 == "patch" { print $2 }' "$lock"); do cat "$patches/$p"; done
	cat "$0" "$root/kernel/builder/Dockerfile"
} | shasum -a 256 | cut -c1-16)

fresh() {
	[ -z "$clean" ] && [ -x "$out/FEX" ] && [ -f "$out/FEX.stamp" ] && [ -f "$out/licenses/BUILD" ] &&
		[ "$(head -1 "$out/FEX.stamp")" = "stamp $stamp" ] &&
		[ "$(sed -n 's/^sha256 //p' "$out/FEX.stamp")" = "$(shasum -a 256 "$out/FEX" | cut -c1-64)" ]
}
if [ -n "$check" ]; then
	if fresh; then
		echo "FEX $(sed -n 's/^sha256 //p' "$out/FEX.stamp" | cut -c1-16) $tag, $(grep -c '^patch ' "$lock") patches, built from these inputs ($stamp)"
		exit 0
	fi
	if [ -f "$out/FEX.stamp" ]; then
		echo "FEX NOT built from these inputs (its stamp $(head -1 "$out/FEX.stamp" | cut -d' ' -f2), these $stamp): guest/emu/build.sh"
	else
		echo "FEX missing: guest/emu/build.sh"
	fi
	exit 1
fi
if fresh; then
	exit 0
fi

if [ ! -d "$pristine/.git" ]; then
	rm -rf "$pristine.tmp"
	mkdir -p "$(dirname "$pristine")"
	git clone -q -c advice.detachedHead=false --depth 1 --branch "$tag" https://github.com/FEX-Emu/FEX "$pristine.tmp"
	awk '$1 == "submodule" { print $2 }' "$lock" | while read -r p; do
		git -C "$pristine.tmp" submodule update -q --init --depth 1 "$p"
	done
	mv "$pristine.tmp" "$pristine"
fi
[ "$(git -C "$pristine" rev-parse HEAD)" = "$commit" ] || {
	echo "$0: $pristine is not at $commit ($tag): remove it to fetch again" >&2
	exit 1
}
awk '$1 == "submodule" { print $2, $3 }' "$lock" | while read -r p want; do
	have=$(git -C "$pristine/$p" rev-parse HEAD 2>/dev/null || echo none)
	[ "$have" = "$want" ] || {
		echo "$0: $pristine/$p is at $have, not $want: remove $pristine to fetch again" >&2
		exit 1
	}
done
[ -z "$(git -C "$pristine" status --porcelain --ignore-submodules=none)" ] || {
	echo "$0: $pristine has changes: remove it to fetch again" >&2
	exit 1
}

rm -rf "$src"
mkdir -p "$out"
cp -c -R "$pristine" "$src" 2>/dev/null || {
	rm -rf "$src"
	cp -R "$pristine" "$src"
}
for p in $(awk '$1 == "patch" { print $2 }' "$lock"); do
	patch -d "$src" -p1 --fuzz=0 -s --no-backup-if-mismatch <"$patches/$p" || {
		echo "$0: $p does not apply at --fuzz=0 after the patches before it" >&2
		exit 1
	}
done

. "$root/tests/lib/buildhost.sh"
build_host_docker
kimage="fev-kbuild:$("$root/tools/dockerfile-id.sh" "$root/kernel/builder/Dockerfile")"
if ! docker image inspect "$kimage" >/dev/null 2>&1; then
	docker build -q -t "$kimage" "$root/kernel/builder" >/dev/null
fi
recipe='apt-get -o Acquire::Check-Valid-Until=false update >/dev/null
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
	cmake ninja-build g++ libstdc++-14-dev pkg-config git file >/dev/null
rm -rf /var/lib/apt/lists/*'
image="fev-emu-build:$(printf '%s\n%s\n' "$kimage" "$recipe" | shasum -a 256 | cut -c1-12)"
if ! docker image inspect "$image" >/dev/null 2>&1; then
	cid=$(docker create "$kimage" /bin/sh -euc "$recipe")
	docker start -a "$cid"
	docker commit "$cid" "$image" >/dev/null
	docker container rm "$cid" >/dev/null
fi

rm -rf "$out/licenses.tmp"
mkdir -p "$out/licenses.tmp"
epoch=$(git -C "$pristine" log -1 --format=%ct)
docker run --rm -i -v "$volume":/build -v "$src":/fex:ro -v "$out":/out \
	-e JOBS="${JOBS:-4}" -e STAMP="$stamp" -e CLEAN="$clean" -e SOURCE_DATE_EPOCH="$epoch" "$image" /bin/sh -euc '
	exec 9>/build/.lock
	flock 9
	cd /build
	if [ -n "$CLEAN" ] || [ "$(cat .fev-stamp 2>/dev/null)" != "$STAMP" ]; then
		find /build -mindepth 1 -maxdepth 1 ! -name .lock -exec rm -rf {} +
	fi
	[ -f build.ninja ] || cmake -G Ninja /fex \
		-DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr \
		-DTUNE_CPU=none -DTUNE_ARCH=armv8.4-a \
		-DCMAKE_C_COMPILER=clang -DCMAKE_CXX_COMPILER=clang++ -DUSE_LINKER=lld \
		-DENABLE_LTO=True -DBUILD_TESTING=False -DBUILD_FEXCONFIG=False -DBUILD_THUNKS=False \
		-DENABLE_OFFLINE_TELEMETRY=False -DENABLE_JEMALLOC_GLIBC_ALLOC=False \
		-DENABLE_CCACHE=False -DCMAKE_CXX_SCAN_FOR_MODULES=OFF -DFEX_SERVERLESS=True \
		-DCMAKE_POSITION_INDEPENDENT_CODE=ON -DCMAKE_EXE_LINKER_FLAGS=-static-pie >/dev/null
	echo "$STAMP" >.fev-stamp
	nice -n 10 ninja -j"$JOBS" FEX >/tmp/ninja.log 2>&1 || { tail -60 /tmp/ninja.log >&2; exit 1; }
	file Bin/FEX | grep -q "static-pie linked" || { file Bin/FEX >&2; exit 1; }
	cp Bin/FEX /out/FEX.tmp

	L=/out/licenses.tmp
	lic() { mkdir -p "$L/$1"; cp "/fex/$2" "$L/$1/$(basename "$2")"; }
	lic fex LICENSE
	lic vixl External/vixl/LICENCE
	lic cpp-optparse Source/Common/cpp-optparse/LICENSE
	lic fmt External/fmt/LICENSE
	lic xxhash External/xxhash/LICENSE
	lic range-v3 External/range-v3/LICENSE.txt
	lic unordered_dense External/unordered_dense/LICENSE
	lic rpmalloc External/rpmalloc/LICENSE
	lic jemalloc_glibc External/jemalloc_glibc/COPYING
	lic Vulkan-Headers External/Vulkan-Headers/LICENSE.md
	lic tiny-json External/tiny-json/LICENSE
	lic cephes External/cephes/LICENSE
	mkdir -p "$L/SoftFloat-3e" "$L/drm-headers"
	awk "/^=+\\*\\/\$/ { print; exit } { print }" /fex/External/SoftFloat-3e/src/softfloat_raiseFlags.c >"$L/SoftFloat-3e/LICENSE"
	for h in /fex/External/drm-headers/include/fex-drm/*.h; do
		echo "== ${h#/fex/External/drm-headers/}"
		awk "{ print } /\\*\\// { exit }" "$h"
	done >"$L/drm-headers/NOTICES"
	mkdir -p "$L/glibc" "$L/libstdc++"
	cp /usr/share/doc/libc6/copyright "$L/glibc/copyright"
	cp /usr/share/common-licenses/LGPL-2.1 "$L/glibc/LGPL-2.1"
	cp /usr/share/common-licenses/GPL-2 "$L/glibc/GPL-2"
	cp /usr/share/doc/gcc-14-base/copyright "$L/libstdc++/copyright"
	cp /usr/share/common-licenses/GPL-3 "$L/libstdc++/GPL-3"
	grep -q "GCC RUNTIME LIBRARY EXCEPTION" "$L/libstdc++/copyright" || {
		echo "gcc-14-base'"'"'s copyright no longer holds the runtime exception" >&2
		exit 1
	}
	{
		echo "FEX static-pie, linked by lld against glibc and libstdc++ from Debian:"
		grep -h "^URIs:" /etc/apt/sources.list.d/debian.sources | sort -u
		dpkg-query -W -f "\${Package} \${Version} (source \${source:Package} \${source:Version})\n" \
			libc6-dev libstdc++-14-dev libgcc-14-dev clang lld cmake ninja-build
		echo "flags: see guest/emu/build.sh (Release, LTO, armv8.4-a, -static-pie)"
	} >"$L/BUILD"
'
chmod -R a+rX "$out/licenses.tmp"
rm -rf "$out/licenses"
mv "$out/licenses.tmp" "$out/licenses"
mv "$out/FEX.tmp" "$out/FEX"
sum=$(shasum -a 256 "$out/FEX" | cut -c1-64)
echo "$sum  FEX" >"$out/FEX.sha256"
{
	echo "stamp $stamp"
	echo "sha256 $sum"
	chain
} >"$out/FEX.stamp"
echo "built $out/FEX ($(wc -c <"$out/FEX" | tr -d ' ') bytes, sha256 $(echo "$sum" | cut -c1-16), $(grep -c '^patch ' "$lock") patches)"
