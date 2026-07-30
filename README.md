# gokrazy-router

A Go daemon that turns a BananaPi R1 (Lamobo R1) into a home router, designed to run on [gokrazy](https://gokrazy.org).

## Features

- **VLAN support** — Per-port network isolation using separate bridges (one bridge per VLAN)
- **DHCP server** — Per-VLAN DHCP servers, each with its own address range
- **NAT/masquerade** — nftables-based masquerade for outbound traffic via `wan`
- **Inter-VLAN isolation** — VLANs marked `isolated` are firewalled from all other VLANs (internet-only)
- **WiFi access point** — Runs the onboard RTL8192CU as an AP via a bundled `hostapd` binary, with automatic restart on crash (exponential backoff)
- **WiFi + LAN shared subnet** — WiFi and a LAN port can share a subnet (split into two /25 ranges), with the router forwarding between them
- **Per-client traffic monitoring** — nftables counters with live throughput rates, session and historical counters, exposed via an HTTP status API on `:8080`
- **PXE/TFTP boot server** — Serve boot images to PXE clients with a single TFTP server. DHCP automatically injects options 66/67 and selects the boot file by client type: legacy BIOS, UEFI, or iPXE (via DHCP option 77). Serve kernels, initrds, and iPXE binaries/scripts from a configurable TFTP root.
- **Port speed detection** — Reads negotiated link speed and duplex from sysfs
- **Status CLI** — `router-cli` queries the API and prints port/client tables
- **Disk mount** — Mount a block device via environment variables for use by SMB/PXE/extras config
- **SMB server** — Share the mounted disk via an externally bundled `smbd`, with user/password from environment variables
- **PXE/TFTP server** — Serve boot images to PXE clients, with per-MAC image selection and a default fallback
- **Static DHCP reservations** — Pin a MAC address to a fixed IP address
- **Runtime extras config** — Place an editable TOML file on the mounted disk; changes take effect on the next gokrazy restart

## Hardware

BananaPi R1 with a Broadcom BCM53125 5-port Gigabit switch. The Linux kernel's `b53` DSA driver exposes each port as a separate interface (`wan`, `lan1`–`lan4`). The onboard WiFi is a Realtek RTL8192CU (USB, soldered on-board).

## Network Layout

```
              ┌─────────────────────────────────────┐
   Internet ──┤ wan (DHCP)                          │
              │                                     │
              │ VLAN 1  (trusted)                   │
              │   lan1 ─── br-vlan1  10.0.1.1/24    │
              │                                     │
              │ VLAN 20 (iot, isolated)              │
              │   lan2 ─── br-vlan20 10.0.20.1/24   │
              │                                     │
              │ VLAN 30 (open)                       │
              │   lan3 ─── br-vlan30 10.0.30.1/24   │
              │                                     │
              │ VLAN 31 (shared with WiFi)           │
              │   lan4 ─── br-vlan31 10.0.31.129/25 │
              │   wlan0 ────────────  10.0.31.1/25   │
              └─────────────────────────────────────┘
```

- **lan4 and WiFi share the 10.0.31.0/24 range**, split into two /25 subnets. WiFi clients get 10.0.31.100-126, lan4 clients get 10.0.31.150-250. The router forwards between them.
- **VLAN 20 is isolated** — devices on lan2 can reach the internet but cannot communicate with any other VLAN.
- **No 802.1Q tags on the wire** — each port is on its own bridge. The VLAN numbering is logical, used for bridge naming and isolation rules. The BCM53125 DSA driver does not support the Linux bridge VLAN filtering API.

## Configuration

All settings are driven by a JSON file (default `/etc/gokrazy-router.json`). If no config is provided, a flat bridge mode with sensible defaults is used.

### Configuration files

The router uses up to four configuration files:

| File | Format | Location on router | Purpose |
|------|--------|-------------------|---------|
| Router config | JSON | `/etc/gokrazy-router.json` | Main config: WAN, LAN, VLANs, NAT, WiFi, PXE |
| Runtime extras | TOML | Path set in `services.extrasFile` (e.g. `/mnt/data/router-extras.toml`) | PXE boot files, per-MAC images, reservations, SMB users |
| MAC map | TOML | Path set in `wifi.macMapFile` (e.g. `/etc/gokrazy-router-macmap.toml`) | MAC-to-VLAN assignment, static IP reservations, per-client PXE images |
| Boot artifacts | Directory | Path set in `services.pxe.tftpRoot` (e.g. `/mnt/data/tftpboot`) | Kernels, initrds, iPXE binaries/scripts, pxelinux configs |

On a gokrazy device, the JSON config and MAC map are deployed via `ExtraFileContents` in the gokrazy instance config. Boot artifacts (kernels, initrds, iPXE binaries, scripts) are placed on persistent storage mounted at `/data/` or another path configured in `services.pxe.tftpRoot`.

Example files are provided in the [`netboot/`](netboot/) directory:
- `gokrazy-router.json` — Full router config with PXE enabled
- `router-extras.toml` — Runtime PXE/reservation configuration for the mounted disk
- `macmap.toml` — MAC-to-VLAN mapping with static IPs
- `boot.ipxe` — Example iPXE boot script
- `netboot.xyz.ipxe` — netboot.xyz iPXE chain script
- `pxelinux.cfg.default` — Example pxelinux config for legacy PXE

### VLAN mode

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
    "ssid": "gokrazy", "passphrase": "changeme123",
    "channel": 6, "hwMode": "g", "countryCode": "DE", "wpa": 2
  }
}
```

### Flat mode (no VLANs)

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
    "ssid": "gokrazy", "passphrase": "changeme123",
    "channel": 6, "hwMode": "g", "countryCode": "DE", "wpa": 2
  }
}
```

