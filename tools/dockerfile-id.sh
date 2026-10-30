#!/bin/sh
set -eu
[ $# -ge 1 ] || {
	echo "usage: $0 FILE..." >&2
	exit 2
}
awk '!/^[[:space:]]*#/ || /^#[[:space:]]*(syntax|escape|check)[[:space:]]*=/' "$@" | shasum -a 256 | cut -c1-12
