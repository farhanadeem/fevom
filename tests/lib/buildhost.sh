#!/bin/sh
if [ -z "${root:-}" ] || [ ! -f "$root/inputs.lock" ]; then
	root=$(cd "$(dirname "$0")" && while [ ! -f inputs.lock ]; do
		[ "$PWD" != / ] || exit 1
		cd ..
	done && pwd) || {
		echo "tests/lib/buildhost.sh: $0 is not inside the repository" >&2
		exit 1
	}
fi

_bh_dir() { echo "${FEV_BUILD_HOST_DIR:-/tmp/bb.$(id -u)}"; }

_bh_alive() {
	_bh_d=$(_bh_dir)
	[ -f "$_bh_d/sock" ] && [ -f "$_bh_d/up.pid" ] && kill -0 "$(cat "$_bh_d/up.pid")" 2>/dev/null &&
		[ -S "$(sed 's|^unix://||' "$_bh_d/sock")" ]
}

_bh_users() {
	_bh_n=0
	for _bh_f in "$(_bh_dir)"/users/*; do
		[ -f "$_bh_f" ] || continue
		if kill -0 "${_bh_f##*/}" 2>/dev/null; then
			_bh_n=$((_bh_n + 1))
		else
			rm -f "$_bh_f"
		fi
	done
	echo "$_bh_n"
}

_bh_up() {
	_bh_d=$(_bh_dir)
	mkdir -p "$_bh_d/users"
	_bh_alive && return 0
	_bh_tries=0
	until mkdir "$_bh_d/lock.d" 2>/dev/null; do
		_bh_holder=$(cat "$_bh_d/lock.d/pid" 2>/dev/null || echo 0)
		if [ "$_bh_holder" != 0 ] && ! kill -0 "$_bh_holder" 2>/dev/null; then
			rm -rf "$_bh_d/lock.d"
			continue
		fi
		_bh_tries=$((_bh_tries + 1))
		[ "$_bh_tries" -lt 900 ] || {
			echo "build host: another start has held $_bh_d/lock.d for 15 minutes" >&2
			return 1
		}
		sleep 1
	done
	echo $$ >"$_bh_d/lock.d/pid"
	if _bh_alive; then
		rm -rf "$_bh_d/lock.d"
		return 0
	fi
	_bh_free=$(memory_pressure 2>/dev/null | sed -n 's/.*free percentage: \([0-9]*\)%.*/\1/p')
	if [ -n "$_bh_free" ] && [ "$_bh_free" -lt "${FEV_BUILD_HOST_MIN_FREE:-20}" ]; then
		echo "build host: only $_bh_free% of memory is free (under ${FEV_BUILD_HOST_MIN_FREE:-20}%); not booting one" >&2
		rm -rf "$_bh_d/lock.d"
		return 1
	fi
	rm -f "$_bh_d/sock" "$_bh_d/manual"
	echo "build host: starting a scratch fev engine in $_bh_d (log: $_bh_d/log)" >&2
	"${FEV_BUILD_HOST_RUNNER:-$root/tests/lib/buildhost-run.sh}" "$_bh_d" </dev/null >"$_bh_d/log" 2>&1 &
	echo $! >"$_bh_d/up.pid"
	_bh_tries=0
	while [ ! -f "$_bh_d/sock" ]; do
		if ! kill -0 "$(cat "$_bh_d/up.pid")" 2>/dev/null; then
			echo "build host: the start failed; its log ends:" >&2
			tail -15 "$_bh_d/log" | sed 's/^/  /' >&2
			rm -rf "$_bh_d/lock.d"
			return 1
		fi
		_bh_tries=$((_bh_tries + 1))
		[ "$_bh_tries" -lt 1800 ] || {
			echo "build host: not up after 30 minutes (log: $_bh_d/log)" >&2
			_bh_down
			rm -rf "$_bh_d/lock.d"
			return 1
		}
		sleep 1
	done
	rm -rf "$_bh_d/lock.d"
	echo "build host: up, $(cat "$_bh_d/sock")" >&2
	_bh_room
}

_bh_room() {
	_bh_free=$(df -m "$(_bh_dir)" 2>/dev/null | awk 'NR == 2 { print $4 }')
	[ -z "$_bh_free" ] || [ "$_bh_free" -ge 6144 ] ||
		echo "build host: the Mac has $_bh_free MiB free; its engine refuses writes past fev's 5 GiB margin, so builds and tests in it may fail with no space left on device" >&2
}

