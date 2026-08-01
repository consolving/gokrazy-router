# gokrazy-router Specification

## Overview

`gokrazy-router` is a Go daemon for the gokrazy platform that turns a BananaPi R1 (Lamobo R1) into a simple home router. It manages the 5 Ethernet ports exposed by the BCM53125 switch via the Linux DSA (Distributed Switch Architecture) framework.

## Hardware Context

The BPI-R1 has a Broadcom BCM53125 5-port Gigabit switch connected to the Allwinner A20 SoC via a single RGMII interface. The Linux kernel's `b53` DSA driver exposes each physical port as a separate network interface:

- `wan` — WAN uplink port (port 4)
- `lan1` .. `lan4` — LAN ports (ports 0-3)
- `eth0` — the SoC-side conduit/master interface

## Goals

1. **LAN bridge** — Bridge `lan1`-`lan4` into a single `br-lan` interface with a static IP (default `10.0.0.1/24`).
2. **DHCP server** — Serve DHCP leases on `br-lan` to LAN clients.
3. **NAT/masquerade** — Masquerade outbound traffic from `br-lan` via the `wan` interface using nftables.
4. **IP forwarding** — Enable `net.ipv4.ip_forward`.
5. **VLAN support** — Optionally assign VLAN IDs to individual LAN ports so that tagged traffic can be isolated or trunked. Clients can define VLAN-aware bridges or per-port VLANs in the configuration.
6. **WiFi access point** — Run the onboard RTL8192CU WiFi as an access point, bridged into `br-lan` (or a separate VLAN bridge). WiFi AP mode is managed by a bundled statically-compiled `hostapd` binary, supervised as a subprocess.
7. **Configuration file** — All settings are driven by a JSON configuration file. The daemon should work with sensible defaults if no config is provided.
8. **gokrazy integration** — Designed to run as a gokrazy package. Network configuration is done via netlink and nftables Go libraries. `hostapd` is the only required external binary, bundled as an extra file.
9. **Optional services** — Mount a USB/SATA disk, share it via SMB, serve PXE boot images over TFTP, and reserve DHCP addresses by MAC.

## Non-Goals (out of scope for v1)

- IPv6
- Dynamic routing protocols (OSPF, BGP)
- Firewall rules beyond basic NAT
- Web UI
- Netboot image building (users must supply their own boot artifacts)

## Architecture

```
┌──────────────────────────────────────────────────────────────────────────┐
│                           gokrazy-router                                │
│                                                                          │
│  ┌──────────┐ ┌────────┐ ┌────────────┐ ┌───────────────┐ ┌──────────┐  │
│  │ netlink  │ │ DHCP   │ │ nftables   │ │ WiFi (hostapd │ │ Netboot  │  │
│  │ (bridge, │ │ server │ │ (NAT/      │ │  subprocess   │ │ (TFTP +  │  │
│  │  vlan,   │ │ (+PXE  │ │  masq.)    │ │  manager)     │ │  HTTP    │  │
│  │  addrs)  │ │  opts) │ │            │ │               │ │  boot)   │  │
│  └────┬─────┘ └───┬────┘ └─────┬──────┘ └──────┬────────┘ └──┬───┬──┘  │
│       │           │            │               │             │   │      │
└───────┼───────────┼────────────┼───────────────┼─────────────┼───┼──────┘
        │           │            │               │             │   │
   ┌────▼────┐ ┌────▼────┐ ┌────▼─────┐  ┌──────▼──────┐ ┌───▼┐ ┌▼────┐
   │ kernel  │ │ UDP:67  │ │ nf_tables│  │  hostapd    │ │TFTP│ │HTTP │
   │ netlink │ │ on      │ │ kernel   │  │  (RTL8192CU │ │:69 │ │boot │
   │         │ │ br-lan  │ │          │  │   AP mode)  │ │    │ │port │
   └─────────┘ └─────────┘ └──────────┘  └─────────────┘ └────┘ └─────┘
```

### Components

#### 1. Network Setup (`pkg/netsetup`)

Uses netlink (via `github.com/vishvananda/netlink`) to:

- Create `br-lan` bridge interface
- Enslave `lan1`-`lan4` into `br-lan`
- Assign static IP to `br-lan`
- Bring up all interfaces
- Optionally create VLAN sub-interfaces (e.g. `lan1.100`) and assign them to VLAN-aware bridges
- Enable IP forwarding via sysctl

#### 2. DHCP Server (`pkg/dhcp`)

A minimal DHCPv4 server (using `github.com/insomniacslk/dhcp`) that:

