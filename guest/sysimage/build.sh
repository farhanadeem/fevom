#!/bin/sh
set -eu

usage() {
	cat <<'EOF' >&2
Build the guest's read-only system image, guest/out/system.img.

  guest/sysimage/build.sh [--force] [--out FILE [--add SRC:DEST]...]
  guest/sysimage/build.sh --tools     print the directory holding the
                                      programs as built

--add puts one more file into the image (DEST is its path inside); it needs
--out, so that guest/out/system.img never holds a test's file.
EOF
}

here=$(cd "$(dirname "$0")" && pwd)
out_dir=$(cd "$here/.." && pwd)/out
out="$out_dir/system.img"
kernel_image=$(cd "$here/../.." && pwd)/kernel/out/Image
cache="$out_dir/sysimage"
lock="$here/sources.lock"
engine=$(cd "$here/../docker" && pwd)
helper_src=$(cd "$here/../cmd/fev-guest" && pwd)
runc_src=$(cd "$here/../cmd/fev-runc" && pwd)
webproxy_src=$(cd "$here/../cmd/fev-webproxy" && pwd)
registry_src=$(cd "$here/../cmd/fev-registry" && pwd)
isostage_src=$(cd "$here/../machines/isostage" && pwd)
dbgstage_src=$(cd "$here/../debug/stage" && pwd)
dbg_src=$(cd "$here/../cmd/fev-debug" && pwd)
kernelid_src=$(cd "$here/../tools/kernelid" && pwd)
emu_build=$(cd "$here/../emu" && pwd)/build.sh
emu_out=$out_dir/emu
emu_probe=$(dirname "$emu_build")/rosetta-probe.S
emu_probe_build=$(dirname "$emu_build")/build-probe.sh
force=
adds=
custom=
print_tools=
while [ $# -gt 0 ]; do
	case "$1" in
	--force) force=1 ;;
	--tools) print_tools=1 ;;
	--out)
		out=$2
		custom=1
		shift
		;;
	--add)
		[ -f "${2%%:*}" ] || {
			echo "$0: no file ${2%%:*}" >&2
			exit 2
		}
		adds="$adds $2"
		shift
		;;
	*)
		usage
		exit 2
		;;
	esac
	shift
done
[ -z "$adds" ] || [ -n "$custom" ] || {
	echo "$0: --add needs --out: the product's image takes no extra files" >&2
	exit 2
}

locked() {
	awk -v name="$1" -v field="$2" '$1 == "source" && $2 == name { print $field }' "$lock"
}
built() {
	echo "$cache/$1-$(locked "$1" 3)-$(locked "$1" 5 | cut -c1-12)"
}
if [ -n "$print_tools" ]; then
	built btrfs-progs
	exit 0
fi

kernel_sum() {
	if [ -f "$kernel_image" ]; then
		shasum -a 256 <"$kernel_image" | cut -d' ' -f1
	else
		echo none
	fi
}

fresh() {
	[ -z "$force" ] && [ -z "$adds" ] && [ -f "$out" ] && [ -x "$(built btrfs-progs)/btrfs" ] &&
		[ -x "$(built busybox)/busybox" ] && [ -f "$(built btrfs-progs)/linked/VERSIONS" ] &&
		[ -f "$(built busybox)/linked/VERSIONS" ] && [ -f "$out.kernel" ] || return 1
	_ksum=$(kernel_sum)
	[ "$(sed -n 's/^image //p' "$out.kernel")" = "$_ksum" ] || return 1
	[ "$_ksum" = none ] || grep -qx 'features recorded' "$out.kernel" || return 1
	"$emu_build" --check >/dev/null || return 1
	"$emu_probe_build" --out "$emu_out/rosetta-probe" --check || return 1
	for input in "$here/build.sh" "$here/Dockerfile" "$lock" "$engine/build.sh" "$engine/sources.lock" "$here/../k8s/k3s.lock" "$isostage_src/fev-isostage.c" "$dbgstage_src/fev-dbgstage.c" "$runc_src/stage/fev-runc.c" "$emu_probe" "$emu_probe_build" "$emu_out/rosetta-probe" "$emu_out/FEX" "$emu_out/FEX.stamp"; do
		[ "$out" -nt "$input" ] || return 1
	done
	repo=$(cd "$here/../.." && pwd)
	srcs=$(cd "$helper_src" && GOOS=linux GOARCH=arm64 "${GO:-go}" list -deps \
		-f '{{if not .Standard}}{{.Dir}}{{end}}' . "$runc_src" "$webproxy_src" "$registry_src" "$dbg_src" "$kernelid_src" 2>/dev/null | grep "^$repo/" | sort -u) &&
		[ -n "$srcs" ] || return 1
	[ -z "$(find $srcs "$here/../go.mod" "$here/../go.sum" -newer "$out" -print 2>/dev/null | head -1)" ]
}
if fresh; then
	exit 0
