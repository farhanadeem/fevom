#!/bin/sh
set -eu

dir=$1
root=$(cd "$(dirname "$0")/../.." && pwd)
. "$root/tests/lib/buildhost.sh"
FEV_BUILD_HOST_STARTING=1
export FEV_BUILD_HOST_STARTING
cargo=${CARGO:-$HOME/.cargo/bin/cargo}

kernel=${KERNEL:-$root/kernel/out/Image}
[ -f "$kernel" ] || {
	echo "no $kernel: build the first one on another Linux Docker host (FEV_BUILD_DOCKER_HOST=unix:///PATH kernel/build.sh)" >&2
	exit 1
}
"$root/guest/sysimage/build.sh"

FEV_BUILD_ID=$("$root/tools/build-id.sh")
export FEV_BUILD_ID
bin=$dir/bin
home=$dir/home
mkdir -p "$bin" "$home"
(cd "$root/vmm" && { "$cargo" build --release --quiet --offline -p fevvm 2>/dev/null || "$cargo" build --release --quiet -p fevvm; })
cp "$root/vmm/target/release/fevvm" "$bin/fevvm"
"$root/tools/sign-dev.sh" "$bin/fevvm" >/dev/null
FEV_HOST_OUT=$bin "$root/host/build.sh" >/dev/null
FEV_GUEST_OUT=$bin "$root/guest/build.sh" >/dev/null
cp "$kernel" "$bin/Image"
cp "$root/guest/out/system.img" "$bin/system.img"

unset FEV_HOME FEV_FAKE_VM FEV_DAEMON_ARGS FEV_DOCKER_CONFIG FEV_COMPAT_SOCKET \
	DOCKER_HOST DOCKER_CONTEXT DOCKER_CONFIG SSH_AUTH_SOCK || true
FEV_VMM=$bin/fevvm
FEV_KERNEL=$bin/Image
FEV_INITRD=$bin/init.cpio
FEV_SYSTEM=$bin/system.img
FEV_DAEMON=$bin/fevd
FEV_SHARE_ROOT=/
export FEV_VMM FEV_KERNEL FEV_INITRD FEV_SYSTEM FEV_DAEMON FEV_SHARE_ROOT

stop_host() {
	rm -f "$dir/sock" "$dir/stop.fifo"
	"$bin/fev" --home "$home" stop >/dev/null 2>&1 || true
}
trap stop_host EXIT
trap 'exit 1' INT TERM HUP

"$bin/fev" --home "$home" start
echo "up, build $FEV_BUILD_ID: unix://$home/run/docker.sock"
build_host_hold "$dir" "unix://$home/run/docker.sock"
