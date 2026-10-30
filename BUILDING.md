# Building Fevom

## What you need

- An Apple silicon Mac on macOS 26 or later.
- The toolchains `tools/toolchains` pins. `tools/build.sh` refuses any other version, so
  that two builds of one commit are the same:
  - Rust, through [rustup](https://rustup.rs) (`rustup default` the pinned version; the
    scripts call `~/.cargo/bin/cargo`, or `CARGO=` names another);
  - Go (`go` on your `PATH`, or `GO=` names another);
  - Xcode, at `/Applications/Xcode.app` (`DEVELOPER_DIR` names another), for the app's
    Swift and the macOS SDK.
- A Linux Docker engine for the first build. The guest kernel, the x86 translator and the
  guest's system image are built in pinned Linux containers. Once one build has made a
  kernel and a system image, fev runs its own build engine from them
  (`tests/lib/buildhost.sh`, a scratch fev under `/tmp/bb.$UID`). The first build needs
  another engine, named explicitly:
  - an installed fev: `FEV_BUILD_DOCKER_HOST=unix://$HOME/.fev/run/docker.sock`;
  - or any other Docker on the Mac: `FEV_BUILD_DOCKER_HOST=unix:///path/to/docker.sock`.

  `tools/install.sh` finds one by itself: an installed fev's, else whatever `docker`
  reaches.
- The network, for the pinned sources and images the builds fetch. Every one is checked
  against the checksum in `inputs.lock`.

## Build

```sh
FEV_BUILD_DOCKER_HOST=unix:///path/to/docker.sock tools/build.sh   # the first time
tools/build.sh                                                       # after that
```

`tools/build.sh` builds whatever is stale, in one pass, under one build identifier stamped
into every program:

| Output | What |
|---|---|
| `vmm/target/release/fevvm` | The VMM, signed ad hoc with the hypervisor entitlement |
| `kernel/out/Image` | The guest kernel and its manifest |
| `guest/out/` | The init image, the system image (the Docker engine, btrfs-progs, the translator) |
| `host/out/` | `fevd`, `fev`, the admin helper and the development tools |
| `app/out/Fevom.app` | The whole app bundle |

`tools/build.sh --check` builds nothing and checks the toolchains, `inputs.lock` and that
everything built carries this tree's identifier. `tools/verify-bundle.sh app/out/Fevom.app`
checks the bundle (its layout, signatures, the kernel's manifest, the pinned client tools,
the licences).

Each layer also builds alone:

```sh
cd vmm && cargo build --release && ../tools/sign-dev.sh target/release/fevvm
kernel/build.sh
guest/build.sh && guest/sysimage/build.sh
host/build.sh
app/build.sh                 # a debug app (dev.fevom.app.debug); --release, --bundle
```

## Check a release

A release is built from its tag in this repository and nothing else, and a build of the
same tag gives the same app. Each release has `Fevom-vX.Y.Z.sha256` beside its disk image:
the SHA-256 of every file in its `Fevom.app`, with code signatures taken out (the release is
signed, your build is not). To check a release against its source:

```sh
git clone --branch vX.Y.Z https://github.com/farhanadeem/fevom.git && cd fevom
tools/build.sh                 # until tools/build.sh --check passes (see below)
tools/bundle-hashes.sh --check Fevom-vX.Y.Z.sha256 app/out/Fevom.app
```

It needs the toolchains `tools/toolchains` pins and a clean clone: an edited file changes
the build identifier, which every program carries. On a first build, the system image is
made on the engine `FEV_BUILD_DOCKER_HOST` names and does not yet record runc's answers
under fev's own kernel; `tools/build.sh --check` says so, and running `tools/build.sh`
again on fev's own build engine finishes it. `tools/bundle-hashes.sh --check
Fevom-vX.Y.Z.sha256 Fevom-vX.Y.Z.dmg` checks a downloaded disk image against the same list.

The app is compiled at `/tmp/fev-app-release` on every Mac, so that no path of yours is in
it; two release builds at once take turns there.

## Install

```sh
tools/install.sh             # build, verify, copy to /Applications, set up PATH, open the app
tools/install.sh --dry-run   # what it would do
fev uninstall                # take it all back; --delete-data deletes your data too (tools/uninstall.sh runs it)
```

## Run a fev of your own while developing

A fev with a home of its own shares nothing with your installed one: its own sockets,
settings, logs and data image, and none of the Mac's files unless told.

```sh
host/out/fev --home /tmp/bh.dev start
docker -H unix:///tmp/bh.dev/run/docker.sock run --rm alpine echo hi
host/out/fev --home /tmp/bh.dev stop
```

Keep such homes directly under `/tmp`: macOS limits a Unix socket's path to 103 bytes.

## Test

```sh
tests/run unit               # every layer's unit tests: vmm, proto, guest, host, app
tests/run unit host          # one layer
tests/run smoke              # a built app end to end: a scratch fev runs a container
```

The guest's Linux-only unit tests run in a container on the build engine;
`FEV_NO_LINUX_TESTS=1` leaves them out. The smoke test needs the network for its image.

The VMM's guest-facing parsers have libFuzzer targets in `vmm/fuzz/fuzz_targets`. They need
nightly Rust and [cargo-fuzz](https://github.com/rust-fuzz/cargo-fuzz)
(`rustup toolchain install nightly`, `cargo install cargo-fuzz`):

```sh
cd vmm/fuzz && cargo +nightly fuzz run blk -- -max_total_time=60
```
