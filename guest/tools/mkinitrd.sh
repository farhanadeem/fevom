#!/bin/sh
set -eu

[ $# -eq 1 ] && [ -f "$1" ] || {
	echo "usage: $0 BINARY > init.cpio" >&2
	exit 2
}

header() {
	printf '070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X' \
		"$1" "$2" 0 0 "$3" 0 "$4" 0 0 0 0 "$5" 0
}
pad() {
	n=$(((4 - $1 % 4) % 4))
	[ "$n" = 0 ] || head -c "$n" /dev/zero
}

size=$(wc -c <"$1" | tr -d ' ')
header 1 $((0100755)) 1 "$size" 5
printf 'init\0'
pad 115
cat "$1"
pad "$size"
header 0 0 1 0 11
printf 'TRAILER!!!\0'
pad 121
