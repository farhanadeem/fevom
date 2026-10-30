#!/bin/sh
set -eu

action=$1
variant=${2:-release}
pristine=/work/linux-$KERNEL_VERSION
fragments="/kernel/config/*.config"
overlays=/kernel/src
patches=$(ls /kernel/patches/*.patch 2>/dev/null || true)
suffix=
case "$variant" in
release)
	tree=/work/tree/linux-$KERNEL_VERSION
	obj=/work/obj
	out=/out
	;;
debug)
	tree=/work/tree/linux-$KERNEL_VERSION
	obj=/work/obj-debug
	out=/out/debug
	fragments="$fragments /kernel/debug.config"
	;;
extra | extra-debug)
	[ -n "${KERNEL_EXTRA:-}" ] && [ -n "${KERNEL_EXTRA_NAME:-}" ] || {
		echo "the extra variants need KERNEL_EXTRA and KERNEL_EXTRA_NAME (kernel/build.sh extra DIR)" >&2
		exit 2
	}
	tree=/work/$KERNEL_EXTRA_NAME/linux-$KERNEL_VERSION
	obj=/work/$KERNEL_EXTRA_NAME/obj-$variant
	out=/out/$KERNEL_EXTRA_NAME
	overlays="$overlays $KERNEL_EXTRA/src"
	patches="$patches $(ls "$KERNEL_EXTRA"/patches/*.patch 2>/dev/null || true)"
	fragments="$fragments $(ls "$KERNEL_EXTRA"/config/*.config 2>/dev/null || true)"
	if [ "$variant" = extra-debug ]; then
		suffix=.debug
		fragments="$fragments /kernel/debug.config"
	fi
	;;
*)
	echo "unknown variant $variant" >&2
	exit 2
	;;
esac

exec 9>/work/.build.lock
flock 9

fetch() {
	[ -d "$pristine" ] && return
	echo "fetching linux-$KERNEL_VERSION"
	major=${KERNEL_VERSION%%.*}
	curl -fsSL -o /work/linux.tar.xz \
		"https://cdn.kernel.org/pub/linux/kernel/v$major.x/linux-$KERNEL_VERSION.tar.xz"
	echo "$KERNEL_SHA256  /work/linux.tar.xz" | sha256sum -c -
	tar -xf /work/linux.tar.xz -C /work
	rm /work/linux.tar.xz
}

put() {
	if ! cmp -s "$1" "$2"; then
		mkdir -p "$(dirname "$2")"
		cp "$1" "$2"
		echo "  updated ${2#"$tree"/}"
	fi
}

assemble() {
	mkdir -p "$(dirname "$tree")"
	[ -d "$tree" ] || cp -a "$pristine" "$tree"

	added=
	for o in $overlays; do
		[ -d "$o" ] || continue
		for f in $(cd "$o" && find . -type f | sed 's|^\./||' | sort); do
			[ ! -e "$pristine/$f" ] || {
				echo "$o/$f exists upstream: change an upstream file with a patch, not an overlay" >&2
				exit 1
			}
			put "$o/$f" "$tree/$f"
			added="$added $f"
		done
	done

	stage=/work/.stage-$variant
	rm -rf "/work/.stage-$variant"
	mkdir -p "$stage"
	touched=
	if [ -n "$(echo $patches)" ]; then
		touched=$(cat $patches | sed -n 's|^+++ b/\([^[:space:]]*\).*|\1|p' | sort -u)
	fi
	for f in $touched; do
		mkdir -p "$stage/$(dirname "$f")"
		cp "$pristine/$f" "$stage/$f"
	done
	for p in $patches; do
		patch -d "$stage" -p1 --fuzz=0 --no-backup-if-mismatch -s <"$p" || {
			echo "patch $(basename "$p") does not apply to linux-$KERNEL_VERSION" >&2
			exit 1
		}
	done
	for f in $touched; do
		put "$stage/$f" "$tree/$f"
	done
	rm -rf "/work/.stage-$variant"

	manifest=$tree.manifest
	now=$(for f in $added $touched; do echo "$f"; done | sort -u)
	if [ -f "$manifest" ]; then
		while read -r f; do
			[ -n "$f" ] || continue
			echo "$now" | grep -qxF "$f" && continue
			if [ -f "$pristine/$f" ]; then
				put "$pristine/$f" "$tree/$f"
			elif [ -f "$tree/$f" ]; then
				(cd "$tree" && rm -f "./$f")
				echo "  removed $f"
			fi
		done <"$manifest"
	fi
	echo "$now" >"$manifest"
}

kmake() {
	make -C "$tree" -s LLVM=1 ARCH=arm64 O="$obj" "$@"
}

configure() {
	mkdir -p "$obj"
	kmake allnoconfig
	(cd "$tree" && scripts/kconfig/merge_config.sh -m -O "$obj" "$obj/.config" $fragments) >/dev/null
	kmake olddefconfig

	missing=0
	for want in $(cat $fragments | grep -E '^CONFIG_[A-Za-z0-9_]+=' | sort -u); do
		name=${want%%=*}
		value=${want#*=}
		if [ "$value" = n ]; then
			if grep -q "^$name=" "$obj/.config"; then
				echo "config: $name should be off but is set" >&2
				missing=1
			fi
		elif ! grep -qx "$want" "$obj/.config"; then
			echo "config: requested $want, got: $(grep -E "^(# )?$name[= ]" "$obj/.config" || echo absent)" >&2
			missing=1
		fi
	done
	[ "$missing" -eq 0 ] || {
		echo "config: fragments ask for options the kernel did not enable" >&2
		exit 1
	}
}

case "$action" in
build)
	fetch
	assemble
	configure
	start=$(date +%s)
	kmake -j"$(nproc)" Image
	end=$(date +%s)
	mkdir -p "$out"
	cp "$obj/arch/arm64/boot/Image" "$out/.Image$suffix.new"
	mv "$out/.Image$suffix.new" "$out/Image$suffix"
	cp "$obj/.config" "$out/config$suffix"
	echo "built linux-$KERNEL_VERSION ($variant) in $((end - start)) s, Image $(stat -c %s "$out/Image$suffix") bytes, $(grep -c '=y$' "$out/config$suffix") options enabled, sha256 $(sha256sum "$out/Image$suffix" | cut -c1-16), headers archive $(sha256sum "$obj/kernel/kheaders_data.tar.xz" | cut -c1-16)"
	;;
clean)
	case "$obj" in
	/work/obj | /work/obj-debug | /work/*/obj-extra | /work/*/obj-extra-debug) rm -rf "$obj" ;;
	esac
	;;
menuconfig)
	fetch
	assemble
	configure
	kmake menuconfig
	;;
*)
	echo "unknown action $action" >&2
	exit 2
	;;
esac
