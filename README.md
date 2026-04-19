# gokrazy-router

A Go daemon that turns a BananaPi R1 (Lamobo R1) into a home router, designed to run on [gokrazy](https://gokrazy.org).

## Features

- **LAN bridge** — Bridges `lan1`–`lan4` into `br-lan` with a static IP
- **DHCP server** — Serves leases on `br-lan` (and optionally on a separate WiFi subnet)
- **NAT/masquerade** — nftables-based masquerade for outbound traffic via `wan`
- **WiFi access point** — Runs the onboard RTL8192CU as an AP via a bundled `hostapd` binary, with automatic restart on crash (exponential backoff)
- **Per-client traffic monitoring** — nftables counters with live throughput rates, session and historical counters, exposed via an HTTP status API on `:8080`
- **Port speed detection** — Reads negotiated link speed and duplex from sysfs
- **Status CLI** — `gokrazy-router-status` queries the API and prints port/client tables

## Hardware

BananaPi R1 with a Broadcom BCM53125 5-port Gigabit switch. The Linux kernel's `b53` DSA driver exposes each port as a separate interface (`wan`, `lan1`–`lan4`).

## Configuration

All settings are driven by a JSON file (default `/etc/gokrazy-router.json`). If no config is provided, sensible defaults are used.

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
  "nat": {
    "enabled": true,
    "outInterface": "wan"
  },
  "wifi": {
    "enabled": true,
    "interface": "wlan0",
    "address": "10.0.1.1/24",
    "dhcp": {
      "enabled": true,
      "rangeStart": "10.0.1.100",
      "rangeEnd": "10.0.1.250",
      "leaseDuration": "12h",
      "dns": ["1.1.1.1", "8.8.8.8"]
    },
    "ssid": "gokrazy",
    "passphrase": "changeme123",
    "channel": 6,
    "hwMode": "g",
    "countryCode": "DE",
    "wpa": 2
  }
}
```

### WiFi modes

- **Routed** (default): `wlan0` gets its own subnet. A separate DHCP server runs on `wlan0`. Works with all drivers including `rtl8xxxu`.
- **Bridged**: Set `"bridge": "br-lan"` in the wifi config. `wlan0` is added to the LAN bridge. Requires driver support for AP+bridge.

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
        "/usr/local/bin/hostapd": "hostapd-armv7-static"
      },
      "ExtraFileContents": {
        "/etc/gokrazy-router.json": "<your JSON config here>"
      }
    }
  }
}
```

The `hostapd` binary must be statically compiled for ARMv7. Use `build-hostapd.sh` to cross-compile it via Docker.

## Building locally

```bash
# Router daemon
go build ./cmd/gokrazy-router/

# Status CLI
go build ./cmd/gokrazy-router-status/

# Cross-compile hostapd
./build-hostapd.sh
```

## Status API

The daemon serves a JSON status endpoint at `http://<router-ip>:8080/status` with port statistics and per-client traffic counters.

Use the CLI tool to query it:

```bash
gokrazy-router-status --host 10.0.0.1
gokrazy-router-status --host 10.0.0.1 --json
```

Example output:

```
IFACE  MAC                SPEED           RX         TX       RX PKTS  TX PKTS
wan    a2:b4:c6:d8:e0:12  100 Mbps/full   8.2 MiB    1.6 MiB  11764    7190
lan1   a2:b4:c6:d8:e0:12  -               0 B        0 B      0        0
lan2   a2:b4:c6:d8:e0:12  -               0 B        0 B      0        0
lan3   a2:b4:c6:d8:e0:12  -               0 B        0 B      0        0
lan4   a2:b4:c6:d8:e0:12  1000 Mbps/full  190.7 KiB  3.4 MiB  1516     2759
wifi   f0:e1:d2:c3:b4:a5  -               1.2 MiB    4.5 MiB  5443     5879

CONNECTED CLIENTS
VIA  IP          MAC                UL RATE    DL RATE    UL         DL       TOTAL UL   TOTAL DL
L    10.0.0.100  00:11:22:33:44:55  1.3 KiB/s  3.4 KiB/s  156.4 KiB  3.3 MiB  156.4 KiB  3.3 MiB
W    10.0.1.100  66:77:88:99:aa:bb  0 B/s      0 B/s      556.9 KiB  2.1 MiB  556.9 KiB  2.1 MiB
W    10.0.1.101  cc:dd:ee:ff:00:11  0 B/s      0 B/s      601.5 KiB  2.2 MiB  601.5 KiB  2.2 MiB

NOTE: WiFi is in routed mode (separate subnet). LAN and WiFi
      clients can reach each other — no inter-subnet firewall
      rules are configured.
```

The columns show:
- **UL RATE / DL RATE** — Current throughput (sampled every 5 seconds)
- **UL / DL** — Current session traffic (reset on reconnect)
- **TOTAL UL / TOTAL DL** — Accumulated traffic across all sessions since boot
- **SPEED** — Negotiated port link speed and duplex

## Dependencies

| Library | Purpose |
|---------|---------|
| `github.com/vishvananda/netlink` | Bridge, interface, address management |
| `github.com/google/nftables` | NAT masquerade rules + traffic counters |
| `github.com/insomniacslk/dhcp` | DHCPv4 server |

External: `hostapd` (statically compiled, bundled via gokrazy ExtraFilePaths)
