# gokrazy-router

A Go daemon that turns a BananaPi R1 (Lamobo R1) into a home router, designed to run on [gokrazy](https://gokrazy.org).

## Features

- **LAN bridge** — Bridges `lan1`–`lan4` into `br-lan` with a static IP
- **DHCP server** — Serves leases on `br-lan` (and optionally on a separate WiFi subnet)
- **NAT/masquerade** — nftables-based masquerade for outbound traffic via `wan`
- **WiFi access point** — Runs the onboard RTL8192CU as an AP via a bundled `hostapd` binary, with automatic restart on crash (exponential backoff)
- **Per-client traffic monitoring** — nftables counters exposed via an HTTP status API on `:8080`
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

## Dependencies

| Library | Purpose |
|---------|---------|
| `github.com/vishvananda/netlink` | Bridge, interface, address management |
| `github.com/google/nftables` | NAT masquerade rules + traffic counters |
| `github.com/insomniacslk/dhcp` | DHCPv4 server |

External: `hostapd` (statically compiled, bundled via gokrazy ExtraFilePaths)
