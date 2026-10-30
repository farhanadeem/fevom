#!/bin/sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)
. "$here/dev-env.sh"

config=debug
helpers=""
run_tests=0
whole=0
opt=""
while [ $# -gt 0 ]; do
	case "$1" in
	--release) config=release ;;
	--helpers) helpers=$2; shift ;;
	--test) run_tests=1 ;;
	--bundle) whole=1 ;;
	--optimized) opt="-Xswiftc -O --scratch-path .build/opt" ;;
	*) echo "usage: app/build.sh [--release|--optimized] [--helpers DIR] [--test] [--bundle]" >&2; exit 2 ;;
	esac
	shift
done
die() {
	echo "app/build.sh: $*" >&2
	exit 1
}
if [ -z "$helpers" ] && [ -x "$root/host/out/fevd" ]; then
	helpers=$root/host/out
fi

(cd "$here/tools/swiftgen" && "${GO:-go}" run . -proto "$root/proto/daemon" -o "$here/Sources/FevomAPI/DaemonAPI.generated.swift")

build=$("$root/tools/build-id.sh")

cd "$here"
[ -n "$opt" ] && [ "$config" = release ] && die "--optimized is a debug build's; --release is optimized already"
src=$here
if [ "$config" = release ]; then
	src=/tmp/fev-app-release
	lock=$src.lock
	until mkdir "$lock" 2>/dev/null; do
		pid=$(cat "$lock/pid" 2>/dev/null || true)
		if [ -n "$pid" ] && ! kill -0 "$pid" 2>/dev/null; then
			rm -rf "$lock"
		else
			sleep 1
		fi
	done
	echo $$ >"$lock/pid"
	trap 'rm -rf "$lock"' EXIT
	mkdir -p "$src"
	rsync -r -l -p --checksum --delete --exclude /.build/ --exclude /out/ "$here/" "$src/"
	ZERO_AR_DATE=1
	export ZERO_AR_DATE
fi
cd "$src"
swift build -c "$config" $opt --product Fevom ${SWIFT_JOBS:+-j "$SWIFT_JOBS"}
bin=$(swift build -c "$config" $opt --show-bin-path)
cd "$here"
if [ "$run_tests" = 1 ]; then
	swift test ${SWIFT_JOBS:+-j "$SWIFT_JOBS"}
fi

out=${FEV_APP_OUT:-$here/out}
app=$out/Fevom.app
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Helpers" "$app/Contents/Resources"
cp "$bin/Fevom" "$app/Contents/MacOS/Fevom"
mkdir -p "$app/Contents/Frameworks"
ditto "$bin/Sparkle.framework" "$app/Contents/Frameworks/Sparkle.framework"
rm -rf "$app/Contents/Frameworks/Sparkle.framework/XPCServices" "$app/Contents/Frameworks/Sparkle.framework/Versions/B/XPCServices"
cp "$here/Resources/AppIcon.icns" "$app/Contents/Resources/AppIcon.icns"
ident=dev.fevom.app
[ "$config" = debug ] && ident=dev.fevom.app.debug
case "$build" in
*-*) version=0.0 ;;
v[0-9]*.[0-9]*.[0-9]*) version=${build#v} ;;
*) version=0.0 ;;
esac
bundle_version=$build
[ "$version" != 0.0 ] && bundle_version=$version
sed -e "s/@BUILD@/$build/g" -e "s/@BUNDLEVERSION@/$bundle_version/g" -e "s/@VERSION@/$version/g" -e "s#<string>dev.fevom.app</string>#<string>$ident</string>#" "$here/Info.plist.in" > "$app/Contents/Info.plist"
if [ -n "${FEV_UPDATE_FEED:-}" ] && [ "$config" = release ] && [ "$version" != 0.0 ]; then
	case "$FEV_UPDATE_FEED" in https://*) ;; *) die "FEV_UPDATE_FEED is not https: $FEV_UPDATE_FEED" ;; esac
	key=$(cat "$root/tools/update/public-key")
	pl() { /usr/libexec/PlistBuddy -c "$1" "$app/Contents/Info.plist"; }
	pl "Add :SUFeedURL string $FEV_UPDATE_FEED"
	pl "Add :SUPublicEDKey string $key"
	pl "Add :SURequireSignedFeed bool true"
	pl "Add :SUVerifyUpdateBeforeExtraction bool true"
	pl "Add :SUSignedFeedFailureExpirationInterval integer 0"
	pl "Add :SUEnableAutomaticChecks bool true"
	pl "Add :SUScheduledCheckInterval integer 86400"
	pl "Add :SUAutomaticallyUpdate bool false"
	pl "Add :SUAllowsAutomaticUpdates bool true"
	pl "Add :CFBundleURLTypes array"
	pl "Add :CFBundleURLTypes:0 dict"
	pl "Add :CFBundleURLTypes:0:CFBundleURLName string dev.fevom.app.update"
	pl "Add :CFBundleURLTypes:0:CFBundleURLSchemes array"
	pl "Add :CFBundleURLTypes:0:CFBundleURLSchemes:0 string fev"
