FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f

RUN sed -i \
        -e 's|^URIs: http://deb.debian.org/debian$|URIs: http://snapshot.debian.org/archive/debian/20261005T000000Z|' \
        -e 's|^URIs: http://deb.debian.org/debian-security$|URIs: http://snapshot.debian.org/archive/debian-security/20261005T000000Z|' \
        /etc/apt/sources.list.d/debian.sources \
    && grep -c snapshot.debian.org /etc/apt/sources.list.d/debian.sources | grep -qx 4 \
    && apt-get -o Acquire::Check-Valid-Until=false update && apt-get install -y --no-install-recommends \
        bc bison flex make patch xz-utils cpio ca-certificates curl python3 \
        clang lld llvm \
        libelf-dev libssl-dev dwarves \
        busybox-static qemu-system-arm \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /work
