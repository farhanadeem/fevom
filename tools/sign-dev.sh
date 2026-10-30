#!/bin/sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
fevd_id=dev.fevom.fevd

case "${1:-}" in
--fevd)
	[ $# -eq 2 ] || {
		echo "usage: $0 --fevd BINARY" >&2
		exit 2
	}
	if [ -z "${FEV_SIGN_IDENTITY:-}" ]; then
		codesign --sign - --force --entitlements "$root/tools/entitlements/fevd.plist" "$2"
		exit
	fi
	codesign --force --timestamp=none --sign "$FEV_SIGN_IDENTITY" --identifier "$fevd_id" --entitlements "$root/tools/entitlements/fevd.plist" "$2" || {
		echo "sign-dev: could not sign $2 with '$FEV_SIGN_IDENTITY' (security find-identity -v -p codesigning lists the identities)" >&2
		exit 1
	}
	;;
--check-fevd)
	[ $# -eq 2 ] || {
		echo "usage: $0 --check-fevd BINARY" >&2
		exit 2
	}
	bin=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
	codesign --verify --strict "$bin" 2>/dev/null || {
		echo "fevd: the signature of $bin is not valid"
		exit 1
	}
	info=$(codesign -dvv "$bin" 2>&1)
	codesign -d --entitlements - --xml "$bin" 2>/dev/null | grep -q '<key>com.apple.security.virtualization</key><true/>' || {
		echo "fevd: $bin lacks the com.apple.security.virtualization entitlement (tools/sign-dev.sh --fevd)"
		exit 1
	}
	[ "$(codesign -d --entitlements - --xml "$bin" 2>/dev/null | grep -o '<key>' | wc -l | tr -d ' ')" = 1 ] || {
		echo "fevd: $bin has entitlements other than com.apple.security.virtualization (tools/sign-dev.sh --fevd)"
		exit 1
	}
	ident=$(printf '%s\n' "$info" | sed -n 's/^Identifier=//p')
	leaf=$(printf '%s\n' "$info" | sed -n 's/^Authority=//p' | head -1)
	if printf '%s\n' "$info" | grep -q '^Signature=adhoc$'; then
		what="ad-hoc"
	else
		what="'$leaf', identifier $ident"
	fi
	if [ -z "${FEV_SIGN_IDENTITY:-}" ]; then
		echo "fevd: $what (FEV_SIGN_IDENTITY unset)"
		exit 0
	fi
	[ "$what" != ad-hoc ] || {
		echo "fevd: ad-hoc, NOT '$FEV_SIGN_IDENTITY'"
		exit 1
	}
	[ "$ident" = "$fevd_id" ] || {
		echo "fevd: identifier $ident, NOT $fevd_id"
		exit 1
	}
	match=
	if printf '%s' "$FEV_SIGN_IDENTITY" | grep -Eq '^[0-9A-Fa-f]{40}$'; then
		certs=$(mktemp -d "${TMPDIR:-/tmp}/fevd-certs.XXXXXX")
		(cd "$certs" && codesign -d --extract-certificates=c "$bin" 2>/dev/null) || :
		sha=$( (shasum -a 1 <"$certs/c0" | cut -d' ' -f1 | tr a-f A-F) 2>/dev/null || :)
		rm -rf "$certs"
		[ "$sha" != "$(printf '%s' "$FEV_SIGN_IDENTITY" | tr a-f A-F)" ] || match=1
	else
		case "$leaf" in *"$FEV_SIGN_IDENTITY"*) match=1 ;; esac
	fi
	[ -n "$match" ] || {
		echo "fevd: $what, NOT '$FEV_SIGN_IDENTITY'"
		exit 1
	}
	echo "fevd: $what"
	;;
-*)
	echo "usage: $0 BINARY... | --fevd BINARY | --check-fevd BINARY" >&2
	exit 2
	;;
*)
	for bin in "$@"; do
		codesign --sign - --force --entitlements "$root/tools/entitlements/vmm.plist" "$bin"
	done
	;;
esac
