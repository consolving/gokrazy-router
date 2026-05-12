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
- **Netboot service** — Per-VLAN PXE (TFTP) and HTTP boot server. DHCP automatically injects boot options for legacy PXE, iPXE, and UEFI HTTP Boot clients. Serve kernels, initrds, and boot scripts from a configurable boot directory.
- **Port speed detection** — Reads negotiated link speed and duplex from sysfs
- **Status CLI** — `grcli` queries the API and prints port/client tables, manages netboot images

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

## Netboot

The router can serve as a PXE/HTTP boot server, configurable per-VLAN. When enabled on a VLAN, the DHCP server automatically injects the appropriate boot options based on client type:

- **Legacy PXE** (BIOS) — DHCP options 66/67 point to the TFTP server and boot filename (e.g. `pxelinux.0`)
- **iPXE** — Detected via option 175; receives an HTTP URL to an iPXE script instead of a TFTP path
- **UEFI HTTP Boot** — Detected via client architecture (option 93); receives the HTTP boot URL directly

### Boot artifacts

Place boot files in a per-VLAN directory:

```
/data/netboot/
├── trusted/          # matches VLAN name
│   ├── pxelinux.0
│   ├── ldlinux.c32
│   ├── pxelinux.cfg/
│   │   └── default
│   ├── vmlinuz
│   ├── initrd.img
│   └── boot.ipxe
└── default/          # fallback for VLANs without a dedicated directory
    └── ...
```

### Configuration

Add a `netboot` block to each VLAN that should serve boot images, plus a global `netboot` section:

```json
{
  "vlans": [
    {
      "id": 1, "name": "trusted", "ports": ["lan1"],
      "address": "10.0.1.1/24",
      "dhcp": {"enabled": true, "rangeStart": "10.0.1.100", "rangeEnd": "10.0.1.250", "dns": ["1.1.1.1"]},
      "nat": true,
      "netboot": {
        "enabled": true,
        "tftp": true,
        "http": true,
        "bootDir": "/data/netboot/trusted",
        "defaultBoot": "pxelinux.0",
        "ipxeScript": "boot.ipxe"
      }
    }
  ],
  "netboot": {
    "dir": "/data/netboot",
    "httpPort": ":8069"
  }
}
```

- **TFTP** runs on UDP `:69` (standard PXE port)
- **HTTP boot** runs on the configured `httpPort` (default `:8069`)
- VLANs without `"netboot": {"enabled": true}` are unaffected — their DHCP responses contain no boot options

### MAC-to-IP and netboot image mapping

The MAC mapping file (TOML) supports static IP reservations and per-client netboot image selection:

```toml
default_vlan = 30

[[clients]]
mac = "aa:bb:cc:dd:ee:ff"
vlan = 1
name = "Philipp's laptop"
ip = "10.0.1.10"
netboot_image = "ubuntu-22.04"

[[clients]]
mac = "11:22:33:44:55:66"
vlan = 20
name = "thermostat"
ip = "10.0.20.50"

[[clients]]
mac = "de:ad:be:ef:00:01"
vlan = 1
name = "workstation"
```

- **`ip`** — Optional. Static DHCP reservation. The DHCP server always assigns this IP instead of allocating from the dynamic range.
- **`netboot_image`** — Optional. Subdirectory name within the VLAN's boot directory. Files are served from `<bootDir>/<netboot_image>/` for this client. If omitted, the VLAN's default boot directory is used.

When a client with a `netboot_image` PXE-boots, the DHCP boot-filename points to the image-specific path (e.g. `ubuntu-22.04/pxelinux.0`), so different machines on the same VLAN can boot different OS images.

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

# CLI tool
go build ./cmd/grcli/

# Cross-compile hostapd
./build-hostapd.sh
```

## Status API

The daemon serves a JSON status endpoint at `http://<router-ip>:8080/status` with port statistics and per-client traffic counters.

Use the CLI tool to query it:

```bash
grcli --host 10.0.31.1:8080
grcli --host 10.0.31.1:8080 --json
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

## Netboot Image Management API

The daemon also exposes netboot image management on `:8080`:

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/netboot/images` | List all images |
| `GET` | `/netboot/images/{name}` | List files in an image |
| `POST` | `/netboot/images/{name}` | Upload a tar/tar.gz archive as an image |
| `PUT` | `/netboot/images/{name}/{path...}` | Upload a single file to an image |
| `DELETE` | `/netboot/images/{name}` | Delete an entire image |
| `DELETE` | `/netboot/images/{name}/{path...}` | Delete a single file from an image |

### Managing images with `grcli`

```bash
# List all netboot images
grcli --host 10.0.1.1:8080 netboot list

# Upload a tar.gz archive as a new image
grcli --host 10.0.1.1:8080 netboot upload --name ubuntu-22.04 --file ubuntu-netboot.tar.gz

# Upload/replace a single file in an existing image
grcli --host 10.0.1.1:8080 netboot upload --name ubuntu-22.04 --file vmlinuz --dest vmlinuz

# Delete an entire image
grcli --host 10.0.1.1:8080 netboot delete --name ubuntu-22.04

# Delete a single file from an image
grcli --host 10.0.1.1:8080 netboot delete --name ubuntu-22.04 --path initrd.img
```

## Dependencies

| Library | Purpose |
|---------|---------|
| `github.com/vishvananda/netlink` | Bridge, interface, address management |
| `github.com/google/nftables` | NAT masquerade + isolation rules + traffic counters |
| `github.com/insomniacslk/dhcp` | DHCPv4 server |
| `github.com/pin/tftp/v3` | TFTP server for PXE netboot |
| `github.com/pelletier/go-toml/v2` | MAC mapping file parsing (VLAN, static IP, netboot image) |

External: `hostapd` (statically compiled, bundled via gokrazy ExtraFilePaths)
