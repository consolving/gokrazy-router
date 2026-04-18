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
8. **gokrazy integration** — Designed to run as a gokrazy package. Network configuration is done via netlink and nftables Go libraries. `hostapd` is the only external binary, bundled as an extra file.

## Non-Goals (out of scope for v1)

- IPv6
- Dynamic routing protocols (OSPF, BGP)
- Firewall rules beyond basic NAT
- Web UI

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      gokrazy-router                         │
│                                                             │
│  ┌──────────┐ ┌────────┐ ┌────────────┐ ┌───────────────┐  │
│  │ netlink  │ │ DHCP   │ │ nftables   │ │ WiFi (hostapd │  │
│  │ (bridge, │ │ server │ │ (NAT/      │ │  subprocess   │  │
│  │  vlan,   │ │        │ │  masq.)    │ │  manager)     │  │
│  │  addrs)  │ │        │ │            │ │               │  │
│  └────┬─────┘ └───┬────┘ └─────┬──────┘ └──────┬────────┘  │
│       │           │            │               │            │
└───────┼───────────┼────────────┼───────────────┼────────────┘
        │           │            │               │
   ┌────▼────┐ ┌────▼────┐ ┌────▼─────┐  ┌──────▼──────┐
   │ kernel  │ │ UDP:67  │ │ nf_tables│  │  hostapd    │
   │ netlink │ │ on      │ │ kernel   │  │  (RTL8192CU │
   │         │ │ br-lan  │ │          │  │   AP mode)  │
   └─────────┘ └─────────┘ └──────────┘  └─────────────┘
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

#### 6. Configuration (`pkg/config`)

JSON configuration file, loaded at startup:

```json
{
  "wan": {
    "interface": "wan",
    "mode": "dhcp"
  },
  "lan": {
    "bridge": "br-lan",
    "interfaces": ["lan1", "lan2", "lan3", "lan4"],
    "address": "10.0.0.1/24",
    "dhcp": {
      "enabled": true,
      "rangeStart": "10.0.0.100",
      "rangeEnd": "10.0.0.250",
      "leaseDuration": "12h",
      "dns": ["1.1.1.1", "8.8.8.8"]
    }
  },
  "vlans": [
    {
      "id": 100,
      "name": "guest",
      "ports": ["lan3", "lan4"],
      "address": "10.0.100.1/24",
      "dhcp": {
        "enabled": true,
        "rangeStart": "10.0.100.100",
        "rangeEnd": "10.0.100.250",
        "dns": ["1.1.1.1"]
      },
      "nat": true
    }
  ],
  "nat": {
    "enabled": true,
    "outInterface": "wan"
  },
  "wifi": {
    "enabled": true,
    "interface": "wlan0",
    "bridge": "br-lan",
    "hostapdBin": "/usr/local/bin/hostapd",
    "ssid": "gokrazy",
    "passphrase": "changeme123",
    "channel": 6,
    "hwMode": "g",
    "htCapab": "[HT40+][SHORT-GI-20][SHORT-GI-40]",
    "countryCode": "DE",
    "wpa": 2
  }
}
```

#### 7. Main Entry Point (`cmd/gokrazy-router`)

- Loads configuration
- Runs network setup
- Starts WiFi AP (if enabled) — launches hostapd subprocess
- Starts DHCP server(s)
- Installs NAT rules
- Blocks forever (supervised by gokrazy init)
- Cleans up nftables rules and stops hostapd on SIGTERM

## Dependencies (Go libraries)

| Library | Purpose |
|---------|---------|
| `github.com/vishvananda/netlink` | Bridge, VLAN, interface, address, route management |
| `github.com/google/nftables` | NAT masquerade rules |
| `github.com/insomniacslk/dhcp` | DHCPv4 server |

### External binaries

| Binary | Purpose |
|--------|---------|
| `hostapd` | WiFi AP mode (statically compiled for ARMv7, bundled via gokrazy ExtraFilePaths) |

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
│   └── wifi/
│       └── wifi.go
├── spec.md
├── go.mod
└── README.md
```

## Startup Sequence

1. Parse config file (from flag `-config` or gokrazy extra file path `/etc/gokrazy-router.json`)
2. Create bridge `br-lan`, enslave LAN ports, assign IP, bring up
3. If VLANs configured: create VLAN sub-interfaces and per-VLAN bridges
4. If WiFi enabled: generate `hostapd.conf`, add `wlan0` to bridge, start hostapd subprocess
5. Enable IP forwarding (`/proc/sys/net/ipv4/ip_forward`)
6. Install nftables NAT masquerade rules
7. Start DHCP server(s) on bridge interface(s)
8. Wait for signals; stop hostapd and clean up nftables on SIGTERM

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
