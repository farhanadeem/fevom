#!/bin/sh
set -eu

if [ -n "${FEV_BUILD_ID:-}" ]; then
	echo "$FEV_BUILD_ID"
	exit 0
fi

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

id=$(git describe --tags --always 2>/dev/null) || {
	echo dev
	exit 0
}
skip=':(exclude,attr:fev-build-id=skip)'
if [ -n "$(git status --porcelain -- . "$skip" 2>/dev/null)" ]; then
	hash=$({
		git diff --binary HEAD -- . "$skip"
		git ls-files --others --exclude-standard -z -- . "$skip" | xargs -0 shasum -a 256 2>/dev/null
	} | shasum -a 256 | cut -c1-8)
	id="$id-dirty.$hash"
fi
echo "$id"
