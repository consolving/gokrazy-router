#!/bin/bash
# Build statically linked unfs3 (userspace NFSv3 server) and rpcbind
# (portmapper) for ARMv7 (armhf) using Alpine in Docker (qemu emulation,
# same approach as build-hostapd.sh).
# The resulting binaries are placed at ./unfsd-arm and ./rpcbind-arm.
#
# unfs3 speaks NFSv3 over UDP+TCP, which is what classic Mac NetBoot
# booters (BSDP root-path) and standard mount(8) clients use.
set -e

UNFS3_VERSION="0.11.0"
RPCBIND_VERSION="1.2.7"
OUTDIR="$(pwd)"

cat > /tmp/Dockerfile.nfs <<'DOCKERFILE'
FROM alpine:3.21

RUN apk add --no-cache \
    build-base \
    flex \
    bison \
    bash \
    pkgconf \
    wget \
    file \
    autoconf \
    automake \
    linux-headers

ARG UNFS3_VERSION=0.11.0
ARG RPCBIND_VERSION=1.2.7

# --- libtirpc (static, GSS disabled to avoid libgssglue/krb5 link deps) ---
# musl lacks <sys/queue.h>; libtirpc needs it. Ship a BSD one plus a minimal
# <sys/cdefs.h> stub for the macros the queue header uses.
RUN mkdir -p /usr/include/sys \
    && wget -q https://raw.githubusercontent.com/freebsd/freebsd-src/release/14.2.0/sys/sys/queue.h -O /usr/include/sys/queue.h \
    && printf '%s\n' \
      '#ifndef _SYS_CDEFS_H_' \
      '#define _SYS_CDEFS_H_' \
      '#define __BEGIN_DECLS' \
      '#define __END_DECLS' \
      '#define __unused __attribute__((__unused__))' \
      '#define __packed __attribute__((__packed__))' \
      '#define __aligned(x) __attribute__((__aligned__(x)))' \
      '#define __section(x) __attribute__((__section__(x)))' \
      '#endif' > /usr/include/sys/cdefs.h

RUN wget -q https://downloads.sourceforge.net/sourceforge/libtirpc/libtirpc-1.3.5.tar.bz2 \
    && tar xjf libtirpc-1.3.5.tar.bz2

# gokrazy has a tmpfs on /run and no /var/run; point the rpcbind socket path
# at /run before installing the header that rpcbind consumes.
WORKDIR /libtirpc-1.3.5
RUN sed -i 's|"/var/run/rpcbind.sock"|"/run/rpcbind.sock"|' tirpc/rpc/rpcb_prot.h \
    && ./configure --disable-gssapi --enable-static --disable-shared \
    && make -j$(nproc) \
    && make install

# --- unfs3 ---
WORKDIR /
RUN wget -q https://github.com/unfs3/unfs3/archive/refs/tags/unfs3-${UNFS3_VERSION}.tar.gz \
    && tar xzf unfs3-${UNFS3_VERSION}.tar.gz

WORKDIR /unfs3-unfs3-${UNFS3_VERSION}
RUN bash ./bootstrap \
    && bash ./configure CPPFLAGS="-I/usr/include/tirpc" LDFLAGS="-static" LIBS="-ltirpc" \
    && make -j$(nproc) \
    && strip unfsd \
    && file unfsd && ls -la unfsd

# --- rpcbind ---
# rpcbind hardcodes /var/run/rpcbind.lock; gokrazy only has a tmpfs on /run.
WORKDIR /
RUN wget -q https://sourceforge.net/projects/rpcbind/files/rpcbind/${RPCBIND_VERSION}/rpcbind-${RPCBIND_VERSION}.tar.bz2/download -O rpcbind-${RPCBIND_VERSION}.tar.bz2 \
    && tar xjf rpcbind-${RPCBIND_VERSION}.tar.bz2 \
    && sed -i 's|"/var/run/rpcbind.lock"|"/run/rpcbind.lock"|' rpcbind-${RPCBIND_VERSION}/src/rpcbind.c

WORKDIR /rpcbind-${RPCBIND_VERSION}
# --with-rpcbind-user=root avoids a getpwnam("rpc") dependency on the appliance.
# --with-statedir=/run/rpcbind: gokrazy has a tmpfs on /run and no /var/run.
RUN ./configure --with-systemdsystemunitdir=no --with-rpcbind-user=root --with-rpcbind-group=root \
    --with-statedir=/run/rpcbind \
    CPPFLAGS="-I/usr/include/tirpc" LDFLAGS="-static" LIBS="-ltirpc" \
    && make -j$(nproc) \
    && strip rpcbind \
    && file rpcbind && ls -la rpcbind
DOCKERFILE

echo "Building static unfs3 ${UNFS3_VERSION} + rpcbind ${RPCBIND_VERSION} for armhf..."
docker build \
    --platform linux/arm/v7 \
    --build-arg UNFS3_VERSION="${UNFS3_VERSION}" \
    --build-arg RPCBIND_VERSION="${RPCBIND_VERSION}" \
    -f /tmp/Dockerfile.nfs \
    -t nfs-builder \
    /tmp

CONTAINER=$(docker create --platform linux/arm/v7 nfs-builder)
docker cp "${CONTAINER}:/unfs3-unfs3-${UNFS3_VERSION}/unfsd" "${OUTDIR}/unfsd-arm"
docker cp "${CONTAINER}:/rpcbind-${RPCBIND_VERSION}/rpcbind" "${OUTDIR}/rpcbind-arm"
docker rm "${CONTAINER}"

echo "Built: ${OUTDIR}/unfsd-arm ${OUTDIR}/rpcbind-arm"
file "${OUTDIR}/unfsd-arm" "${OUTDIR}/rpcbind-arm"
ls -la "${OUTDIR}/unfsd-arm" "${OUTDIR}/rpcbind-arm"
