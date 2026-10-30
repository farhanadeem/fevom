#!/bin/sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
cd "$here"
out=${FEV_GUEST_OUT:-$here/out}
mkdir -p "$out"
build=$("$here/../tools/build-id.sh")
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "${GO:-go}" build -trimpath \
	-ldflags "-s -w -X fev/proto/version.Build=$build" -o "$out/fev-init" ./init
tools/mkinitrd.sh "$out/fev-init" >"$out/init.cpio"
