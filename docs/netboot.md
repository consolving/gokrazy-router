# NetBoot / NetInstall Reference

This directory contains example files for PXE/TFTP and Apple NetBoot:

- `gokrazy-router.json` — Full router config with PXE enabled
- `router-extras.toml` — Runtime PXE/reservation configuration for the mounted disk
- `macmap.toml` — MAC-to-VLAN mapping with static IPs
- `boot.ipxe` — Example iPXE boot script
- `netboot.xyz.ipxe` — netboot.xyz iPXE chain script
- `pxelinux.cfg.default` — Example pxelinux config for legacy PXE

For complete PXE/TFTP configuration details, see [Services Documentation](../services.md#pxetftp-server).

For Apple NetBoot (BSDP) setup including creating NetInstall images for Intel and PowerPC Macs, see [Apple NetBoot (Mac)](../services.md#apple-netboot-mac).

## Quick start: netboot.xyz

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

Then enable PXE in `services.pxe` and point at the TFTP root.