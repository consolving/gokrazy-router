# Deployment

Add to your gokrazy instance config:

```json
{
  "Packages": [
    "github.com/consolving/gokrazy-router/cmd/gokrazy-router"
  ],
  "PackageConfig": {
    "github.com/consolving/gokrazy-router/cmd/gokrazy-router": {
      "ExtraFilePaths": {
        "/usr/local/bin/hostapd": "hostapd-armv7-static",
        "/usr/local/bin/smbd": "smbd-armv7-static",
        "/usr/local/bin/smbpasswd": "smbpasswd-armv7-static"
      },
      "ExtraFileContents": {
        "/etc/gokrazy-router.json": "<your JSON config here>"
      },
      "Environment": [
        "DISK_DEVICE=/dev/sda1",
        "DISK_TARGET=/mnt/data"
      ]
    }
  }
}
```

The `hostapd` and `smbd` binaries must be statically compiled for ARMv7. Use `build-hostapd.sh` and/or `build-samba.sh` to cross-compile them via Docker. Omit the Samba binaries if you do not enable SMB.

If PXE is enabled, mount a disk and place boot artifacts in the configured TFTP root (default `/mnt/data/tftpboot`). For netboot.xyz:

```bash
mkdir -p /mnt/data/tftpboot
curl -L -o /mnt/data/tftpboot/undionly.kpxe \
  https://boot.ipxe.org/undionly.kpxe
curl -L -o /mnt/data/tftpboot/netboot.xyz.efi \
  https://github.com/netbootxyz/netboot.xyz/releases/latest/download/netboot.xyz.efi
cp netboot/netboot.xyz.ipxe /mnt/data/tftpboot/netboot.xyz.ipxe
cp netboot/boot.ipxe /mnt/data/tftpboot/boot.ipxe
cp netboot/router-extras.toml /mnt/data/router-extras.toml
```

The shipped `netboot/router-extras.toml` sends the T410i (legacy BIOS) through the iPXE chain (`undionly.kpxe` → `boot.ipxe` → netboot.xyz) and defaults every other client to the UEFI bootloader `netboot.xyz.efi` (see [Runtime Extras Config](../runtime-extras.md)). Do not put SMB credentials or other secrets in this file.

See [`netboot/gokrazy-router.json`](../netboot/gokrazy-router.json) for a complete config example.