fi
if [ -n "${FEV_BUILD_HOST_STARTING:-}" ] && [ -z "$custom" ]; then
	[ -f "$out" ] || {
		echo "$0: no $out to start the build host with; build the first one on another Linux Docker host (FEV_BUILD_DOCKER_HOST=unix:///PATH $0)" >&2
		exit 1
	}
	echo "$0: $out is not fresh (older than an input, or built for another kernel); the build host starts with it as it is" >&2
	exit 0
fi

mkdir -p "$cache" "$(dirname "$out")"
. "$here/../../tests/lib/buildhost.sh"
build_host_docker
"$emu_build"
image="fev-sysimage:$("$here/../../tools/dockerfile-id.sh" "$here/Dockerfile")"
if ! docker image inspect "$image" >/dev/null 2>&1; then
	docker build -q -t "$image" -f "$here/Dockerfile" "$here" >/dev/null
fi

build_btrfs_progs() {
	version=$(locked btrfs-progs 3)
	sum=$(locked btrfs-progs 5)
	url=$(locked btrfs-progs 6)
	dest=$(built btrfs-progs)
	[ -x "$dest/mkfs.btrfs" ] && [ -x "$dest/btrfs" ] && [ -z "$force" ] && return 0
	rm -rf "$dest"
	mkdir -p "$dest" "$cache/dl"
	docker run --rm -i -v "$cache/dl":/dl -v "$dest":/dest \
		-e URL="$url" -e SUM="$sum" "$image" sh -eu -c '
		tarball=/dl/$(basename "$URL")
		[ -f "$tarball" ] || curl -fsSL -o "$tarball" "$URL"
		echo "$SUM  $tarball" | sha256sum -c - >/dev/null || {
			echo "checksum mismatch for $tarball; it is not what sources.lock pins" >&2
			rm -f "$tarball"
			exit 1
		}
		mkdir /src && tar -xJf "$tarball" -C /src --strip-components=1 && cd /src
		./configure --quiet --disable-documentation --disable-python --disable-convert \
			--disable-zoned --disable-libudev --disable-backtrace --with-crypto=builtin >/dest/build.log 2>&1
		make -s -j"$(nproc)" mkfs.btrfs.static btrfs.static >>/dest/build.log 2>&1
		strip -o /dest/mkfs.btrfs mkfs.btrfs.static
		strip -o /dest/btrfs btrfs.static
		cp COPYING /dest/COPYING
		(cd /dest && sha256sum mkfs.btrfs btrfs >SHA256)
	'
}