_bh_down() {
	_bh_d=$(_bh_dir)
	[ -f "$_bh_d/up.pid" ] || return 0
	_bh_pid=$(cat "$_bh_d/up.pid")
	perl -e 'use Fcntl; sysopen(my $f, $ARGV[0], O_WRONLY | O_NONBLOCK) or exit 1; close $f' "$_bh_d/stop.fifo" 2>/dev/null ||
		kill "$_bh_pid" 2>/dev/null
	_bh_tries=0
	while kill -0 "$_bh_pid" 2>/dev/null; do
		_bh_tries=$((_bh_tries + 1))
		[ "$_bh_tries" -lt 120 ] || {
			kill "$_bh_pid" 2>/dev/null
			break
		}
		sleep 1
	done
	rm -f "$_bh_d/up.pid" "$_bh_d/sock" "$_bh_d/stop.fifo" "$_bh_d/manual"
	echo "build host: down (its home is kept: $_bh_d/home)" >&2
}

build_host_hold() {
	rm -f "$1/stop.fifo"
	mkfifo "$1/stop.fifo"
	echo "$2" >"$1/sock.part"
	mv "$1/sock.part" "$1/sock"
	: <"$1/stop.fifo"
	rm -f "$1/sock" "$1/stop.fifo"
}

_bh_env() {
	_bh_host=${FEV_BUILD_DOCKER_HOST:-$(cat "$(_bh_dir)/sock")}
	_bh_cfg=${FEV_BUILD_DOCKER_CONFIG:-$(_bh_dir)/docker-config}
	_bh_cli=${FEV_BUILD_DOCKER_CLI:-$("$root/tests/fixtures/docker-cli.sh")} || return 1
	mkdir -p "$_bh_cfg"
	cat <<END
FEV_BUILD_DOCKER_HOST='$_bh_host'; export FEV_BUILD_DOCKER_HOST
FEV_BUILD_DOCKER_CONFIG='$_bh_cfg'; export FEV_BUILD_DOCKER_CONFIG
FEV_BUILD_DOCKER_CLI='$_bh_cli'; export FEV_BUILD_DOCKER_CLI
PATH='$root/tests/lib/buildhost-bin':\$(printf '%s' "\$PATH" | awk -v RS=: -v ORS=: '\$0 != "$root/tests/lib/buildhost-bin"' | sed 's/:\$//'); export PATH
END
	[ "${1:-}" != hand ] || cat <<END
DOCKER_HOST='$_bh_host'; export DOCKER_HOST
DOCKER_CONFIG='$_bh_cfg'; export DOCKER_CONFIG
unset DOCKER_CONTEXT
END
}

fev_build_host() {
	case "${1:-}" in
	up)
		_bh_up || return 1
		: >"$(_bh_dir)/manual"
		;;
	env)
		[ -n "${FEV_BUILD_DOCKER_HOST:-}" ] || _bh_alive || {
			_bh_up || return 1
			: >"$(_bh_dir)/manual"
		}
		_bh_env hand
		;;
	down) _bh_down ;;
	reset)
		_bh_down
		rm -rf "$(_bh_dir)"
		echo "build host: its home is removed" >&2
		;;
	status)
		if _bh_alive; then
			echo "up: $(cat "$(_bh_dir)/sock") ($(_bh_users) scripts using it$([ -f "$(_bh_dir)/manual" ] && echo ", started by hand"))"
		else
			echo "down"
			return 1
		fi
		;;
	*)
		echo "fev_build_host up|env|down|reset|status" >&2
		return 2
		;;
	esac
}

build_host_docker() {
	if [ -z "${FEV_BUILD_DOCKER_HOST:-}" ]; then
		_bh_up || return 1
		_bh_d=$(_bh_dir)
		: >"$_bh_d/users/$$"
		if [ -n "${build_host_at_exit:-}" ]; then
			"$build_host_at_exit" build_host_release
		else
			trap build_host_release EXIT
			trap 'exit 130' INT
			trap 'exit 143' TERM HUP
		fi
	fi
	eval "$(_bh_env)" || return 1
}

build_host_release() {
	_bh_d=$(_bh_dir)
	[ -f "$_bh_d/users/$$" ] || return 0
	rm -f "$_bh_d/users/$$"
	unset FEV_BUILD_DOCKER_HOST
	[ ! -f "$_bh_d/manual" ] && [ "$(_bh_users)" = 0 ] || return 0
	_bh_down
}

case "${0##*/}" in
buildhost.sh)
	set -eu
	fev_build_host "$@"
	;;
esac
