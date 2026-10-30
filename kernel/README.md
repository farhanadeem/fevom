# kernel

The guest's Linux kernel: the upstream release fev builds, fev's changes to it and its
configuration, built reproducibly in a pinned container. GPL-2.0, as Linux.

| Path | What |
|---|---|
| `VERSION` | The upstream release, its tarball's checksum and the fixed build timestamp |
| `config/*.config` | Fragments merged onto `allnoconfig`; the build fails if an option a fragment asks for is not in the result |
| `src/` | Whole files fev adds, copied into the tree at the same relative path |
| `patches/*.patch` | Changes to upstream files, applied in order with no fuzz |
| `debug.config` | Lock proving and sleep-in-atomic checks, for `build.sh debug` only |
| `builder/` | The pinned toolchain image and the script that runs in it |
| `out/` | Build output: `Image`, its `config` and `Image.manifest` |

## Build

```sh
kernel/build.sh              # kernel/out/Image, in the builder container on a Linux Docker engine
kernel/build.sh debug        # kernel/out/debug/Image, with the debug options
kernel/build.sh repro        # two clean builds, compared byte for byte
```

The build runs on fev's build engine, or on the Linux Docker engine
`FEV_BUILD_DOCKER_HOST` names (see `BUILDING.md`). `tools/build.sh` builds the kernel with
everything else; `tests/run smoke` boots a built app's kernel end to end.