build_busybox() {
	version=$(locked busybox 3)
	sum=$(locked busybox 5)
	url=$(locked busybox 6)
	dest=$(built busybox)
	[ -x "$dest/busybox" ] && [ -s "$dest/applets" ] && [ -f "$dest/epoch" ] && [ -z "$force" ] && return 0
	rm -rf "$dest"
	mkdir -p "$dest" "$cache/dl"
	docker run --rm -i -v "$cache/dl":/dl -v "$dest":/dest \
		-e URL="$url" -e SUM="$sum" "$image" sh -eu -c '
		tarball=/dl/$(basename "$URL")
		[ -f "$tarball" ] || curl -fsSL -o "$tarball" "$URL"
		echo "$SUM  $tarball" | sha256sum -c - >/dev/null || {
			echo "checksum mismatch for $tarball; it is not what sources.lock pins" >&2
			rm -f "$tarball"
			exit 1
		}
		mkdir /src && tar -xjf "$tarball" -C /src --strip-components=1 && cd /src
		SOURCE_DATE_EPOCH=$(stat -c %Y Makefile)
		export SOURCE_DATE_EPOCH
		echo "$SOURCE_DATE_EPOCH" >/dest/epoch
		make -s defconfig >/dest/build.log 2>&1
		sed -i -e "s/^# CONFIG_STATIC is not set/CONFIG_STATIC=y/" \
			-e "s/^CONFIG_TC=y/# CONFIG_TC is not set/" \
			-e "s/^CONFIG_SHA1_HWACCEL=y/# CONFIG_SHA1_HWACCEL is not set/" \
			-e "s/^CONFIG_SHA256_HWACCEL=y/# CONFIG_SHA256_HWACCEL is not set/" .config
		yes "" | make -s oldconfig >>/dest/build.log 2>&1
		make -s -j"$(nproc)" busybox >>/dest/build.log 2>&1
		strip -o /dest/busybox busybox
		./busybox --list >/dest/applets
		cp LICENSE /dest/LICENSE
		cp .config /dest/config
		(cd /dest && sha256sum busybox >SHA256)
	'
}

linked_notices() {
	[ -f "$2/linked/VERSIONS" ] && [ -z "$force" ] && return 0
	rm -rf "$2/linked"
	mkdir -p "$2/linked"
	awk -v n="$1" '$1 == "linked" && $2 == n { print $3, $5, $6, $7 }' "$lock" >"$2/linked/want"
	[ -s "$2/linked/want" ] || {
		echo "$0: sources.lock names no library linked into $1" >&2
		exit 1
	}
	docker run --rm -i -v "$2/linked":/out "$image" sh -eu -c '
		while read -r lib pkg src ver; do
			got=$(dpkg-query -W -f "\${source:Package} \${source:Version}" "$pkg")
			[ "$got" = "$src $ver" ] || {
				echo "$lib: the builder image has $pkg from $got; sources.lock pins $src $ver" >&2
				exit 1
			}
			mkdir -p "/out/$lib"
			cp -L "/usr/share/doc/$pkg/copyright" "/out/$lib/copyright"
			echo "$lib $pkg $src $ver" >>/out/VERSIONS.part
		done </out/want
		mkdir -p /out/common-licenses
		cp /usr/share/common-licenses/GPL-2 /usr/share/common-licenses/LGPL-2.1 /out/common-licenses/
		mv /out/VERSIONS.part /out/VERSIONS
		rm /out/want
	'
}