- Listens on `br-lan`
- Serves leases from a configurable IP range (default `10.0.0.100`-`10.0.0.250`)
- Provides gateway (`10.0.0.1`), subnet mask, DNS servers
- Supports static MAC-to-IP reservations
- Optionally includes PXE boot options (66/67) per scope
- Maintains a simple lease table in memory (no persistence needed for v1)

#### 3. NAT / Firewall (`pkg/nat`)

Uses nftables (via `github.com/google/nftables`) to:

- Create a `nat` table with a `postrouting` chain
- Add a masquerade rule for traffic from `br-lan` subnet going out `wan`
- Flush and re-apply rules on startup

#### 4. VLAN Manager (`pkg/vlan`)

Handles optional VLAN configuration:

- Create 802.1Q VLAN sub-interfaces on LAN ports
- Support port-based VLANs (isolate ports into separate subnets)
- Support trunk ports (multiple VLANs tagged on a single port)
- Each VLAN can have its own bridge, IP range, and DHCP scope

#### 5. WiFi AP Manager (`pkg/wifi`)

Manages the onboard RTL8192CU WiFi adapter as an access point:

- Generates a `hostapd.conf` from the JSON configuration and writes it to a temp file
- Starts `hostapd` as a supervised subprocess (restart on crash)
- The WiFi interface (`wlan0`) is bridged into `br-lan` by default (or a VLAN bridge if configured)
- `hostapd` binary must be provided as a statically-compiled ARM binary via gokrazy `ExtraFilePaths` (e.g. at `/usr/local/bin/hostapd`)
- Supports configurable SSID, passphrase, channel, HT mode (802.11n), and country code

#### 6. Disk Mount (`pkg/mount`)

Optional block-device mount used by SMB, PXE, and the runtime extras config:

- Waits for the configured device to appear
- Mounts it at a configured target (e.g. `/mnt/data`)
- Supports filesystem type and mount options via `${ENV}` expansion
- Unmounts on shutdown

#### 7. SMB Server (`pkg/smb`)

Optional SMB file share of the mounted disk:

- Generates a minimal `smb.conf` in `/tmp`
- Creates the configured user via `smbpasswd`
- Starts `smbd` as a supervised subprocess
- Credentials and paths support `${ENV}` expansion
- Requires a statically-compiled `smbd`/`smbpasswd` bundled via gokrazy `ExtraFilePaths`

#### 8. PXE/TFTP Server (`pkg/pxe`)

Optional PXE boot server:

- Serves boot images over a built-in UDP TFTP server
- Supports default image and per-MAC image selection
- DHCP scopes automatically advertise PXE options 66/67 when enabled
- Selects the boot file by client type: legacy BIOS, UEFI (option 93), or iPXE (option 77)

#### 9. Configuration (`pkg/config`)

JSON configuration file, loaded at startup:

```json
{
  "wan": {"interface": "wan", "mode": "dhcp"},
  "lan": {"bridge": "br-lan", "interfaces": ["lan1", "lan2", "lan3", "lan4"], "address": "10.0.0.1/24", "dhcp": {"enabled": true, "rangeStart": "10.0.0.100", "rangeEnd": "10.0.0.250", "leaseDuration": "12h", "dns": ["1.1.1.1", "8.8.8.8"]}},
  "vlans": [
    {
      "id": 100, "name": "guest", "ports": ["lan3", "lan4"],
      "address": "10.0.100.1/24",
      "dhcp": {"enabled": true, "rangeStart": "10.0.100.100", "rangeEnd": "10.0.100.250", "dns": ["1.1.1.1"]},
      "nat": true
    },
    {
      "id": 1, "name": "trusted", "ports": ["lan1"],
      "address": "10.0.1.1/24",
      "dhcp": {"enabled": true, "rangeStart": "10.0.1.100", "rangeEnd": "10.0.1.250", "dns": ["1.1.1.1", "8.8.8.8"], "pxeBootFile": "undionly.kpxe"},
      "nat": true
    }
  ],
  "nat": {"enabled": true, "outInterface": "wan"},
  "wifi": {
    "enabled": true, "interface": "wlan0",
    "address": "10.0.200.1/24",
    "dhcp": {"enabled": true, "rangeStart": "10.0.200.100", "rangeEnd": "10.0.200.250", "dns": ["1.1.1.1", "8.8.8.8"]},
    "ssid": "gokrazy", "passphrase": "changeme123",
    "channel": 6, "hwMode": "g", "countryCode": "DE", "wpa": 2
  },
  "services": {
    "mount": {"enabled": true, "device": "/dev/sda1", "fsType": "ext4", "target": "/mnt/data", "options": "defaults,noatime"},
    "pxe": {
      "enabled": true,
      "listen": "0.0.0.0:69",
      "tftpRoot": "/mnt/data/tftpboot",
      "defaultImage": "undionly.kpxe",
      "bootFile": "undionly.kpxe",
      "legacyBootFile": "undionly.kpxe",
      "uefiBootFile": "netboot.xyz.efi",
      "ipxeScript": "boot.ipxe"
    }
  }
}
```

