# gokrazy-router

A Go daemon that turns a BananaPi R1 (Lamobo R1) into a home router, designed to run on [gokrazy](https://gokrazy.org).

## Features

- **VLAN support** — Per-port network isolation using separate bridges (one bridge per VLAN)
- **DHCP server** — Per-VLAN DHCP servers, each with its own address range
- **NAT/masquerade** — nftables-based masquerade for outbound traffic via `wan`
- **Inter-VLAN isolation** — VLANs marked `isolated` are firewalled from all other VLANs (internet-only)
- **WiFi access point** — Runs the onboard RTL8192CU as an AP via a bundled `hostapd` binary, with automatic restart on crash (exponential backoff)
- **WiFi + LAN shared subnet** — WiFi and a LAN port can share a subnet (split into two /25 ranges), with the router forwarding between them
- **Per-client traffic monitoring** — nftables counters with live throughput rates, session and historical counters, exposed via an HTTP status API on the admin listener (loopback `127.0.0.1:8080` by default)
- **PXE/TFTP boot server** — Serve boot images to PXE clients with a single TFTP server. DHCP automatically injects options 66/67 and selects the boot file by client type: legacy BIOS, UEFI, or iPXE (via DHCP option 77). Serve kernels, initrds, and iPXE binaries/scripts from a configurable TFTP root.
- **Apple NetBoot (BSDP)** — Boot classic Macs over the network: DHCP answers `AAPLBSDPC/i386` / `AAPLBSDPC/ppc` clients with the BSDP option-43 payload, a TFTP booter (`macnbi-<arch>/booter`) and an NFS root-path pointing at the NetInstall image
- **NFS server** — Serve the mounted disk via a bundled userspace NFSv3 server (`unfs3` + `rpcbind`), e.g. as the NetBoot image source; exports are generated from `services.nfs`
- **Port speed detection** — Reads negotiated link speed and duplex from sysfs
- **Status CLI** — `router-cli` queries the API and prints port/client tables
- **Disk mount** — Mount a block device via environment variables for use by SMB/PXE/extras config
- **SMB server** — Share the mounted disk via an externally bundled `smbd`, with user/password from environment variables
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

## Documentation

| Topic | File |
|-------|------|
| Configuration (VLAN/flat mode, WiFi) | [docs/config.md](docs/config.md) |
| Optional services (disk, SMB, NFS, PXE, Apple NetBoot) | [docs/services.md](docs/services.md) |
| Runtime extras config (TOML on mounted disk) | [docs/runtime-extras.md](docs/runtime-extras.md) |
| Admin API (status + reload) | [docs/admin-api.md](docs/admin-api.md) |
| Deployment guide | [docs/deployment.md](docs/deployment.md) |
| Status API / CLI reference | [docs/status-api.md](docs/status-api.md) |
| NetBoot / NetInstall examples | [docs/netboot.md](docs/netboot.md) |

## Quick start

1. Build the router daemon:
   ```bash
   GOOS=linux GOARCH=arm GOARM=7 go build ./cmd/gokrazy-router/
   ```

2. Cross-compile `hostapd` (required for WiFi):
   ```bash
   ./build-hostapd.sh
   ```

3. Add to your gokrazy instance config (see [Deployment](docs/deployment.md)):
   ```json
   {
     "Packages": ["github.com/consolving/gokrazy-router/cmd/gokrazy-router"],
     "PackageConfig": {
       "github.com/consolving/gokrazy-router/cmd/gokrazy-router": {
         "ExtraFilePaths": {
           "/usr/local/bin/hostapd": "hostapd-armv7-static"
         },
         "ExtraFileContents": {
           "/etc/gokrazy-router.json": "{...your config...}"
         }
       }
     }
   }
   ```

4. For full config examples, see the [`netboot/`](netboot/) directory and [docs/config.md](docs/config.md).

## Building locally

```bash
# Router daemon (ARMv7)
GOOS=linux GOARCH=arm GOARM=7 go build ./cmd/gokrazy-router/

# Status CLI (ARMv7)
GOOS=linux GOARCH=arm GOARM=7 go build ./cmd/router-cli/

# Router daemon (amd64 for x86 router or VM)
GOOS=linux GOARCH=amd64 go build ./cmd/gokrazy-router/

# Cross-compile hostapd
./build-hostapd.sh
```

Both `linux/arm` (ARMv7) and `linux/amd64` are supported build targets.

## Dependencies

| Library | Purpose |
|---------|---------|
| `github.com/vishvananda/netlink` | Bridge, interface, address management |
| `github.com/google/nftables` | NAT masquerade + isolation rules + traffic counters |
| `github.com/insomniacslk/dhcp` | DHCPv4 server |
| `github.com/pelletier/go-toml/v2` | MAC-to-VLAN mapping and runtime extras config parsing |

External: `hostapd` and optionally `smbd`/`smbpasswd` (statically compiled, bundled via gokrazy ExtraFilePaths)