fi
[ "$config" = debug ] && /usr/libexec/PlistBuddy -c "Add :LSUIElement bool true" "$app/Contents/Info.plist"

if [ -n "$helpers" ]; then
	for b in fevd; do
		cp "$helpers/$b" "$app/Contents/Helpers/$b"
	done
	if [ -x "$helpers/fev" ]; then
		mkdir -p "$app/Contents/MacOS/bin"
		cp "$helpers/fev" "$app/Contents/MacOS/bin/fev"
	fi
fi

if [ "$whole" = 1 ]; then
	host_out=${helpers:-$root/host/out}
	need() { [ -f "$1" ] || die "no $1: build it first (tools/build.sh)"; }
	for f in "$host_out/fevd" "$host_out/fev-k8s" "$host_out/fev" "$host_out/fev-privhelper" "$host_out/dockerhost-dev" \
		"$root/host/keysigner/fev-keysigner" "$root/vmm/target/release/fevvm" \
		"$root/kernel/out/Image" "$root/guest/out/init.cpio" "$root/guest/out/system.img"; do
		need "$f"
	done
	put() {
		mkdir -p "$(dirname "$2")"
		cp -c "$1" "$2" 2>/dev/null || cp "$1" "$2"
	}
	put "$host_out/fevd" "$app/Contents/Helpers/fevd"
	put "$host_out/fev-k8s" "$app/Contents/Helpers/fev-k8s"
	put "$host_out/fev-privhelper" "$app/Contents/Helpers/fev-privhelper"
	put "$root/host/keysigner/fev-keysigner" "$app/Contents/Helpers/fev-keysigner"
	put "$root/vmm/target/release/fevvm" "$app/Contents/Helpers/fevvm"
	put "$host_out/fev" "$app/Contents/MacOS/bin/fev"
	for name in fevctl docker docker-compose; do
		ln -s fev "$app/Contents/MacOS/bin/$name"
	done
	tools_home=$(mktemp -d "${TMPDIR:-/tmp}/fev-tools.XXXXXX")
	"$host_out/dockerhost-dev" tools install --home "$tools_home" \
		--cache "${FEV_TOOLS_CACHE:-$root/out/cache/docker-tools}" >/dev/null ||
		{ rm -rf "$tools_home"; die "the Docker client tools could not be installed"; }
	mkdir -p "$app/Contents/MacOS/xbin"
	for t in "$tools_home/tools"/*; do
		[ -f "$t" ] && [ ! -L "$t" ] && put "$t" "$app/Contents/MacOS/xbin/$(basename "$t")"
	done
	rm -rf "$tools_home"
	"$root/tools/kernel-manifest.sh" --check "$root/kernel/out/Image" >/dev/null 2>&1 ||
		"$root/tools/kernel-manifest.sh" "$root/kernel/out/Image"
	for f in kernel/out/Image kernel/out/Image.manifest guest/out/init.cpio guest/out/system.img; do
		put "$root/$f" "$app/Contents/Resources/guest/$(basename "$f")"
	done
	for arch in arm64 amd64; do
		d=$root/guest/out/debug-agent/$arch
		[ -d "$d" ] || die "no $d: build it first (guest/debug/remote/build.sh)"
		for f in fev-debug-agent fev-debug-stage fev-debug defaultset.lock stamp; do
			need "$d/$f"
			put "$d/$f" "$app/Contents/Resources/guest/debug-agent/$arch/$f"
		done
	done
	FEV_BUILD_ID=$build FEV_APP_PACKAGES=$src/.build "$root/tools/licenses.sh" "$app/Contents/Resources/licenses" >/dev/null
fi

runtime=""
entitlements=""
if [ "$config" = release ]; then
	runtime="--options runtime"
	entitlements="--entitlements $root/tools/entitlements/app.plist"
fi
fw=$app/Contents/Frameworks/Sparkle.framework
codesign --force --sign - --timestamp=none $runtime "$fw/Versions/B/Autoupdate" "$fw/Versions/B/Updater.app"
codesign --force --sign - --timestamp=none $runtime "$fw"
codesign --force --sign - --timestamp=none $runtime $entitlements "$app"
what=""
[ "$whole" = 1 ] && what=", the whole bundle"
echo "$app ($config, build $build$what)"
