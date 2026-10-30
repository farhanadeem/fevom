#!/bin/sh
set -eu

usage() {
	cat <<'EOF' >&2
Make, check or remove the compatibility link for tools that ignore Docker
contexts and open /var/run/docker.sock.

  sudo host/docker/docker-sock-link.sh                 link /var/run/docker.sock to fev's socket
  host/docker/docker-sock-link.sh --status             say what the link is now; changes nothing
  sudo host/docker/docker-sock-link.sh --remove        remove the link, if it is fev's

  --socket PATH    fev's Docker socket (default ${FEV_HOME:-$HOME/.fev}/run/docker.sock;
                   under sudo, the home of the user who ran sudo)
  --force          replace, or with --remove take away, a link that points somewhere else
  LINK             the link's path, last (default /var/run/docker.sock)
EOF
	exit 2
}


mode=link
force=no
socket=
link=/var/run/docker.sock
while [ $# -gt 0 ]; do
	case "$1" in
	--status) mode=status ;;
	--remove) mode=remove ;;
	--force) force=yes ;;
	--socket)
		[ $# -ge 2 ] || usage
		socket=$2
		shift
		;;
	-h | --help) usage ;;
	-*) usage ;;
	*) link=$1 ;;
	esac
	shift
done

if [ -z "$socket" ]; then
	home=$HOME
	if [ -n "${SUDO_USER:-}" ]; then
		case "$SUDO_USER" in
		*[!A-Za-z0-9._-]*)
			echo "$0: cannot tell whose home to use; give --socket" >&2
			exit 2
			;;
		esac
		home=$(dscl . -read "/Users/$SUDO_USER" NFSHomeDirectory | awk '{ print $2 }')
		[ -n "$home" ] || {
			echo "$0: cannot find the home of $SUDO_USER; give --socket" >&2
			exit 2
		}
	fi
	socket=${FEV_HOME:-$home/.fev}/run/docker.sock
fi
case "$socket" in
/*) ;;
*)
	echo "$0: the socket must be an absolute path: $socket" >&2
	exit 2
	;;
esac

state() {
	if [ -L "$link" ]; then
		target=$(readlink "$link")
		if [ "$target" = "$socket" ]; then echo ours; else echo other; fi
	elif [ -e "$link" ]; then
		echo notlink
	else
		echo absent
	fi
}

now=$(state)
case "$mode" in
status)
	case "$now" in
	ours) echo "linked: $link -> $socket" ;;
	other) echo "points elsewhere: $link -> $(readlink "$link")" ;;
	notlink) echo "points elsewhere: $link is not a link" ;;
	absent) echo "absent: $link" ;;
	esac
	exit 0
	;;
remove)
	case "$now" in
	absent) echo "$link is not there; nothing to remove" ;;
	ours) rm "$link" && echo "removed $link" ;;
	other)
		if [ "$force" != yes ]; then
			echo "$0: $link points at $(readlink "$link"), which is not fev's socket; not removed (--force removes it)" >&2
			exit 1
		fi
		echo "removing $link, which pointed at $(readlink "$link")"
		rm "$link"
		;;
	notlink)
		echo "$0: $link is not a link; not removed" >&2
		exit 1
		;;
	esac
	exit 0
	;;
esac

case "$now" in
ours)
	echo "$link already points at $socket"
	exit 0
	;;
other)
	if [ "$force" != yes ]; then
		echo "$0: $link points at $(readlink "$link"): it belongs to another product, and was left alone." >&2
		echo "    To replace it: sudo $0 --force $link" >&2
		echo "    To put it back afterwards: sudo ln -sfn '$(readlink "$link")' '$link'" >&2
		exit 1
	fi
	echo "replacing $link, which pointed at $(readlink "$link")"
	;;
notlink)
	echo "$0: $link is there and is not a link: an engine on this Mac is serving it. It was left alone." >&2
	exit 1
	;;
esac
if ! ln -sfn "$socket" "$link" 2>/dev/null; then
	echo "$0: cannot write $link; run this with sudo" >&2
	exit 1
fi
echo "$link -> $socket"
[ -S "$socket" ] || echo "note: $socket is not there yet; the link works once fev is running"
