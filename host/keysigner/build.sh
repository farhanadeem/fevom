#!/bin/sh
set -eu

usage() {
	cat <<'EOF'
fev-keysigner, the one program that holds fev's CA key. It is built
once and kept in the repository beside its source: the login keychain binds
the key to the binary's code directory hash.

  host/keysigner/build.sh --check    rebuild into a scratch directory and compare with the
                                     kept binary, byte for byte, and its hash with CDHASH
                                     (exit 0 the same, 1 different, 2 another toolchain)
  host/keysigner/build.sh --rotate   replace the kept binary and CDHASH: a key rotation for
                                     every install (its key unusable until `fev setup
                                     --https` makes a new key and root)
  host/keysigner/build.sh --to FILE  build into FILE

The toolchain is pinned in TOOLCHAIN; --check on another one says so and
exits 2.
EOF
}
here=$(cd "$(dirname "$0")" && pwd)
src=$here/fev-keysigner.c
kept=$here/fev-keysigner
sdk=$(xcrun --sdk macosx --show-sdk-path)
toolchain() {
	printf '%s\n' "$(xcrun --sdk macosx clang --version | head -1)" "sdk $(xcrun --sdk macosx --show-sdk-version)"
}
build() {
	d=$(mktemp -d "${TMPDIR:-/tmp}/keysigner.XXXXXX")
	cp "$src" "$d/fev-keysigner.c"
	(cd "$d" && xcrun --sdk macosx clang -std=c11 -Os -arch arm64 -mmacosx-version-min=26.0 \
		-isysroot "$sdk" -Wall -Werror -g0 -fno-ident -ffile-prefix-map="$d"=. \
		-framework Security -framework CoreFoundation \
		-Wl,-S -Wl,-x -o fev-keysigner fev-keysigner.c)
	codesign -s - -f -i dev.fevom.keysigner "$d/fev-keysigner" 2>/dev/null
	mv "$d/fev-keysigner" "$1"
	rm -rf "$d"
}
cdhash() { codesign -dvvv "$1" 2>&1 | sed -n 's/^CDHash=//p'; }
case ${1:-} in
--check)
	if [ "$(toolchain)" != "$(cat "$here/TOOLCHAIN")" ]; then
		echo "keysigner: another toolchain than TOOLCHAIN records:" >&2
		toolchain >&2
		exit 2
	fi
	t=$(mktemp "${TMPDIR:-/tmp}/keysigner-check.XXXXXX")
	build "$t"
	same=0
	cmp -s "$t" "$kept" && [ "$(cdhash "$t")" = "$(cat "$here/CDHASH")" ] && [ "$(cdhash "$kept")" = "$(cat "$here/CDHASH")" ] || same=1
	if [ $same = 0 ]; then echo "keysigner: the kept binary rebuilds bit for bit (cdhash $(cat "$here/CDHASH"))"; else
		echo "keysigner: the rebuild differs from the kept binary (kept $(cdhash "$kept"), CDHASH $(cat "$here/CDHASH"), rebuilt $(cdhash "$t"))" >&2
	fi
	rm -f "$t"
	exit $same ;;
--rotate)
	build "$kept"
	cdhash "$kept" >"$here/CDHASH"
	toolchain >"$here/TOOLCHAIN"
	echo "keysigner: a new signer, cdhash $(cat "$here/CDHASH"): every install's CA key is unusable from its next start until fev setup --https replaces it" ;;
--to)
	build "${2:?--to FILE}" ;;
*)
	usage
	exit 2 ;;
esac
