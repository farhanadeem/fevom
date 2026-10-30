#!/bin/sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
out=
cache=$root/out/cache/notices
while [ $# -gt 0 ]; do
	case "$1" in
	--cache)
		cache=$2
		shift
		;;
	-*)
		echo "usage: $0 OUT [--cache DIR]" >&2
		exit 2
		;;
	*) out=$1 ;;
	esac
	shift
done
[ -n "$out" ] || {
	echo "usage: $0 OUT [--cache DIR]" >&2
	exit 2
}
go=${GO:-go}
cargo=${CARGO:-$HOME/.cargo/bin/cargo}
die() {
	echo "licenses: $*" >&2
	exit 1
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/fev-licenses.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
L=$tmp/licenses
mkdir -p "$L" "$cache"

copy_licences() {
	mkdir -p "$2"
	found=0
	for f in "$1"/*; do
		case "$(basename "$f" | tr '[:lower:]' '[:upper:]')" in
		LICENSE* | LICENCE* | COPYING* | NOTICE* | PATENTS* | UNLICENSE* | COPYRIGHT*)
			[ -f "$f" ] || continue
			cp "$f" "$2/"
			found=1
			;;
		esac
	done
	[ "$found" = 1 ] || die "no licence file in $1 ($3)"
}

go_modules() {
	dir=$1
	dest=$2
	shift 2
	(cd "$dir" && env "$@" "$go" list -deps -f '{{with .Module}}{{.Path}} {{.Version}} {{.Dir}}{{end}}' $pkgs) |
		sort -u | while read -r path version mdir; do
		case "$path" in fev/*) continue ;; esac
		[ -n "$mdir" ] || die "module $path $version is not in the module cache (go mod download)"
		copy_licences "$mdir" "$dest/$path" "$path $version"
		echo "$path $version" >>"$dest/MODULES"
	done
}
pkgs="./cmd/fevd ./cmd/fev ./cmd/fev-privhelper ./cmd/fev-k8s"
go_modules "$root/host" "$L/mac/go"
pkgs="./init ./cmd/..."
go_modules "$root/guest" "$L/guest/go" GOOS=linux GOARCH=arm64 CGO_ENABLED=0
goroot=$("$go" env GOROOT)
golicence=
for f in "$goroot/LICENSE" "$goroot/../LICENSE"; do
	[ -f "$f" ] && golicence=$f && break
done
[ -n "$golicence" ] || die "no LICENSE for Go at $goroot"
for d in mac guest; do
	mkdir -p "$L/$d/go/go"
	cp "$golicence" "$L/$d/go/go/LICENSE"
	[ ! -f "$goroot/PATENTS" ] || cp "$goroot/PATENTS" "$L/$d/go/go/PATENTS"
	echo "go $("$go" env GOVERSION)" >>"$L/$d/go/MODULES"
done

(cd "$root/vmm" && "$cargo" metadata --format-version 1 --offline) >"$tmp/cargo.json"
(cd "$root/vmm" && "$cargo" tree -p fevvm -e normal --prefix none --format '{p}' --offline) | sort -u >"$tmp/crates"
python3 -I - "$tmp/cargo.json" "$tmp/crates" >"$tmp/crate-dirs" <<'PY'
import json, os, sys
meta = json.load(open(sys.argv[1]))
want = set()
for line in open(sys.argv[2]):
    f = line.split()
    if len(f) >= 2 and "(" not in line:
        want.add((f[0], f[1].lstrip("v")))
for p in meta["packages"]:
    if (p["name"], p["version"]) in want:
        print(p["name"], p["version"], os.path.dirname(p["manifest_path"]), p.get("license") or "-")
PY
mkdir -p "$L/mac/rust"
while read -r name version dir licence; do
	copy_licences "$dir" "$L/mac/rust/$name-$version" "crate $name $version"
	echo "$name $version $licence" >>"$L/mac/rust/CRATES"
done <"$tmp/crate-dirs"

packages=${FEV_APP_PACKAGES:-$root/app/.build}
term=$packages/checkouts/SwiftTerm
[ -d "$term" ] || die "no $term: build the app first (app/build.sh)"
copy_licences "$term" "$L/mac/swift/SwiftTerm" SwiftTerm
sparkle=$packages/artifacts/sparkle/Sparkle
[ -d "$sparkle" ] || die "no $sparkle: build the app first (app/build.sh)"
copy_licences "$sparkle" "$L/mac/swift/Sparkle" Sparkle

sys=$root/guest/out/sysimage
for d in "$sys"/busybox-*; do
	[ -f "$d/LICENSE" ] || continue
	mkdir -p "$L/guest/busybox"
	cp "$d/LICENSE" "$d/config" "$L/guest/busybox/"
done
for d in "$sys"/btrfs-progs-*; do
	[ -f "$d/COPYING" ] || continue
	mkdir -p "$L/guest/btrfs-progs"
	cp "$d/COPYING" "$L/guest/btrfs-progs/"
done
[ -d "$L/guest/busybox" ] && [ -d "$L/guest/btrfs-progs" ] || die "no busybox or btrfs-progs build under $sys: build the system image first"
for into in btrfs-progs busybox; do
	for d in "$sys/$into"-*; do
		[ -f "$d/linked/VERSIONS" ] || continue
		rm -rf "$L/guest/$into/linked"
		cp -R "$d/linked" "$L/guest/$into/linked"
	done
done
awk '$1 == "linked" && ($2 == "btrfs-progs" || $2 == "busybox") { print $2, $3 }' "$root/guest/sysimage/sources.lock" |
	while read -r into lib; do
		[ -s "$L/guest/$into/linked/$lib/copyright" ] || die "no copyright file of $lib, linked into $into (guest/sysimage/build.sh writes it: build the system image)"
	done
[ -f "$root/guest/out/emu/licenses/BUILD" ] || die "no guest/out/emu/licenses: build FEX first (guest/emu/build.sh)"
cp -R "$root/guest/out/emu/licenses" "$L/guest/fex"

mkdir -p "$tmp/deb"
for name in $(sed -n 's/^source \([^ ]*\) .*snapshot\.debian\.org.*/\1/p' "$root/guest/docker/sources.lock"); do
	sum=$(awk -v n="$name" '$1 == "source" && $2 == n { print $5 }' "$root/guest/docker/sources.lock")
	deb=$(ls "$sys/dl/$(echo "$sum" | cut -c1-12)"-*.deb 2>/dev/null | head -1)
	[ -n "$deb" ] || die "no download of $name in $sys/dl: build the system image first"
	echo "$sum  $deb" | shasum -a 256 -c - >/dev/null || die "$deb is not what guest/docker/sources.lock pins"
	data=$(ar t "$deb" | grep '^data\.tar')
	(cd "$tmp/deb" && ar p "$deb" "$data" | tar -xf - ./usr/share/doc 2>/dev/null) || true
done
for name in $(sed -n 's/^source \([^ ]*\) .*snapshot\.debian\.org.*/\1/p' "$root/guest/docker/sources.lock"); do
	c=$tmp/deb/usr/share/doc/$name/copyright
	[ -f "$c" ] || die "no copyright file in the $name package"
	mkdir -p "$L/guest/debian/$name"
	cp -L "$c" "$L/guest/debian/$name/copyright"
done

grep '^notice ' "$root/tools/licenses/notices.lock" | while read -r _ path version sum url; do
	file=$cache/$(echo "$sum" | cut -c1-16)
	if [ ! -f "$file" ]; then
		/usr/bin/curl -fsSL --retry 3 --proto '=https' --tlsv1.2 -o "$file.part" "$url" || die "cannot fetch $url"
		mv "$file.part" "$file"
	fi
	echo "$sum  $file" | shasum -a 256 -c - >/dev/null || {
		rm -f "$file"
		die "$url is not what tools/licenses/notices.lock pins ($path)"
	}
	mkdir -p "$(dirname "$L/$path")"
	cp "$file" "$L/$path"
done

[ -f "$L/guest/fex/glibc/LGPL-2.1" ] || die "no LGPL-2.1 text in guest/fex/glibc"
mkdir -p "$L/guest/engine/glibc"
cp "$L/guest/fex/glibc/LGPL-2.1" "$L/guest/engine/glibc/LGPL-2.1"

cp "$root/inputs.lock" "$L/inputs.lock"
cp "$root/tools/licenses/SOURCE-OFFER.txt" "$root/tools/licenses/TERMS.txt" "$root/LICENSE" "$L/"
(
	cd "$L"
	for top in mac guest; do
		find "$top" -mindepth 2 -type f ! -name MODULES ! -name CRATES | sed 's|/[^/]*$||' | sort -u | while read -r d; do
			printf '  %-60s %s\n' "$d" "$(cd "$d" && find . -maxdepth 1 -type f ! -name MODULES ! -name CRATES | sed 's|^\./||' | sort | tr '\n' ' ')"
		done
	done
) >"$tmp/index"
build=${FEV_BUILD_ID:-$("$root/tools/build-id.sh")}
awk -v build="$build" -v index_file="$tmp/index" '
	/@INDEX@/ { while ((getline line < index_file) > 0) print line; next }
	{ gsub(/@BUILD@/, build); print }
' "$root/tools/licenses/README.txt.in" >"$L/README.txt"
chmod -R u+w,a+rX,go-w "$L"

rm -rf "$out.tmp"
mkdir -p "$(dirname "$out")"
mv "$L" "$out.tmp"
rm -rf "$out"
mv "$out.tmp" "$out"
echo "licenses: $(find "$out" -type f | wc -l | tr -d ' ') files in $out"
