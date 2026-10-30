#!/bin/sh
set -eu

usage() {
	cat <<'EOF' >&2
Remove fev: runs the installed app's `fev uninstall`.

  tools/uninstall.sh [--delete-data] [--dry-run] [--home DIR] [--no-ask]

  --delete-data  also delete fev's home and data (every container, image,
                 volume and machine), the app's preferences and the HTTPS key
  --dry-run      print every step; change nothing
  --home DIR     the scratch user home tools/install.sh --home used
  --no-ask       ask nothing; leave the admin helper and the HTTPS trust
EOF
}

scratch=
delete=
dry=
noask=
while [ $# -gt 0 ]; do
	case "$1" in
	--delete-data) delete=--delete-data ;;
	--dry-run) dry=--dry-run ;;
	--home)
		scratch=$2
		shift
		;;
	--no-ask) noask=1 ;;
	*)
		usage
		exit 2
		;;
	esac
	shift
done

if [ -n "$scratch" ]; then
	scratch=$(cd "$scratch" && pwd)
	HOME=$scratch
	FEV_HOME=${FEV_HOME:-$scratch/fev}
	DOCKER_CONFIG=${DOCKER_CONFIG:-$scratch/.docker}
	FEV_DOCKER_CONFIG=$DOCKER_CONFIG
	export HOME FEV_HOME DOCKER_CONFIG FEV_DOCKER_CONFIG
fi
fev_home=${FEV_HOME:-$HOME/.fev}
record=$fev_home/install.record
fev=
if [ -f "$record" ]; then
	fev=$(sed -n 's/^app //p' "$record")/Contents/MacOS/bin/fev
fi
[ -n "$fev" ] && [ -x "$fev" ] || fev=$fev_home/bin/fev
[ -x "$fev" ] || {
	echo "uninstall: no fev in $record or $fev_home/bin: run the uninstall of the app you installed (its menu, Uninstall Fevom…)" >&2
	exit 1
}

set -- uninstall
[ -z "$delete" ] || set -- "$@" "$delete"
[ -z "$dry" ] || set -- "$@" "$dry"
[ -z "$noask" ] || set -- "$@" --yes --keep-admin
[ -z "$scratch" ] || [ -n "$noask" ] || set -- "$@" --yes
exec "$fev" "$@"
