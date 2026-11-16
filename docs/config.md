# Configuration Reference

All settings are driven by a JSON file (default `/etc/gokrazy-router.json`). If no config is provided, a flat bridge mode with sensible defaults is used.

## Configuration files

The router uses up to four configuration files:

| File | Format | Location on router | Purpose |
|------|--------|-------------------|---------|
| Router config | JSON | `/etc/gokrazy-router.json` | Main config: WAN, LAN, VLANs, NAT, WiFi, PXE |
| Runtime extras | TOML | Path set in `services.extrasFile` (e.g. `/mnt/data/router-extras.toml`) | PXE boot files, per-MAC images, reservations, SMB users |
| MAC map | TOML | Path set in `wifi.macMapFile` (e.g. `/etc/gokrazy-router-macmap.toml`) | MAC-to-VLAN assignment, static IP reservations, per-client PXE images |
| Boot artifacts | Directory | Path set in `services.pxe.tftpRoot` (e.g. `/mnt/data/tftpboot`) | Kernels, initrds, iPXE binaries/scripts, pxelinux configs |

On a gokrazy device, the JSON config and MAC map are deployed via `ExtraFileContents` in the gokrazy instance config. Boot artifacts are placed on persistent storage mounted at `/data/` or another path configured in `services.pxe.tftpRoot`.

Example files are provided in the [`netboot/`](../netboot/) directory:
- `gokrazy-router.json` — Full router config with PXE enabled
- `router-extras.toml` — Runtime PXE/reservation configuration for the mounted disk
- `macmap.toml` — MAC-to-VLAN mapping with static IPs
- `boot.ipxe` — Example iPXE boot script
- `netboot.xyz.ipxe` — netboot.xyz iPXE chain script
- `pxelinux.cfg.default` — Example pxelinux config for legacy PXE

## VLAN mode

```json
{
  "wan": {"interface": "wan", "mode": "dhcp"},
  "lan": {"bridge": "br-lan", "interfaces": [], "address": "10.0.0.1/24", "dhcp": {"enabled": false}},
  "vlans": [
    {
      "id": 1, "name": "trusted", "ports": ["lan1"],
      "address": "10.0.1.1/24",
      "dhcp": {"enabled": true, "rangeStart": "10.0.1.100", "rangeEnd": "10.0.1.250", "leaseDuration": "12h", "dns": ["1.1.1.1", "8.8.8.8"]},
      "nat": true
    },
    {
      "id": 20, "name": "iot", "ports": ["lan2"],
      "address": "10.0.20.1/24",
      "dhcp": {"enabled": true, "rangeStart": "10.0.20.100", "rangeEnd": "10.0.20.250", "leaseDuration": "12h", "dns": ["1.1.1.1", "8.8.8.8"]},
      "nat": true, "isolated": true
    },
    {
      "id": 30, "name": "open", "ports": ["lan3"],
      "address": "10.0.30.1/24",
      "dhcp": {"enabled": true, "rangeStart": "10.0.30.100", "rangeEnd": "10.0.30.250", "leaseDuration": "12h", "dns": ["1.1.1.1", "8.8.8.8"]},
      "nat": true
    },
    {
      "id": 31, "name": "shared", "ports": ["lan4"],
      "address": "10.0.31.129/25",
      "dhcp": {"enabled": true, "rangeStart": "10.0.31.150", "rangeEnd": "10.0.31.250", "leaseDuration": "12h", "dns": ["1.1.1.1", "8.8.8.8"]},
      "nat": true
    }
  ],
  "nat": {"enabled": true, "outInterface": "wan"},
  "wifi": {
    "enabled": true, "interface": "wlan0",
    "address": "10.0.31.1/25",
    "dhcp": {"enabled": true, "rangeStart": "10.0.31.100", "rangeEnd": "10.0.31.126", "leaseDuration": "12h", "dns": ["1.1.1.1", "8.8.8.8"]},
    "ssid": "gokrazy", "passphrase": "${WIFI_PASSPHRASE}",
    "channel": 6, "hwMode": "g", "countryCode": "DE", "wpa": 2
  }
}
```

## Flat mode (no VLANs)

When the `vlans` array is empty or omitted, all LAN ports are bridged into a single `br-lan`:

```json
{
  "wan": {"interface": "wan", "mode": "dhcp"},
  "lan": {
    "bridge": "br-lan",
    "interfaces": ["lan1", "lan2", "lan3", "lan4"],
    "address": "10.0.0.1/24",
    "dhcp": {"enabled": true, "rangeStart": "10.0.0.100", "rangeEnd": "10.0.0.250", "leaseDuration": "12h", "dns": ["1.1.1.1", "8.8.8.8"]}
  },
  "nat": {"enabled": true, "outInterface": "wan"},
  "wifi": {
    "enabled": true, "interface": "wlan0",
    "address": "10.0.1.1/24",
    "dhcp": {"enabled": true, "rangeStart": "10.0.1.100", "rangeEnd": "10.0.1.250", "leaseDuration": "12h", "dns": ["1.1.1.1", "8.8.8.8"]},
    "ssid": "gokrazy", "passphrase": "${WIFI_PASSPHRASE}",
    "channel": 6, "hwMode": "g", "countryCode": "DE", "wpa": 2
  }
}
```

## WiFi modes

- **Routed** (default): `wlan0` gets its own subnet. A separate DHCP server runs on `wlan0`. The RTL8192CU does not support bridged AP mode (data frames are not forwarded), so routed mode is required.
- **Shared subnet with LAN**: Split a /24 into two /25 subnets — one for WiFi, one for a LAN port. The router forwards between them. See VLAN 31 in the example above.