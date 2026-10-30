#!/bin/sh
set -eu

usage() {
	cat <<'EOF' >&2
The Docker engine's root filesystem in the guest's system image.

  guest/docker/build.sh                   fetch and verify the pinned inputs,
                                          then build the system image
  guest/docker/build.sh --fetch DIR       fetch into DIR what is not there and
                                          verify every input against the lock
  guest/docker/build.sh --assemble DL DEST LICENSES
                                          lay the root out in DEST from the
                                          verified downloads in DL (in the Linux
                                          build container)
  guest/docker/build.sh --k3s DEST        k3s's mount points and links in DEST
  guest/docker/build.sh --k8s-pin         the first enable's pins, as JSON
EOF
}

here=$(cd "$(dirname "$0")" && pwd)
lock="$here/sources.lock"
k3slock="$here/../k8s/k3s.lock"

names() { awk '$1 == "source" { print $2 }' "$lock"; }
locked() {
	awk -v name="$1" -v field="$2" '$1 == "source" && $2 == name { print $field }' "$lock"
}
file_of() {
	echo "$(locked "$1" 5 | cut -c1-12)-$(basename "$(locked "$1" 6)")"
}
sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}
verify() {
	[ "$(sha256 "$2")" = "$(locked "$1" 5)" ] || {
		echo "$0: checksum mismatch for $2: it is not what sources.lock pins as $1" >&2
		return 1
	}
}

fetch() {
	mkdir -p "$1"
	for name in $(names); do
		file="$1/$(file_of "$name")"
		if [ ! -f "$file" ]; then
			curl -fsSL --retry 3 -o "$file.part" "$(locked "$name" 6)"
			mv "$file.part" "$file"
		fi
		verify "$name" "$file"
	done
}

assemble() {
	dl=$1
	dest=$2
	licenses=$3
	work=$(mktemp -d)
	for name in $(names); do
		verify "$name" "$dl/$(file_of "$name")"
	done

	mkdir -p "$dest/usr/bin" "$dest/usr/sbin" "$dest/usr/lib/aarch64-linux-gnu" "$dest/etc/ssl/certs"
	mkdir -p "$dest/proc" "$dest/sys" "$dest/dev" "$dest/run" "$dest/tmp" "$dest/root" \
		"$dest/var/lib/docker" "$dest/etc/docker" "$dest/opt/containerd/bin" "$dest/opt/containerd/lib"
	ln -s usr/lib "$dest/lib"
	ln -s /run "$dest/var/run"
	mkdir -p "$dest/mnt/mac" "$dest/Users" "$dest/Volumes" "$dest/private" "$dest/Applications" \
		"$dest/Library" "$dest/System" "$dest/opt/homebrew" "$dest/usr/local" "$dest/var/folders"
	mkdir -p "$dest/mnt/machines"
	mkdir -p "$dest/bin"
	ln -s bin "$dest/sbin"

	mkdir "$work/docker"
	tar -xzf "$dl/$(file_of docker-engine)" -C "$work/docker" --strip-components=1
	for program in dockerd containerd containerd-shim-runc-v2 runc docker-init docker-proxy; do
		install -m 755 "$work/docker/$program" "$dest/usr/bin/$program"
	done

	install -m 644 "$dl/$(file_of cacert)" "$dest/etc/ssl/certs/ca-certificates.crt"
	mkdir -p "$dest/var/lib/rancher/k3s" "$dest/var/lib/kubelet" "$dest/var/log/pods" "$dest/etc/rancher" "$dest/var/lib/cni"

	mkdir "$work/deb"
	lib=usr/lib/aarch64-linux-gnu
	for name in $(names); do
		case "$(file_of "$name")" in *.deb) ;; *) continue ;; esac
		dpkg-deb -x "$dl/$(file_of "$name")" "$work/deb"
		mkdir -p "$licenses/$name"
		cp "$work/deb/usr/share/doc/$name/copyright" "$licenses/$name/copyright"
	done
	find "$work/deb/$lib" -maxdepth 1 ! -type d -exec cp -a {} "$dest/$lib/" \;
	cp -a "$work/deb/$lib/xtables" "$dest/$lib/xtables"
	ln -s aarch64-linux-gnu/ld-linux-aarch64.so.1 "$dest/usr/lib/ld-linux-aarch64.so.1"
	install -m 755 "$work/deb/usr/sbin/xtables-nft-multi" "$dest/usr/sbin/xtables-nft-multi"
	install -m 755 "$work/deb/usr/sbin/nft" "$dest/usr/sbin/nft"
	install -m 755 "$work/deb/usr/bin/ps" "$dest/usr/bin/ps"
	for link in iptables ip6tables iptables-save ip6tables-save iptables-restore ip6tables-restore; do
		ln -s xtables-nft-multi "$dest/usr/sbin/$link"
	done

	printf '%s\n' 'root:x:0:0:root:/root:/bin/sh' 'dockremap:x:998:998:dockremap:/:/sbin/nologin' \
		'nobody:x:65534:65534:nobody:/:/sbin/nologin' >"$dest/etc/passwd"
	printf '%s\n' 'root:x:0:' 'docker:x:999:' 'dockremap:x:998:' 'nobody:x:65534:' >"$dest/etc/group"
	printf '%s\n' 'dockremap:100000:65536' >"$dest/etc/subuid"
	printf '%s\n' 'dockremap:100000:65536' >"$dest/etc/subgid"
	printf '%s\n' 'NAME="fev"' 'ID=fev' 'PRETTY_NAME="fev"' >"$dest/etc/os-release"
	printf '%s\n' '127.0.0.1 localhost' '::1 localhost' >"$dest/etc/hosts"
	printf '%s\n' 'hosts: files dns' >"$dest/etc/nsswitch.conf"
	ln -s /run/resolv.conf "$dest/etc/resolv.conf"
	rm -rf "$work"
}

k3s_layout() {
	mkdir -p "$1/usr/bin" "$1/usr/lib/k3s/data" "$1/usr/lib/k3s-images"
	for t in k3s kubectl crictl ctr; do
		ln -s "/usr/lib/k3s/data/current/bin/$t" "$1/usr/bin/$t"
	done
}

k8s_pin() {
	awk '$1 == "fetch" && $2 == "k3s" { k3s = sprintf("\"k3s_url\":\"%s\",\"k3s_size\":%s,\"k3s_sha256\":\"%s\",\"payload\":\"%s\"", $8, $6, $5, $7); kv = $3 }
	$1 == "archive" && $2 == "k3s-airgap-images" {
		a = sprintf("{\"version\":\"%s\",\"url\":\"%s\",\"size\":%s,\"sha256\":\"%s\"", $3, $6, $5, $4); av = $3
	}
	END {
		if (a == "" || k3s == "" || kv != av) exit 1
		printf "%s,%s}\n", a, k3s
	}' "$k3slock"
}

case "${1:-}" in
--k3s)
	k3s_layout "$2"
	;;
--k8s-pin)
	k8s_pin
	;;
--fetch)
	fetch "$2"
	;;
--assemble)
	assemble "$2" "$3" "$4"
	;;
"")
	exec "$here/../sysimage/build.sh"
	;;
*)
	usage
	exit 2
	;;
esac
