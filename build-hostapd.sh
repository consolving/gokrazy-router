#!/bin/bash
# Build a statically linked hostapd for ARMv7 (armhf) using Alpine in Docker.
# The resulting binary is placed at ./hostapd-arm.
set -e

HOSTAPD_VERSION="2.11"
OUTPUT="$(pwd)/hostapd-arm"

cat > /tmp/Dockerfile.hostapd <<'DOCKERFILE'
FROM alpine:3.21

RUN apk add --no-cache \
    build-base \
    linux-headers \
    libnl3-dev \
    libnl3-static \
    openssl-dev \
    openssl-libs-static \
    wget

ARG HOSTAPD_VERSION=2.11
RUN wget -q https://w1.fi/releases/hostapd-${HOSTAPD_VERSION}.tar.gz \
    && tar xzf hostapd-${HOSTAPD_VERSION}.tar.gz

WORKDIR /hostapd-${HOSTAPD_VERSION}/hostapd

# Minimal config for nl80211 AP mode with WPA2-PSK
RUN cat > .config <<'EOF'
CONFIG_DRIVER_HOSTAP=y
CONFIG_DRIVER_NL80211=y
CONFIG_LIBNL32=y
CONFIG_RSN_PREAUTH=y
CONFIG_IEEE80211N=y
CONFIG_IEEE80211W=y
CONFIG_WPA=y
CONFIG_WPA2=y
CONFIG_PKCS12=y
CONFIG_INTERNAL_LIBTOMMATH=y
CONFIG_INTERNAL_LIBTOMMATH_FAST=y
CONFIG_TLS=openssl
EOF

# Build static binary
RUN make -j$(nproc) LDFLAGS="-static" hostapd

RUN strip hostapd
RUN file hostapd && ls -la hostapd
DOCKERFILE

echo "Building static hostapd ${HOSTAPD_VERSION} for armhf..."
docker build \
    --platform linux/arm/v7 \
    --build-arg HOSTAPD_VERSION="${HOSTAPD_VERSION}" \
    -f /tmp/Dockerfile.hostapd \
    -t hostapd-builder \
    /tmp

CONTAINER=$(docker create --platform linux/arm/v7 hostapd-builder)
docker cp "${CONTAINER}:/hostapd-${HOSTAPD_VERSION}/hostapd/hostapd" "${OUTPUT}"
docker rm "${CONTAINER}"

echo "Built: ${OUTPUT}"
file "${OUTPUT}"
ls -la "${OUTPUT}"
