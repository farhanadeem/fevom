#!/bin/sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
others=
case "${1:-}" in
--for-others)
	others=1
	shift
	;;
esac
[ $# -eq 1 ] && [ -d "$1" ] || {
	echo "usage: $0 [--for-others] APP" >&2
	exit 2
}
app=$(cd "$1" && pwd)
C=$app/Contents
fails=0
checks=0
ok() {
	checks=$((checks + 1))
	echo "ok    $*"
}
bad() {
	checks=$((checks + 1))
	fails=$((fails + 1))
	echo "FAIL  $*"
}

missing=
for f in Info.plist MacOS/Fevom MacOS/bin/fev Helpers/fevd Helpers/fev-k8s Helpers/fevvm Helpers/fev-privhelper \
	Helpers/fev-keysigner MacOS/xbin/kubectl Resources/guest/Image Resources/guest/Image.manifest Resources/guest/init.cpio \
	Resources/guest/system.img MacOS/xbin/docker MacOS/xbin/docker-compose MacOS/xbin/docker-buildx \
	MacOS/xbin/docker-credential-osxkeychain Resources/licenses/README.txt \
	Frameworks/Sparkle.framework/Versions/B/Sparkle Frameworks/Sparkle.framework/Versions/B/Autoupdate \
	Frameworks/Sparkle.framework/Versions/B/Updater.app/Contents/MacOS/Updater \
	Resources/guest/debug-agent/arm64/fev-debug-agent Resources/guest/debug-agent/arm64/fev-debug-stage \
	Resources/guest/debug-agent/arm64/fev-debug Resources/guest/debug-agent/arm64/defaultset.lock \
	Resources/guest/debug-agent/arm64/stamp \
	Resources/guest/debug-agent/amd64/fev-debug-agent Resources/guest/debug-agent/amd64/fev-debug-stage \
	Resources/guest/debug-agent/amd64/fev-debug Resources/guest/debug-agent/amd64/defaultset.lock \
	Resources/guest/debug-agent/amd64/stamp; do
	[ -f "$C/$f" ] || missing="$missing $f"
done
for name in fevctl docker docker-compose; do
	[ "$(readlink "$C/MacOS/bin/$name" 2>/dev/null)" = fev ] || missing="$missing MacOS/bin/$name(->fev)"
done
ro=$(find "$app" ! -type l ! -perm -u+w | head -3 | sed "s|^$app/||" | tr '\n' ' ')
[ -z "$ro" ] || missing="$missing (not writable by its owner: $ro...)"
if [ -z "$missing" ]; then ok "layout: every file of the bundle, and fevctl, docker, docker-compose -> fev"; else bad "layout: missing$missing"; fi

if out=$(codesign --verify --strict --deep "$app" 2>&1); then
	ok "seal: the bundle's signature and every nested one hold"
else
	bad "seal: $(printf '%s' "$out" | tr '\n' ' ')"
fi

ent() {
	codesign -d --entitlements - --xml "$1" 2>/dev/null | tr -d ' \t\n' | grep -q "<key>$2</key><true/>"
}
if ent "$C/Helpers/fevvm" com.apple.security.hypervisor; then
	ok "entitlements: fevvm has com.apple.security.hypervisor"
else
	bad "entitlements: fevvm lacks com.apple.security.hypervisor"
fi
if ent "$C/Helpers/fevd" com.apple.security.virtualization; then
	ok "entitlements: fevd has com.apple.security.virtualization"
else
	bad "entitlements: fevd lacks com.apple.security.virtualization"
fi
hardened() { codesign -dv "$1" 2>&1 | grep -q '^CodeDirectory.*flags=.*runtime'; }
if ent "$C/MacOS/Fevom" com.apple.security.cs.allow-dyld-environment-variables; then
	bad "entitlements: the app takes DYLD_ variables (allow-dyld-environment-variables)"
elif ! hardened "$C/MacOS/Fevom"; then
	ok "entitlements: the app is a debug build's, no hardened runtime"
elif ent "$C/MacOS/Fevom" com.apple.security.cs.disable-library-validation; then
	ok "entitlements: the hardened app loads its ad-hoc Sparkle, and no DYLD_ variables"
else
	bad "entitlements: the hardened app lacks com.apple.security.cs.disable-library-validation: it cannot load Sparkle"
fi

P=$C/Info.plist
pv() { plutil -extract "$1" raw -o - "$P" 2>/dev/null; }
up=
fw=$C/Frameworks/Sparkle.framework/Versions/B
[ ! -e "$fw/XPCServices" ] || up="$up Sparkle's XPC services are there"
if hardened "$C/MacOS/Fevom"; then
	for f in "$fw/Autoupdate" "$fw/Updater.app"; do
		hardened "$f" || up="$up ${f#"$C/"} without the hardened runtime"
	done
