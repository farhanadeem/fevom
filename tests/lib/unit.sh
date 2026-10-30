unit_vmm() { (cd "$root/vmm" && "$cargo" test --quiet -p fevvm -p fileshare); }
unit_proto() { (cd "$root/proto" && "$go" test ./...); }
unit_host() {
	(cd "$root/host" && "$go" test ./...)
}
unit_app() {
	(cd "$root/app/tools/swiftgen" && "$go" test ./...)
	(
		. "$root/app/dev-env.sh"
		cd "$root/app" && swift test ${SWIFT_JOBS:+-j "$SWIFT_JOBS"}
	)
}
unit_guest() {
	(cd "$root/guest" && "$go" test ./...)
	if [ -z "${FEV_NO_LINUX_TESTS:-}" ]; then
		(
			. "$root/tests/lib/buildhost.sh"
			build_host_docker
			image="fev-fixtures:$("$root/tools/dockerfile-id.sh" "$root/tests/fixtures/Dockerfile")"
			docker image inspect "$image" >/dev/null 2>&1 ||
				docker build -q -t "$image" -f "$root/tests/fixtures/Dockerfile" "$root/tests/fixtures" >/dev/null
			(cd "$root/guest" && mkdir -p out &&
				GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go" test -c -o out/init.test ./init)
			status=0
			out=$(docker run --rm -v "$root/guest/out":/out:ro "$image" /out/init.test 2>&1) || status=$?
			echo "$out" | sed 's|^|linux: fev/guest/init |'
			[ "$status" -eq 0 ] || exit "$status"
			"$root/guest/sysimage/build.sh"
			tools=$("$root/guest/sysimage/build.sh" --tools)
			for pkg in internal/reaper internal/thp internal/timerslack internal/binfmt internal/emulation internal/macexec internal/btrfs internal/session internal/copytar internal/streams internal/listen internal/webproxy internal/registry storage machines machines/net cmd/fev-guest cmd/fev-runc sftpserver nfsview debug/attach; do
				name=$(basename "$pkg")
				(cd "$root/guest" &&
					GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$go" test -c -o "out/$name.test" "./$pkg")
				status=0
				out=$(docker run --rm --privileged -e FEV_TOOLS=/tools -v "$root/guest/out":/out:ro -v "$tools":/tools:ro \
					"$image" "/out/$name.test" 2>&1) || status=$?
				echo "$out" | sed "s|^|linux: fev/guest/$pkg |"
				[ "$status" -eq 0 ] || exit "$status"
			done
		)
	else
		echo "skipped: the Linux-only tests of fev/guest (FEV_NO_LINUX_TESTS is set)" >&2
	fi
}
