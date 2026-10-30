#!/bin/sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
app=${1:-$root/app/out/Fevom.app}
image=${SMOKE_IMAGE:-public.ecr.aws/docker/library/alpine:3.22}
fev=$app/Contents/MacOS/bin/fev
docker=$app/Contents/MacOS/xbin/docker
for f in "$fev" "$docker"; do
	[ -x "$f" ] || {
		echo "smoke: no $f (tools/build.sh makes the app)" >&2
		exit 2
	}
done

home=$(mktemp -d /tmp/bs.XXXXXX)
cfg=$(mktemp -d /tmp/bs-docker.XXXXXX)
cleanup() {
	limit 120 "$fev" --home "$home" stop >/dev/null 2>&1 || :
	rm -rf "$home" "$cfg"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

limit() {
	_s=$1
	shift
	perl -e 'alarm shift; exec @ARGV' "$_s" "$@"
}
fail() {
	echo "FAIL: $*" >&2
	echo "fev's log: $home/log (kept until this script ends)" >&2
	tail -n 40 "$home"/log/*.log 2>/dev/null >&2 || :
	exit 1
}
ok() { echo "ok    $*"; }

limit 180 "$fev" --home "$home" start >"$cfg/start.out" 2>&1 ||
	fail "fev start: $(cat "$cfg/start.out")"
ok "fev start"

export DOCKER_HOST="unix://$home/run/docker.sock" DOCKER_CONFIG="$cfg"
limit 60 "$docker" version >/dev/null 2>&1 || fail "docker version: the engine does not answer"
ok "the engine answers"

limit 300 "$docker" pull -q "$image" >/dev/null || fail "docker pull $image"
out=$(limit 120 "$docker" run --rm "$image" sh -c 'echo hello from $(uname -s) $(uname -m)') ||
	fail "docker run"
[ "$out" = "hello from Linux aarch64" ] || fail "docker run said: $out"
ok "docker run: $out"

limit 30 "$fev" --home "$home" status >"$cfg/status.out" 2>&1 || fail "fev status: $(cat "$cfg/status.out")"
grep -qi running "$cfg/status.out" || fail "fev status does not say running: $(cat "$cfg/status.out")"
ok "fev status"

limit 120 "$fev" --home "$home" stop >/dev/null 2>&1 || fail "fev stop"
ok "fev stop"
echo "smoke: passed"
