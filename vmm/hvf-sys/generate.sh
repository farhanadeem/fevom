#!/bin/sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
sdk=$(xcrun --show-sdk-path)

bindgen "$here/wrapper.h" \
	--allowlist-function 'hv_.*' \
	--allowlist-type 'hv_.*' \
	--allowlist-var 'HV_.*' \
	--blocklist-function hv_vcpu_set_simd_fp_reg \
	--default-enum-style consts \
	--no-prepend-enum-name \
	--no-layout-tests \
	--no-doc-comments \
	--merge-extern-blocks \
	--rust-target 1.82 \
	-o "$here/src/bindings.rs" \
	-- -isysroot "$sdk" -target arm64-apple-macos26.0

echo "wrote $here/src/bindings.rs from $sdk"
