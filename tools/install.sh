#!/bin/sh
set -eu

usage() {
	cat <<'EOF' >&2
Install fev from this checkout.

  tools/install.sh [--dry-run] [--home DIR] [--apps DIR] [--no-open] [--no-ask] [APP]

  1. checks: an Apple silicon Mac on macOS 26 or later, the pinned
     toolchains and Xcode's Swift, and a Docker engine for the Linux
     builds: FEV_BUILD_DOCKER_HOST if set, else an installed fev's own
     (<home>/run/docker.sock), else whatever `docker` reaches;
  2. tools/build.sh: every layer and the whole bundle;
  3. tools/verify-bundle.sh;
  4. a running fev of another build is stopped; Fevom.app is copied to
     /Applications, replacing one there; the bundle's `fev setup` makes
     the links, PATH, the SSH Include and the Docker CLI plugins;
  5. offers `sudo fev setup --admin` and `fev setup --https`;
  6. opens the app, which starts fev.
  What it did is in <home>/install.record, which tools/uninstall.sh reads.

  APP            install that bundle as built, without building
  --dry-run      print every step and what it would change; change nothing
  --home DIR     a scratch user home for tests: HOME is DIR, fev's home
                 DIR/fev unless FEV_HOME says, the Docker configuration
                 DIR/.docker unless DOCKER_CONFIG says
  --apps DIR     where Fevom.app goes (default /Applications)
  --no-open      do not open the app at the end
  --no-ask       offer nothing; say how to take the admin and HTTPS steps
EOF
}

src=$(cd "$(dirname "$0")/.." && pwd)
build_home=$HOME
dry=
scratch=
apps=
open_app=1
ask=1
app_in=
while [ $# -gt 0 ]; do
	case "$1" in
	--dry-run) dry=1 ;;
	--home)
		scratch=$2
		shift
		;;
	--apps)
		apps=$2
		shift
		;;
	--no-open) open_app= ;;
	--no-ask) ask= ;;
	-*)
		usage
		exit 2
		;;
	*) app_in=$1 ;;
	esac
	shift
done

