#!/bin/sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
lock="$root/tools/debug-index.lock"
cache="${FEV_DEBUG_INDEX_CACHE:-$root/out/cache/debug-index}"
arch=both
check=0
while [ $# -gt 0 ]; do
  case "$1" in
    -cache) cache=$2; shift 2 ;;
    -arch) arch=$2; shift 2 ;;
    --check) check=1; shift ;;
    *) echo "debug-index.sh: unknown argument $1" >&2; exit 2 ;;
  esac
done
mkdir -p "$cache"

lockfield() {
  awk -v k="$1" -v sel="$2" -v f="$3" '
    $1==k && index($0, sel) {
      for (i=2;i<=NF;i++){ n=index($i,"="); if(substr($i,1,n-1)==f){print substr($i,n+1); exit} }
    }' "$lock"
}

fetch() {
  name=$1
  url=$(lockfield input "name=$name	" url)
  want=$(lockfield input "name=$name	" sha256)
  [ -n "$url" ] || { echo "debug-index.sh: no input $name in $lock" >&2; exit 1; }
  f="$cache/$name"
  if [ ! -f "$f" ] || [ "$(shasum -a 256 "$f" | cut -d' ' -f1)" != "$want" ]; then
    echo "debug-index.sh: fetching $name" >&2
    curl -fsSL -o "$f.tmp" "$url"
    got=$(shasum -a 256 "$f.tmp" | cut -d' ' -f1)
    [ "$got" = "$want" ] || { echo "debug-index.sh: $name sha256 $got, lock says $want" >&2; exit 1; }
    mv "$f.tmp" "$f"
  fi
  echo "$f"
}

echo "debug-index.sh: building host/cmd/debug-index" >&2
bin="$cache/debug-index"
(cd "$root/host" && "${GO:-go}" build -o "$bin" ./cmd/debug-index)

pkgs=$(fetch packages.json.br)
_=$(fetch store-paths.xz)
release=$(lockfield channel release= release)

build_one() {
  a=$1
  full=$(fetch "index-$a-linux")
  tsv="$cache/table-$a.tsv"
  "$bin" build -full "$full" -packages "$pkgs" -out "$tsv" \
    -header "$release $a-linux" -stats "$cache/stats-$a.json"
  got=$(shasum -a 256 "$tsv" | cut -d' ' -f1)
  want=$(lockfield output "arch=$a	" table_sha256)
  zst="$root/guest/debug/store/index-$a-linux.tsv.zst"
  if [ "$check" = 1 ]; then
    if [ "$got" != "$want" ]; then
      echo "debug-index.sh: $a table sha256 $got, lock says $want" >&2
      exit 1
    fi
    [ -f "$zst" ] || { echo "debug-index.sh: $zst is missing" >&2; exit 1; }
    zgot=$(zstd -dc "$zst" | shasum -a 256 | cut -d' ' -f1)
    if [ "$zgot" != "$want" ]; then
      echo "debug-index.sh: $zst decompresses to $zgot, lock says $want (the committed table drifted)" >&2
      exit 1
    fi
    echo "debug-index.sh: $a table and committed zst match the lock ($got)" >&2
    return
  fi
  zstd -19 -q -f -o "$zst" "$tsv"
  tmp=$(mktemp)
  awk -v a="$a" -v h="$got" '
    $1=="output" && index($0,"arch="a"\t") {
      for(i=2;i<=NF;i++){ n=index($i,"="); if(substr($i,1,n-1)=="table_sha256") $i="table_sha256="h }
    }1' OFS='\t' "$lock" > "$tmp" && mv "$tmp" "$lock"
  echo "debug-index.sh: wrote $zst (table sha256 $got)" >&2
}

case "$arch" in
  both) build_one aarch64; build_one x86_64 ;;
  aarch64|x86_64) build_one "$arch" ;;
  *) echo "debug-index.sh: -arch aarch64|x86_64|both" >&2; exit 2 ;;
esac
echo "debug-index.sh: done" >&2
