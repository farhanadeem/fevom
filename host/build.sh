#!/bin/sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
cd "$here"
out=${FEV_HOST_OUT:-$here/out}
mkdir -p "$out"
build=$("$here/../tools/build-id.sh")
"${GO:-go}" build -trimpath -ldflags "-X fev/proto/version.Build=$build" -o "$out/" ./cmd/...
"$here/../tools/sign-dev.sh" --fevd "$out/fevd"
cp "$here/keysigner/fev-keysigner" "$out/fev-keysigner.new"
mv -f "$out/fev-keysigner.new" "$out/fev-keysigner"