stage_all() {
	build_btrfs_progs
	linked_notices btrfs-progs "$dest"
	mkdir -p "$1/bin" "$1/licenses/btrfs-progs"
	cp "$dest/mkfs.btrfs" "$dest/btrfs" "$1/bin/"
	cp "$dest/COPYING" "$1/licenses/btrfs-progs/COPYING"
	cp -R "$dest/linked" "$1/licenses/btrfs-progs/linked"
	chmod 755 "$1/bin/"*
	build_busybox
	linked_notices busybox "$dest"
	mkdir -p "$1/engine/bin" "$1/licenses/busybox"
	cp -R "$dest/linked" "$1/licenses/busybox/linked"
	cp "$dest/busybox" "$1/engine/bin/busybox"
	chmod 755 "$1/engine/bin/busybox"
	while read -r applet; do
		[ "$applet" = busybox ] || ln -s busybox "$1/engine/bin/$applet"
	done <"$dest/applets"
	cp "$dest/LICENSE" "$1/licenses/busybox/LICENSE"
	cp "$dest/config" "$1/licenses/busybox/config"
	mkdir -p "$1/opt/fev/bin"
	(cd "$here/.." && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "${GO:-go}" build -trimpath -ldflags "-s -w" \
		-o "$1/opt/fev/fev-guest" ./cmd/fev-guest)
	chmod 755 "$1/opt/fev/fev-guest" "$1/opt/fev" "$1/opt/fev/bin"
	ln -s fev-guest "$1/opt/fev/fev-macexec"
	mkdir -p "$1/engine/usr/bin" "$1/engine/usr/libexec/fev"
	(cd "$here/.." && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "${GO:-go}" build -trimpath -ldflags "-s -w" \
		-o "$1/engine/usr/libexec/fev/fev-runc-create" ./cmd/fev-runc)
	chmod 755 "$1/engine/usr/libexec/fev" "$1/engine/usr/libexec/fev/fev-runc-create"
	(cd "$here/.." && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "${GO:-go}" build -trimpath -ldflags "-s -w" \
		-o "$1/bin/fev-webproxy" ./cmd/fev-webproxy)
	chmod 755 "$1/bin/fev-webproxy"
	(cd "$here/.." && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "${GO:-go}" build -trimpath -ldflags "-s -w" \
		-o "$1/bin/fev-registry" ./cmd/fev-registry)
	chmod 755 "$1/bin/fev-registry"
	(cd "$here/.." && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "${GO:-go}" build -trimpath -ldflags "-s -w" \
		-o "$1/bin/fev-debug" ./cmd/fev-debug)
	chmod 755 "$1/bin/fev-debug"
	mkdir -p "$1/etc/fev-debug"
	(cd "$here/.." && CGO_ENABLED=0 "${GO:-go}" run ./debug/tools/writeetc "$1/etc/fev-debug")
	cp "$here/../debug/defaultset.lock" "$1/etc/fev-debug/defaultset.lock"
	(cd "$here/.." && CGO_ENABLED=0 "${GO:-go}" run ./cmd/fev-debug stamp -lock "$1/etc/fev-debug/defaultset.lock") >"$1/etc/fev-debug/stamp"
	cp "$here/../debug/store/prefs.tsv" "$1/etc/fev-debug/prefs.tsv"
	chmod -R a+r "$1/etc/fev-debug"
	mkdir -p "$1/licenses/fev-debug"
	for mod in github.com/klauspost/compress@v1.20.1 github.com/ulikunitz/xz@v0.5.17; do
		lic="$("${GO:-go}" env GOMODCACHE)/$mod/LICENSE"
		if [ -r "$lic" ]; then cp "$lic" "$1/licenses/fev-debug/$(echo "$mod" | tr '/@' '__')"; fi
	done
	docker run --rm -i -v "$isostage_src":/src:ro -v "$runc_src/stage":/runc:ro \
		-v "$dbgstage_src":/dbg:ro \
		-v "$1/bin":/dest -v "$1/engine/usr/bin":/engine "$image" sh -eu -c '
		for p in /src/fev-isostage.c:/dest/fev-isostage /dbg/fev-dbgstage.c:/dest/fev-dbgstage /runc/fev-runc.c:/engine/fev-runc; do
			gcc -static -nostdlib -ffreestanding -fno-builtin -fno-stack-protector -fno-pie -no-pie \
				-fno-asynchronous-unwind-tables -Os -Wall -Wextra -Werror -Wl,--build-id=none \
				-o /tmp/stage "${p%%:*}"
			strip -o "${p#*:}" /tmp/stage
		done
	'
	chmod 755 "$1/bin/fev-isostage" "$1/bin/fev-dbgstage" "$1/engine/usr/bin/fev-runc"
	mkdir -p "$1/engine/usr/libexec/fev/byname"
	ln -sf /usr/bin/fev-runc "$1/engine/usr/libexec/fev/byname/runc"
	(cd "$here/.." && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "${GO:-go}" build -trimpath -ldflags "-s -w" \
		-o "$tools/kernelid" ./tools/kernelid)
	mkdir -p "$1/emu"
	cp "$emu_out/FEX" "$1/emu/FEX"
	"$emu_probe_build" --out "$emu_out/rosetta-probe"
	cp "$emu_out/rosetta-probe" "$1/emu/rosetta-probe"
	chmod 755 "$1/emu" "$1/emu/FEX"
	cp -R "$emu_out/licenses" "$1/licenses/fex"
	cat "$lock" "$engine/sources.lock" "$engine/../k8s/k3s.lock" "$(dirname "$emu_build")/sources.lock" | grep -v '^#' | grep . >"$1/licenses/sources.lock"
	"$engine/build.sh" --fetch "$cache/dl"
	"$engine/build.sh" --k3s "$1/engine"
	mkdir -p "$1/k8s"
	"$engine/build.sh" --k8s-pin >"$1/k8s/archive.json"
}

