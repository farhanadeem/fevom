#!/bin/sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
cargo=${CARGO:-$HOME/.cargo/bin/cargo}
go=${GO:-go}
check=
case "${1:-}" in
"") ;;
--check) check=1 ;;
*)
	echo "usage: $0 [--check]" >&2
	exit 2
	;;
esac

say() { printf '%s\n' "$*"; }
die() {
	echo "build: $*" >&2
	exit 1
}

want_rust=$(sed -n 's/^rust //p' tools/toolchains)
want_go=$(sed -n 's/^go //p' tools/toolchains)
want_swift=$(sed -n 's/^swift //p' tools/toolchains)
want_xcode=$(sed -n 's/^xcode //p' tools/toolchains)
have_rust=$("$(dirname "$cargo")/rustc" --version | cut -d' ' -f2)
have_go=$("$go" env GOVERSION | sed 's/^go//')
have_swift=$(. app/dev-env.sh && swift --version 2>/dev/null | sed -n 's/.*Apple Swift version \([0-9.]*\).*/\1/p' | head -1)
have_xcode=$(. app/dev-env.sh && xcodebuild -version 2>/dev/null | sed -n 's/^Xcode //p' | head -1)
[ "$have_rust" = "$want_rust" ] || die "rustc is $have_rust; tools/toolchains pins $want_rust"
[ "$have_go" = "$want_go" ] || die "go is $have_go; tools/toolchains pins $want_go"
[ "$have_swift" = "$want_swift" ] || die "swift is ${have_swift:-missing} (Xcode's, app/dev-env.sh); tools/toolchains pins $want_swift"
[ "$have_xcode" = "$want_xcode" ] || die "Xcode is ${have_xcode:-missing} (app/dev-env.sh's DEVELOPER_DIR); tools/toolchains pins $want_xcode"
tools/gen-lock.sh --check || die "inputs.lock does not match the lock files it gathers"

FEV_BUILD_ID=$(tools/build-id.sh)
export FEV_BUILD_ID
ZERO_AR_DATE=1
export ZERO_AR_DATE

if [ -z "$check" ]; then
	say "build $FEV_BUILD_ID (rust $have_rust, go $have_go, swift $have_swift, xcode $have_xcode)"
	FEV_BUILD_DOCKER_HOST_GIVEN=${FEV_BUILD_DOCKER_HOST:-}
	[ -n "${FEV_BUILD_DOCKER_HOST:-}" ] || { [ -f kernel/out/Image ] && [ -f guest/out/system.img ]; } ||
		die "no kernel/out/Image or guest/out/system.img to boot the build host with: build the first ones on another Linux Docker host (FEV_BUILD_DOCKER_HOST=unix:///PATH tools/build.sh)"
	. tests/lib/buildhost.sh
	build_host_docker
	say "kernel"
	ksum_before=$(shasum -a 256 kernel/out/Image | cut -c1-64)
	kernel/build.sh >/dev/null
	tools/kernel-manifest.sh kernel/out/Image
	if [ -z "${FEV_BUILD_DOCKER_HOST_GIVEN:-}" ] && [ "$(shasum -a 256 kernel/out/Image | cut -c1-64)" != "$ksum_before" ]; then
		build_host_release
		if fev_build_host status >/dev/null 2>&1; then
			say "build: the kernel changed, and the build host is up by hand or for another script: it still runs the old kernel, so the system image's runc answers are not recorded under the new one (tests/lib/buildhost.sh down when it is free, then tools/build.sh again)" >&2
		else
			say "build host: started again on the new kernel"
		fi
		build_host_docker
	fi
	say "guest: x86 translator"
	guest/emu/build.sh
	say "vmm"
	(cd vmm && RUSTFLAGS="${RUSTFLAGS:+$RUSTFLAGS }--remap-path-prefix=${CARGO_HOME:-$HOME/.cargo}=/cargo --remap-path-prefix=$root=/fev" \
		"$cargo" build --release --quiet -p fevvm)
	tools/sign-dev.sh vmm/target/release/fevvm 2>/dev/null
	say "guest: init image"
	guest/build.sh
	say "guest: system image"
	guest/sysimage/build.sh >/dev/null
	say "host"
	host/build.sh
	for name in docker docker-compose; do
		ln -sfn fev "host/out/$name"
	done
	say "guest: remote debug agent"
	guest/debug/remote/build.sh
	say "app"
	app/build.sh --release --bundle
fi

fail=0
stamped() {
	if [ "$2" = "$FEV_BUILD_ID" ]; then
		say "  $1: $2"
	else
		say "  $1: $2, NOT $FEV_BUILD_ID"
		fail=1
	fi
}
say "stamped:"
stamped fevvm "$(vmm/target/release/fevvm --version 2>/dev/null || echo missing)"
stamped fevd "$(host/out/fevd --version 2>/dev/null || echo missing)"
stamped fev-k8s "$(host/out/fev-k8s --version 2>/dev/null || echo missing)"
for bin in host/out/fev host/out/fevctl-dev host/out/agentctl guest/out/fev-init; do
	if [ -f "$bin" ] && LC_ALL=C grep -aq -- "$FEV_BUILD_ID" "$bin"; then
		say "  $(basename "$bin"): $FEV_BUILD_ID"
	else
		say "  $(basename "$bin"): NOT $FEV_BUILD_ID"
		fail=1
	fi
done
if fex=$(guest/emu/build.sh --check); then
	say "  $fex"
else
	say "  $fex"
	fail=1
fi
for f in kernel/out/Image guest/out/init.cpio guest/out/system.img; do
	[ -f "$f" ] || {
		say "  missing: $f"
		fail=1
	}
done
if m=$(tools/kernel-manifest.sh --check kernel/out/Image 2>&1); then
	say "  $m"
else
	say "  $m"
	fail=1
fi
for name in docker docker-compose; do
	if [ "$(readlink "host/out/$name" 2>/dev/null)" = fev ]; then
		say "  host/out/$name -> fev"
	else
		say "  host/out/$name: NOT a link to fev"
		fail=1
	fi
done
if [ -f kernel/out/Image ] && [ -f guest/out/system.img ]; then
	ksum=$(shasum -a 256 kernel/out/Image | cut -c1-64)
	if [ "$(sed -n 's/^image //p' guest/out/system.img.kernel 2>/dev/null)" = "$ksum" ] &&
		grep -qx 'features recorded' guest/out/system.img.kernel 2>/dev/null; then
		say "  system.img: runc's answers recorded under kernel/out/Image"
	else
		say "  system.img: runc's answers NOT recorded under kernel/out/Image ($(tr '\n' ' ' <guest/out/system.img.kernel 2>/dev/null || echo 'no system.img.kernel'))"
		fail=1
	fi
fi
say "signed:"
if [ -f host/out/fevd ]; then
	sig=$(tools/sign-dev.sh --check-fevd host/out/fevd) || fail=1
	say "  $sig"
else
	say "  fevd: missing"
	fail=1
fi
say "bundle:"
if [ -d app/out/Fevom.app ]; then
	if v=$(tools/verify-bundle.sh app/out/Fevom.app 2>&1); then
		say "  $(printf '%s\n' "$v" | tail -1)"
	else
		printf '%s\n' "$v" | grep -v '^ok ' | sed 's/^/  /'
		fail=1
	fi
else
	say "  missing: app/out/Fevom.app"
	fail=1
fi
[ "$fail" -eq 0 ] || die "the build is not of one identifier, or fevd is not signed as asked, or the bundle is not this build's; run tools/build.sh with the tree left alone"
say "ok"