fi
feed=$(pv SUFeedURL || true)
if [ -n "$feed" ]; then
	case "$feed" in https://*) ;; *) up="$up the feed is not https ($feed)" ;; esac
	[ "$(pv SUPublicEDKey || true)" = "$(cat "$root/tools/update/public-key")" ] || up="$up SUPublicEDKey is not tools/update/public-key's"
	for k in SURequireSignedFeed SUVerifyUpdateBeforeExtraction; do
		[ "$(pv $k || true)" = true ] || up="$up $k not true"
	done
	[ "$(pv SUSignedFeedFailureExpirationInterval || true)" = 0 ] || up="$up SUSignedFeedFailureExpirationInterval not 0"
	v=$(pv CFBundleVersion || true)
	case "$v" in [0-9]*.[0-9]*.[0-9]*) ;; *) up="$up CFBundleVersion $v is not X.Y.Z" ;; esac
	[ "$v" = "$(pv CFBundleShortVersionString || true)" ] || up="$up CFBundleVersion $v is not the version shown"
	what="the feed $feed, fev's key, both signatures required"
else
	what="no feed (not a release's build: it never checks)"
fi
if [ -z "$up" ]; then ok "update: $what"; else bad "update:$up"; fi

want=$(plutil -extract FevomBuild raw -o - "$C/Info.plist" 2>/dev/null || echo none)
got=""
for b in Helpers/fevd Helpers/fev-k8s Helpers/fevvm; do
	v=$("$C/$b" --version 2>/dev/null || echo missing)
	[ "$v" = "$want" ] || got="$got $(basename "$b")=$v"
done
for b in MacOS/bin/fev Resources/guest/init.cpio; do
	[ -f "$C/$b" ] && LC_ALL=C grep -aq -- "$want" "$C/$b" || got="$got $(basename "$b")(not $want)"
done
if [ "$want" != none ] && [ -z "$got" ]; then
	ok "build: $want in Info.plist, fevd, fev-k8s, fevvm, fev and the init image"
else
	bad "build: Info.plist says $want;$got"
fi

da_bad=
for arch in arm64 amd64; do
	a=$C/Resources/guest/debug-agent/$arch/fev-debug-agent
	[ -f "$a" ] && LC_ALL=C grep -aq -- "$want" "$a" || da_bad="$da_bad $arch"
done
if [ "$want" != none ] && [ -z "$da_bad" ]; then
	ok "debug-agent: arm64 and amd64 agents carry $want"
else
	bad "debug-agent: not $want in:$da_bad"