### WiFi modes

- **Routed** (default): `wlan0` gets its own subnet. A separate DHCP server runs on `wlan0`. The RTL8192CU does not support bridged AP mode (data frames are not forwarded), so routed mode is required.
- **Shared subnet with LAN**: Split a /24 into two /25 subnets — one for WiFi, one for a LAN port. The router forwards between them. See VLAN 31 in the example above.

## Optional services

The `services` section enables disk mounting, SMB, PXE/TFTP and static DHCP reservations.

### Disk mount

Mount a block device before the optional services start. Values support `${ENV}` expansion.

```json
"services": {
  "mount": {
    "enabled": true,
    "device": "${DISK_DEVICE}",
    "fsType": "ext4",
    "target": "${DISK_TARGET}",
    "options": "${DISK_OPTS}"
  }
}
```

Set the environment variables in your `config.json` `PackageConfig`:

```json
"CommandLineFlags": ["-config=/etc/gokrazy-router.json"],
"Environment": [
  "DISK_DEVICE=/dev/sda1",
  "DISK_TARGET=/mnt/data",
  "DISK_OPTS=defaults,noatime"
]
```

### SMB share

Share the mounted volume via SMB. The credentials use `${ENV}` expansion so they are not stored in the JSON config or in `router-extras.toml`.

```json
"smb": {
  "enabled": true,
  "binPath": "/usr/local/bin/smbd",
  "listen": "0.0.0.0:445",
  "shareName": "data",
  "sharePath": "/mnt/data",
  "user": "${SMB_USER}",
  "password": "${SMB_PASSWORD}"
}
```

A statically linked `smbd` (and `smbpasswd`) must be bundled via ExtraFilePaths, just like `hostapd`.

### PXE/TFTP server

Serve boot images to PXE clients. The TFTP server listens on UDP/69. Its root directory defaults to `<mountTarget>/tftpboot`.

For a working iPXE chain, place the iPXE UNDI binary (e.g. `undionly.kpxe`) and a real iPXE text script named `boot.ipxe` into the TFTP root. The DHCP server sends `undionly.kpxe` to the initial PXE ROM and `boot.ipxe` once iPXE identifies itself via DHCP option 77 (User Class). If both names point to the same file, iPXE will reload itself in a loop.

