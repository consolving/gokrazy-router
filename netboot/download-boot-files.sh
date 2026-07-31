#!/bin/bash
# Download boot binaries for PXE/TFTP server.
# Usage: ./download-boot-files.sh [tftp_root]
#
# Downloads iPXE binaries, netboot.xyz images, and local boot scripts
# to the TFTP root directory.
set -euo pipefail

TFTP_ROOT="${1:-/tmp/gokrazy-tftpboot}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

mkdir -p "$TFTP_ROOT"

# iPXE UNDI binaries (confirmed available at boot.ipxe.org)
ipxe_files=(
  "undionly.kpxe:https://boot.ipxe.org/undionly.kpxe"
)

# netboot.xyz binaries (GitHub releases, may require internet)
netboot_files=(
  "netboot.xyz.efi:https://github.com/netbootxyz/netboot.xyz/releases/latest/download/netboot.xyz.efi"
  "netboot.xyz.kpxe:https://github.com/netbootxyz/netboot.xyz/releases/latest/download/netboot.xyz.kpxe"
)

echo "=== Downloading iPXE binaries ==="
for entry in "${ipxe_files[@]}"; do
  file="${entry%%:*}"
  url="${entry#*:}"
  target="$TFTP_ROOT/$file"
  if [ -f "$target" ]; then
    echo "skip (exists): $file"
  else
    echo "download: $file"
    curl -fL --retry 3 --retry-delay 2 -o "$target" "$url"
  fi
done

echo ""
echo "=== Downloading netboot.xyz binaries ==="
for entry in "${netboot_files[@]}"; do
  file="${entry%%:*}"
  url="${entry#*:}"
  target="$TFTP_ROOT/$file"
  if [ -f "$target" ]; then
    echo "skip (exists): $file"
  else
    echo "download: $file"
    if ! curl -fL --retry 3 --retry-delay 2 -o "$target" "$url" 2>/dev/null; then
      echo "  WARNING: failed (requires internet connection)"
    fi
  fi
done

# Copy local scripts if not already present
echo ""
echo "=== Copying local scripts ==="
for script in boot.ipxe netboot.xyz.ipxe; do
  src="$SCRIPT_DIR/$script"
  if [ -f "$src" ]; then
    target="$TFTP_ROOT/$script"
    if [ ! -f "$target" ]; then
      echo "copy: $script"
      cp "$src" "$target"
    else
      echo "skip: $script (exists)"
    fi
  fi
done

echo ""
echo "=== Boot files in $TFTP_ROOT ==="
ls -lh "$TFTP_ROOT"