fi
elf_machine() { od -An -tx1 -j18 -N2 "$1" 2>/dev/null | tr -d ' \n'; }
da_bad=
for pair in arm64:aarch64-linux:b700 amd64:x86_64-linux:3e00; do
	arch=${pair%%:*} rest=${pair#*:} want_arch=${rest%%:*} machine=${rest#*:}
	d=$C/Resources/guest/debug-agent/$arch
	head -3 "$d/defaultset.lock" 2>/dev/null | grep -q "($want_arch)" || da_bad="$da_bad $arch:lock"
	LC_ALL=C grep -aqiE 'musl|glibc|GNU C Library' "$d/fev-debug-stage" 2>/dev/null && da_bad="$da_bad $arch:stage-libc"
	[ -s "$d/stamp" ] || da_bad="$da_bad $arch:stamp"
	for prog in fev-debug-agent fev-debug-stage fev-debug; do
		[ "$(elf_machine "$d/$prog")" = "$machine" ] || da_bad="$da_bad $arch:$prog-machine"
	done
done
if [ -z "$da_bad" ]; then
	ok "debug-agent: each architecture's own ELF machine, default set and stamp; stages link no C library"
else
	bad "debug-agent:$da_bad"
fi

cd_have=$(codesign -dvvv "$C/Helpers/fev-keysigner" 2>&1 | sed -n 's/^CDHash=//p')
cd_want=$(tr -d ' \n' <"$root/host/keysigner/CDHASH" 2>/dev/null || true)
if [ -n "$cd_have" ] && [ "$cd_have" = "$cd_want" ] && cmp -s "$C/Helpers/fev-keysigner" "$root/host/keysigner/fev-keysigner"; then
	ok "keysigner: the kept binary, cdhash $cd_have"
else
	bad "keysigner: cdhash ${cd_have:-none}, host/keysigner/CDHASH ${cd_want:-none}, or not the kept binary byte for byte"
fi

if m=$("$root/tools/kernel-manifest.sh" --check "$C/Resources/guest/Image" 2>&1); then
	ok "kernel: $m"
else
	bad "kernel: $m"
fi

tools_bad=
while read -r kind name version _ sum url member; do
	[ "$kind" = tool ] || continue
	f=$C/MacOS/xbin/$name
	if [ -z "${member:-}" ]; then
		[ "$(shasum -a 256 "$f" 2>/dev/null | cut -c1-64)" = "$sum" ] || tools_bad="$tools_bad $name"
	else
		"$f" --version 2>/dev/null | grep -q "version $version" || tools_bad="$tools_bad $name"
	fi
done <"$root/host/docker/tools/tools.lock"
if [ -z "$tools_bad" ]; then ok "tools: the pinned client tools (the Docker CLI, Compose, Buildx, the credential helper, kubectl)"; else bad "tools: not the pinned$tools_bad"; fi

k8s_deps() { LC_ALL=C grep -ao "dep	k8s\.io/[^	]*" "$1" 2>/dev/null | cut -f2 | sort -u | tr '\n' ' '; }
if [ -n "$(k8s_deps "$C/Helpers/fevd")" ]; then
	bad "k8s: fevd links $(k8s_deps "$C/Helpers/fevd")"
elif ! k8s_deps "$C/Helpers/fev-k8s" | grep -q 'k8s.io/client-go'; then
	bad "k8s: fev-k8s links no client-go: not the cluster's client"
else
	ok "k8s: fevd links no k8s.io module; fev-k8s links client-go"
fi

L=$C/Resources/licenses
lic_missing=
for f in README.txt SOURCE-OFFER.txt TERMS.txt LICENSE inputs.lock mac/go/go/LICENSE mac/swift/SwiftTerm mac/swift/Sparkle/LICENSE \
	guest/go/go/LICENSE guest/kernel/COPYING guest/engine/moby/LICENSE guest/engine/runc/LICENSE \
	guest/engine/containerd/LICENSE guest/engine/libseccomp/LICENSE guest/engine/tini/LICENSE \
	guest/fex/BUILD guest/fex/glibc/LGPL-2.1 guest/fex/libstdc++/GPL-3 guest/fex/fex/LICENSE \
	guest/busybox/LICENSE guest/btrfs-progs/COPYING guest/cacert/MPL-2.0; do
	[ -e "$L/$f" ] || lic_missing="$lic_missing $f"
done
for t in "$C/MacOS/xbin"/*; do
	[ -f "$L/mac/docker-tools/$(basename "$t")/LICENSE" ] || lic_missing="$lic_missing mac/docker-tools/$(basename "$t")"
done
for name in $(sed -n 's/^source \([^ ]*\) .*snapshot\.debian\.org.*/\1/p' "$root/guest/docker/sources.lock"); do
	[ -f "$L/guest/debian/$name/copyright" ] || lic_missing="$lic_missing guest/debian/$name"
done
[ -s "$L/mac/go/MODULES" ] && [ -s "$L/guest/go/MODULES" ] && [ -s "$L/mac/rust/CRATES" ] || lic_missing="$lic_missing (MODULES/CRATES lists)"
while read -r _ into lib _ pkg src ver; do
	case "$into" in
	docker-engine)
		[ -n "$(ls "$L/guest/engine/$lib" 2>/dev/null)" ] || lic_missing="$lic_missing guest/engine/$lib"
		;;
	k3s)
		;;
	*)
		[ -s "$L/guest/$into/linked/$lib/copyright" ] || lic_missing="$lic_missing guest/$into/linked/$lib"
		[ -z "$ver" ] || grep -qxF "$lib $pkg $src $ver" "$L/guest/$into/linked/VERSIONS" 2>/dev/null ||
			lic_missing="$lic_missing guest/$into/linked/VERSIONS($lib $ver)"
		;;
	esac
done <<EOF
$(grep '^linked ' "$L/inputs.lock" 2>/dev/null)
EOF
if [ -z "$lic_missing" ]; then
	ok "licences: $(find "$L" -type f | wc -l | tr -d ' ') files, every part's text, the source offer and the terms"
else
	bad "licences: missing$lic_missing"
fi

if [ -n "$others" ]; then
	sig=$(codesign -dvv "$C/Helpers/fevd" 2>&1)
	if printf '%s\n' "$sig" | grep -q '^Signature=adhoc'; then
		ok "for others: fevd is ad-hoc, no identity"
	else
		bad "for others: fevd carries an identity ($(printf '%s\n' "$sig" | grep -m1 '^Authority=' || echo '?')); build with FEV_SIGN_IDENTITY unset"
	fi
fi

echo "verify-bundle: $checks checks, $fails failed ($app)"
[ "$fails" -eq 0 ]