```json
"services": {
  "pxe": {
    "enabled": true,
    "listen": "0.0.0.0:69",
    "tftpRoot": "/mnt/data/tftpboot",
    "defaultImage": "undionly.kpxe",
    "bootFile": "undionly.kpxe",
    "legacyBootFile": "undionly.kpxe",
    "uefiBootFile": "netboot.xyz.efi",
    "ipxeScript": "boot.ipxe",
    "macImages": {
      "aa:bb:cc:dd:ee:ff": "ipxe-workstation.efi"
    }
  }
}
```

Supported fields:

| Field | Purpose |
|-------|---------|
| `defaultImage` | File served by TFTP when no per-MAC mapping matches |
| `bootFile` | Default DHCP option 67 boot file |
| `legacyBootFile` | Option 67 for legacy BIOS PXE clients |
| `uefiBootFile` | Option 67 for UEFI PXE clients |
| `ipxeScript` | Option 67 once a client identifies itself as iPXE |

DHCP replies automatically include option 66 (TFTP server) and option 67 (boot file) when PXE is enabled. If `pxeBootServer`/`pxeBootFile` are set on a `dhcp` block, those values override the global `services.pxe` defaults for that scope.

#### Booting netboot.xyz

To load the public [netboot.xyz](https://netboot.xyz) menu on legacy BIOS clients:

1. Download `netboot.xyz.kpxe` into the TFTP root (e.g. `/mnt/data/tftpboot/netboot.xyz.kpxe`). This is the iPXE UNDI binary.
2. Create a real iPXE script named `netboot.xyz.ipxe` that chainloads the netboot.xyz menu over HTTPS:

   ```ipxe
   #!ipxe
   chain --autofree https://boot.netboot.xyz/ipxe/netboot.xyz.ipxe ||
   chain --autofree http://boot.netboot.xyz/ipxe/netboot.xyz.ipxe
   ```

3. Configure the router to hand out those files:

   ```json
   "services": {
     "pxe": {
       "enabled": true,
       "tftpRoot": "/mnt/data/tftpboot",
       "defaultImage": "netboot.xyz.kpxe",
       "bootFile": "netboot.xyz.kpxe",
       "legacyBootFile": "netboot.xyz.kpxe",
       "uefiBootFile": "netboot.xyz.efi",
       "ipxeScript": "netboot.xyz.ipxe"
     }
   }
   ```

Legacy PXE ROM receives `netboot.xyz.kpxe` (the iPXE binary), iPXE starts and then receives `netboot.xyz.ipxe` (the script) via DHCP option 77, and the script loads the live netboot.xyz menu.

For UEFI clients, provide `netboot.xyz.efi` as `uefiBootFile`.

### Static DHCP reservations

Reserve fixed IPs by MAC address in any `dhcp` block:

```json
"dhcp": {
  "enabled": true,
  "rangeStart": "10.0.1.100",
  "rangeEnd": "10.0.1.250",
  "reservations": {
    "aa:bb:cc:dd:ee:ff": "10.0.1.10",
    "11:22:33:44:55:66": "10.0.1.11"
  }
}
```

### Runtime extras config

You can place a TOML file on the mounted disk and edit it with a text editor. The file is re-read on every gokrazy restart, so changes take effect when you restart the router service from the web UI.

Example `/mnt/data/router-extras.toml`:

```toml
[reservations]
"aa:bb:cc:dd:ee:ff" = "10.0.1.10"
"11:22:33:44:55:66" = "10.0.1.11"

[macImages]
"aa:bb:cc:dd:ee:ff" = "netboot.xyz.kpxe"

defaultImage = "netboot.xyz.kpxe"
pxeBootFile = "netboot.xyz.kpxe"
legacyBootFile = "netboot.xyz.kpxe"
uefiBootFile = "netboot.xyz.efi"
ipxeScript = "netboot.xyz.ipxe"
```

Point the JSON config at it:

```json
"services": {
  "extrasFile": "/mnt/data/router-extras.toml"
}
```

A startup log line prints all active services, including extras-driven reservations and PXE images. Putting PXE boot files here is recommended because they change often and do not require a gokrazy redeploy.

## Deployment

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
curl -L -o /mnt/data/tftpboot/netboot.xyz.kpxe \
  https://github.com/netbootxyz/netboot.xyz/releases/latest/download/netboot.xyz.kpxe
curl -L -o /mnt/data/tftpboot/netboot.xyz.efi \
  https://github.com/netbootxyz/netboot.xyz/releases/latest/download/netboot.xyz.efi
cp netboot/netboot.xyz.ipxe /mnt/data/tftpboot/netboot.xyz.ipxe
cp netboot/router-extras.toml /mnt/data/router-extras.toml
```

The runtime extras file selects `netboot.xyz.kpxe` for legacy clients and `netboot.xyz.ipxe` for iPXE clients (see [PXE/TFTP server](#pxetftp-server)). Do not put SMB credentials or other secrets in this file.

See [`netboot/gokrazy-router.json`](netboot/gokrazy-router.json) for a complete config example.

## Building locally

```bash
# Router daemon
go build ./cmd/gokrazy-router/

# Status CLI
go build ./cmd/router-cli/

# Cross-compile for BananaPi R1 (ARMv7)
GOOS=linux GOARCH=arm GOARM=7 go build ./cmd/gokrazy-router/
GOOS=linux GOARCH=arm GOARM=7 go build ./cmd/router-cli/

# Build for amd64 (e.g. x86 router or VM)
GOOS=linux GOARCH=amd64 go build ./cmd/gokrazy-router/
GOOS=linux GOARCH=amd64 go build ./cmd/router-cli/

# Cross-compile hostapd
./build-hostapd.sh
```

Both `linux/arm` (ARMv7) and `linux/amd64` are supported build targets.

## Status API

The daemon serves a JSON status endpoint at `http://<router-ip>:8080/status` with port statistics and per-client traffic counters.

Use the CLI tool to query it:

```bash
router-cli --host 10.0.31.1
router-cli --host 10.0.31.1 --json
```

Example output:

```
IFACE  MAC                SPEED           RX         TX       RX PKTS  TX PKTS
wan    a2:b4:c6:d8:e0:12  100 Mbps/full   8.2 MiB    1.6 MiB  11764    7190
lan                        -               0 B        0 B      0        0
wifi   f0:e1:d2:c3:b4:a5  -               1.2 MiB    4.5 MiB  5443     5879

CONNECTED CLIENTS
VIA  IP            MAC                UL RATE    DL RATE    LINK  SIGNAL   UL         DL       TOTAL UL   TOTAL DL
V1   10.0.1.100    00:11:22:33:44:55  1.3 KiB/s  3.4 KiB/s  -     -        156.4 KiB  3.3 MiB  156.4 KiB  3.3 MiB
V31  10.0.31.150   66:77:88:99:aa:bb  0 B/s      0 B/s      -     -        90.2 KiB   1.1 MiB  90.2 KiB   1.1 MiB
W    10.0.31.100   cc:dd:ee:ff:00:11  0 B/s      0 B/s      -     -42 dBm  601.5 KiB  2.2 MiB  601.5 KiB  2.2 MiB
```

The columns show:
- **VIA** — `V<id>` for VLAN clients, `W` for WiFi, `L` for flat-mode LAN
- **UL RATE / DL RATE** — Current throughput (sampled every 5 seconds)
- **LINK** — WiFi link rate (from hostapd control socket)
- **SIGNAL** — WiFi signal strength in dBm (from hostapd control socket)
- **UL / DL** — Current session traffic (reset on reconnect)
- **TOTAL UL / TOTAL DL** — Accumulated traffic across all sessions since boot
- **SPEED** — Negotiated port link speed and duplex

## Dependencies

| Library | Purpose |
|---------|---------|
| `github.com/vishvananda/netlink` | Bridge, interface, address management |
| `github.com/google/nftables` | NAT masquerade + isolation rules + traffic counters |
| `github.com/insomniacslk/dhcp` | DHCPv4 server |
| `github.com/pelletier/go-toml/v2` | MAC-to-VLAN mapping and runtime extras config parsing |
| `github.com/pin/tftp/v3` | TFTP server for PXE boot |

External: `hostapd` and optionally `smbd`/`smbpasswd` (statically compiled, bundled via gokrazy ExtraFilePaths)