say() { printf '%s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }
die() {
	printf 'install: %s\n' "$*" >&2
	exit 1
}
run() {
	if [ -n "$dry" ]; then
		say "would run: $*"
	else
		say "+ $*"
		"$@"
	fi
}

if [ -n "$scratch" ]; then
	mkdir -p "$scratch"
	scratch=$(cd "$scratch" && pwd)
	HOME=$scratch
	FEV_HOME=${FEV_HOME:-$scratch/fev}
	DOCKER_CONFIG=${DOCKER_CONFIG:-$scratch/.docker}
	FEV_DOCKER_CONFIG=$DOCKER_CONFIG
	export HOME FEV_HOME DOCKER_CONFIG FEV_DOCKER_CONFIG
	open_app=
	ask=
fi
fev_home=${FEV_HOME:-$HOME/.fev}
apps=${apps:-/Applications}
[ ! -d "$apps" ] || [ -w "$apps" ] || die "$apps is not writable by you: install from an administrator's account, or name another place with --apps DIR"
record=$fev_home/install.record
[ -n "$dry" ] && say "(dry run: nothing is changed)"

step "1. checks"
[ "$(uname -m)" = arm64 ] || die "fev runs on Apple silicon Macs only (this is $(uname -m))"
major=$(sw_vers -productVersion | cut -d. -f1)
[ "$major" -ge 26 ] || die "fev needs macOS 26 or later (this is $(sw_vers -productVersion))"
say "Mac: $(uname -m), macOS $(sw_vers -productVersion)"

app=
if [ -n "$app_in" ]; then
	[ -d "$app_in/Contents" ] || die "$app_in is not an app bundle"
	app=$(cd "$app_in" && pwd)
	say "installing the bundle as built: $app (no build)"
else
	cargo=${CARGO:-$build_home/.cargo/bin/cargo}
	[ -x "$cargo" ] || cargo=$(command -v cargo || true)
	[ -n "$cargo" ] || die "no Rust toolchain (rustup: https://rustup.rs; then rustup default $(sed -n 's/^rust //p' "$src/tools/toolchains"))"
	export CARGO="$cargo"
	command -v "${GO:-go}" >/dev/null || die "no Go toolchain (Go $(sed -n 's/^go //p' "$src/tools/toolchains"))"
	tc() { env HOME="$build_home" "$@"; }
	tc xcrun --find swift >/dev/null 2>&1 || die "no Swift toolchain: install Xcode, then xcode-select -s /Applications/Xcode.app"
	say "toolchains: $(tc "$(dirname "$cargo")/rustc" --version 2>/dev/null | cut -d' ' -f1-2), $(tc "${GO:-go}" env GOVERSION), $(tc xcrun swift --version 2>/dev/null | head -1 | sed 's/.*\(Swift version [0-9.]*\).*/\1/')"
	engine=
	how=
	docker_cli=$(command -v docker || true)
	[ -n "$docker_cli" ] || [ ! -x "$fev_home/bin/docker" ] || docker_cli=$fev_home/bin/docker
	if [ -n "${FEV_BUILD_DOCKER_HOST:-}" ]; then
		[ -n "$docker_cli" ] && DOCKER_HOST=$FEV_BUILD_DOCKER_HOST "$docker_cli" info >/dev/null 2>&1 ||
			die "the engine FEV_BUILD_DOCKER_HOST names ($FEV_BUILD_DOCKER_HOST) does not answer"
		engine=$FEV_BUILD_DOCKER_HOST
		how="named by FEV_BUILD_DOCKER_HOST"
	fi
	if [ -z "$engine" ] && [ -S "$fev_home/run/docker.sock" ] && [ -n "$docker_cli" ] &&
		DOCKER_HOST=unix://$fev_home/run/docker.sock "$docker_cli" info >/dev/null 2>&1; then
		engine=unix://$fev_home/run/docker.sock
		how="the installed fev's own: later builds are built by fev"
	fi
	if [ -z "$engine" ] && [ -n "$docker_cli" ] && "$docker_cli" info >/dev/null 2>&1; then
		engine=$("$docker_cli" context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null || true)
		[ -n "${DOCKER_HOST:-}" ] && engine=$DOCKER_HOST
		how="the one docker reaches"
	fi
	[ -n "$engine" ] || die "no Docker engine answers, and the kernel and the guest images are Linux builds that need one: start one (Docker, or an installed fev), or name one with FEV_BUILD_DOCKER_HOST=unix:///PATH; with none, take the built disk image instead (tools/dmg.sh)"
	[ -n "$docker_cli" ] || die "no docker command for the Linux builds: put one on PATH"
	say "engine: $engine ($how)"
	FEV_BUILD_DOCKER_HOST=$engine
	export FEV_BUILD_DOCKER_HOST

	step "2. build"
	run env HOME="$build_home" "$src/tools/build.sh"
	app=$src/app/out/Fevom.app
fi

step "3. verify"
for_others=--for-others
[ -z "${FEV_SIGN_IDENTITY:-}" ] || for_others=
if [ -n "$dry" ] && { [ -z "$app_in" ] || [ ! -d "$app" ]; }; then
	say "would run: $src/tools/verify-bundle.sh $for_others $app"
else
	"$src/tools/verify-bundle.sh" $for_others "$app" | tail -1 || die "the bundle failed its verification (tools/verify-bundle.sh $for_others $app)"
fi
build=$(plutil -extract FevomBuild raw -o - "$app/Contents/Info.plist" 2>/dev/null || echo "(not built)")
say "build: $build"

step "4. install"
dest=$apps/Fevom.app
new_fev=$app/Contents/MacOS/bin/fev
if [ -S "$fev_home/run/fevd.sock" ] && [ -x "$new_fev" ]; then
	"$new_fev" status >/dev/null 2>&1 && st=0 || st=$?
	if [ "$st" = 4 ]; then
		say "a fev of another build is running: stopping it"
		run "$new_fev" stop
	else
		say "fev of this build is running or stopped: left as it is"
	fi
fi
if [ -z "$scratch" ]; then
	if [ -n "$dry" ]; then
		say "would ask Fevom.app to quit, if it is running"
	elif osascript -e 'application id "dev.fevom.app" is running' 2>/dev/null | grep -q true; then
		say "+ asking Fevom.app to quit before its bundle is replaced (its Quit stops fev; opening the new one starts it)"
		osascript -e 'tell application id "dev.fevom.app" to quit' >/dev/null 2>&1 || true
		_n=0
		while [ "$_n" -lt 120 ]; do
			osascript -e 'application id "dev.fevom.app" is running' 2>/dev/null | grep -q true || break
			sleep 0.5
			_n=$((_n + 1))
		done
		! osascript -e 'application id "dev.fevom.app" is running' 2>/dev/null | grep -q true ||
			die "Fevom.app did not quit (a dialog of its own waiting?): quit it, then run the install again"
	fi
fi
apps_made=0
[ -d "$apps" ] || apps_made=1
replaced=0
[ ! -d "$dest" ] || replaced=1
if [ -n "$dry" ]; then
	say "would copy $app to $dest$([ "$replaced" = 1 ] && echo ", replacing the one there")$([ "$apps_made" = 1 ] && echo " (making $apps)")"
else
	mkdir -p "$apps"
	rm -rf "$apps/.Fevom.app.new" "$apps/.Fevom.app.old"
	ditto "$app" "$apps/.Fevom.app.new"
	[ "$replaced" = 0 ] || mv "$dest" "$apps/.Fevom.app.old"
	mv "$apps/.Fevom.app.new" "$dest"
	rm -rf "$apps/.Fevom.app.old"
	say "+ copied to $dest"
fi
fev=$dest/Contents/MacOS/bin/fev
write_record() {
	[ -n "$dry" ] && return 0
	mkdir -p "$fev_home"
	{
		echo "# tools/install.sh's record of what it did; tools/uninstall.sh reads it"
		echo "app $dest"
		echo "apps_made $apps_made"
		echo "build $build"
		echo "setup 1"
		echo "admin $1"
		echo "https $2"
	} >"$record.tmp"
	mv "$record.tmp" "$record"
}
prev_admin=$(sed -n 's/^admin //p' "$record" 2>/dev/null || echo 0)
prev_https=$(sed -n 's/^https //p' "$record" 2>/dev/null || echo 0)
if [ -f "$record" ] && [ "$(sed -n 's/^apps_made //p' "$record")" = 1 ]; then apps_made=1; fi
write_record "${prev_admin:-0}" "${prev_https:-0}"
run "$fev" setup
if [ -n "$dry" ]; then
	sh_name=$(basename "${SHELL:-/bin/zsh}")
	say "  setup would: link fev, fevctl, docker and docker-compose in $fev_home/bin;"
	say "  add one fenced block putting $fev_home/bin first on PATH at the end of your $sh_name profile;"
	say "  add one fenced Include of fev's SSH entry at the top of $HOME/.ssh/config;"
	say "  link docker compose and docker buildx in ${DOCKER_CONFIG:-$HOME/.docker}/cli-plugins (config.json is not changed);"
	say "  start fev's daemon without a VM, which makes the Docker context \"fev\" and selects it"
	say "  (docker.set_context; the uninstall selects the one before again). All of it is recorded in"
	say "  $fev_home/setup.json, and tools/uninstall.sh (fev setup --undo) takes it back byte for byte."
fi

step "5. the admin helper and HTTPS (optional)"
admin=${prev_admin:-0}
https=${prev_https:-0}
ask_yes() {
	[ -n "$ask" ] && [ -z "$dry" ] && [ -t 0 ] || return 1
	printf '%s [y/N] ' "$1"
	read -r answer </dev/tty || return 1
	case "$answer" in y | Y | yes | YES) return 0 ;; esac
	return 1
}
if ask_yes "Install fev's admin helper now (sudo; direct access to containers' IPs, *.fev.local on this Mac, low ports)?"; then
	sudo "$fev" setup --admin && admin=1
else
	say "not now; later: sudo fev setup --admin"
fi
if ask_yes "Trust fev's root for HTTPS on *.fev.local (one dialog)?"; then
	"$fev" setup --https && https=1
else
	say "not now; later: fev setup --https"
fi
write_record "$admin" "$https"

step "6. open"
if [ -n "$open_app" ]; then
	run open "$dest"
else
	say "not opened; open $dest when you want the app"
fi
say ""
say "installed: $dest, build $build$([ -n "$dry" ] && echo ' (dry run: nothing was changed)')"
say "open a new terminal for fev and docker on PATH; fev uninstall [--delete-data] (or tools/uninstall.sh) removes it"