stage=$(mktemp -d "$cache/stage.XXXXXX")
tools=$(mktemp -d "$cache/tools.XXXXXX")
trap 'rm -rf "$stage" "$tools"; build_host_release' EXIT
stage_all "$stage"
ksum=$(kernel_sum)
set --
[ "$ksum" = none ] || set -- -v "$kernel_image:/kernel/Image:ro"
for a in $adds; do
	mkdir -p "$stage/$(dirname "${a#*:}")"
	cp "${a%%:*}" "$stage/${a#*:}"
	chmod 755 "$stage/${a#*:}"
done

rm -f "$out" "$out.kernel" "$out.kernel.tmp"
docker run --rm -i -v "$stage":/stage:ro -v "$cache/dl":/dl:ro -v "$engine":/engine-src:ro \
	-v "$tools/kernelid":/kernelid:ro "$@" -e KSUM="$ksum" \
	-v "$(dirname "$out")":/out -e OUT="$(basename "$out")" "$image" sh -eu -c '
	mkdir /tree && cp -a /stage/. /tree/
	sh /engine-src/build.sh --assemble /dl /tree/engine /tree/licenses
	/tree/engine/usr/bin/runc --version >/tree/engine/usr/libexec/fev/runc.version
	want=none
	[ ! -f /kernel/Image ] || want=$(/kernelid image /kernel/Image) || want=unknown
	have=$(/kernelid notes /sys/kernel/notes) || have=unknown
	features="not recorded"
	line="(not recorded)"
	case "$want" in
	none | unknown) ;;
	*)
		if [ "$want" = "$have" ]; then
			/tree/engine/usr/bin/runc features >/tree/engine/usr/libexec/fev/runc.features
			echo "$want" >/tree/engine/usr/libexec/fev/runc.kernel
			awk '"'"'{ for (i = 1; i <= NF; i++) if ($i !~ /^fev\./) print $i }'"'"' /proc/cmdline \
				>/tree/engine/usr/libexec/fev/runc.cmdline
			line=$(tr "\n" " " </tree/engine/usr/libexec/fev/runc.cmdline)
			features=recorded
		fi
		;;
	esac
	mkfs.erofs --quiet -T 0 --all-root -U 6665766f-6d00-4000-8000-000000000010 -L fev-system \
		"/out/$OUT.tmp" /tree
	printf "image %s\nkernel %s\nbuild-host %s\nfeatures %s\ncmdline %s\n" "$KSUM" "$want" "$have" "$features" "$line" >"/out/$OUT.kernel.tmp"
'
mv "$out.tmp" "$out"
mv "$out.kernel.tmp" "$out.kernel"
echo "built $out ($(wc -c <"$out" | tr -d ' ') bytes)"
grep -qx 'features recorded' "$out.kernel" || {
	_kwant=$(sed -n 's/^kernel //p' "$out.kernel")
	case "$_kwant" in
	none) _kwhy="there is no kernel/out/Image" ;;
	unknown) _kwhy="kernel/out/Image's build ID cannot be read" ;;
	*) _kwhy="the build host runs the kernel with build ID $(sed -n 's/^build-host //p' "$out.kernel"), not kernel/out/Image's $_kwant (once it runs this kernel, the next build writes the answer: tests/lib/buildhost.sh down, if it is up)" ;;
	esac
	echo "$0: runc's features answer is not in the image, so the engine's runtime asks runc for it at each start: $_kwhy" >&2
}