#### 10. Main Entry Point (`cmd/gokrazy-router`)

- Loads configuration
- Mounts optional disk and loads optional extras TOML config
- Runs network setup
- Starts WiFi AP (if enabled) — launches hostapd subprocess
- Starts DHCP server(s) with optional reservations and PXE options
- Starts optional SMB and PXE/TFTP servers
- Installs NAT rules
- Blocks forever (supervised by gokrazy init)
- Cleans up SMB/pxe, WiFi, nftables rules and unmounts disk on SIGTERM

## Dependencies (Go libraries)

| Library | Purpose |
|---------|---------|
| `github.com/vishvananda/netlink` | Bridge, VLAN, interface, address, route management |
| `github.com/google/nftables` | NAT masquerade rules |
| `github.com/insomniacslk/dhcp` | DHCPv4 server |
| `github.com/pelletier/go-toml/v2` | MAC-to-VLAN mapping and runtime extras config parsing |
| `github.com/pin/tftp/v3` | TFTP server for PXE boot |

### External binaries

| Binary | Purpose | Required |
|--------|---------|----------|
| `hostapd` | WiFi AP mode (statically compiled for ARMv7, bundled via gokrazy ExtraFilePaths) | yes |
| `smbd` | SMB server (statically compiled, bundled via gokrazy ExtraFilePaths) | only if SMB enabled |
| `smbpasswd` | User management for SMB | only if SMB enabled |

## Project Structure

```
gokrazy-router/
├── cmd/
│   └── gokrazy-router/
│       └── main.go
├── pkg/
│   ├── config/
│   │   └── config.go
│   ├── netsetup/
│   │   └── netsetup.go
│   ├── dhcp/
│   │   └── dhcp.go
│   ├── nat/
│   │   └── nat.go
│   ├── vlan/
│   │   └── vlan.go
│   ├── wifi/
│   │   └── wifi.go
│   ├── mount/
│   │   └── mount.go
│   ├── smb/
│   │   └── smb.go
│   └── pxe/
│       └── pxe.go
├── spec.md
├── go.mod
└── README.md
```

## Startup Sequence

1. Parse config file (from flag `-config` or gokrazy extra file path `/etc/gokrazy-router.json`)
2. Expand `${ENV}` references in service configuration
3. If disk mount enabled: wait for device, mount it, load optional extras TOML config
4. Create bridge `br-lan`, enslave LAN ports, assign IP, bring up
5. If VLANs configured: create VLAN sub-interfaces and per-VLAN bridges
6. If WiFi enabled: generate `hostapd.conf`, add `wlan0` to bridge, start hostapd subprocess
7. Enable IP forwarding (`/proc/sys/net/ipv4/ip_forward`)
8. Install nftables NAT masquerade rules
9. Start DHCP server(s) on bridge interface(s) with optional reservations and PXE options
10. If enabled: start SMB server and/or PXE/TFTP server
11. Wait for signals; stop services, unmount disk and clean up nftables on SIGTERM

## gokrazy PackageConfig

To deploy, add to the gokrazy instance config:

```json
{
  "Packages": [
    "github.com/consolving/gokrazy-router/cmd/gokrazy-router"
  ],
  "PackageConfig": {
    "github.com/consolving/gokrazy-router/cmd/gokrazy-router": {
      "ExtraFilePaths": {
        "/usr/local/bin/hostapd": "hostapd-armv7-static"
      },
      "ExtraFileContents": {
        "/etc/gokrazy-router.json": "{\"wan\":{\"interface\":\"wan\",\"mode\":\"dhcp\"},\"lan\":{\"bridge\":\"br-lan\",\"interfaces\":[\"lan1\",\"lan2\",\"lan3\",\"lan4\"],\"address\":\"10.0.0.1/24\",\"dhcp\":{\"enabled\":true,\"rangeStart\":\"10.0.0.100\",\"rangeEnd\":\"10.0.0.250\",\"leaseDuration\":\"12h\",\"dns\":[\"1.1.1.1\",\"8.8.8.8\"]}},\"wifi\":{\"enabled\":true,\"interface\":\"wlan0\",\"bridge\":\"br-lan\",\"hostapdBin\":\"/usr/local/bin/hostapd\",\"ssid\":\"gokrazy\",\"passphrase\":\"changeme123\",\"channel\":6,\"hwMode\":\"g\",\"countryCode\":\"DE\",\"wpa\":2},\"nat\":{\"enabled\":true,\"outInterface\":\"wan\"}}"
      }
    }
  }
}
```
