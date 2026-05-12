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
7. **Netboot service** — PXE (TFTP) and HTTP boot server, configurable per-VLAN. Extends the DHCP server with boot options (next-server, boot-filename) so clients on enabled VLANs can network-boot. Serves boot artifacts via an embedded TFTP server and the HTTP boot endpoint.
8. **Configuration file** — All settings are driven by a JSON configuration file. The daemon should work with sensible defaults if no config is provided.
9. **gokrazy integration** — Designed to run as a gokrazy package. Network configuration is done via netlink and nftables Go libraries. `hostapd` is the only external binary, bundled as an extra file.

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

#### 6. Netboot Service (`pkg/netboot`)

Provides PXE and HTTP network boot, configurable per-VLAN:

**TFTP server** (using `github.com/pin/tftp/v3`):

- Listens on UDP port 69, bound to the router's IP on each netboot-enabled VLAN/bridge
- Serves boot artifacts (pxelinux.0, kernels, initrds, iPXE scripts) from a configurable boot directory
- Read-only; no write support needed

**HTTP boot server**:

- Serves the same boot directory over HTTP on a configurable port (default `:8069`)
- Supports iPXE chainloading and UEFI HTTP Boot
- Serves per-VLAN boot directories, routed by request source IP

**DHCP PXE option injection**:

- When netboot is enabled for a VLAN, the existing DHCP server adds:
  - Option 66 (`next-server`) — router's IP on that VLAN
  - Option 67 (`boot-filename`) — e.g. `pxelinux.0` or an iPXE script URL
  - Option 60 (`vendor-class-identifier`) detection to distinguish legacy PXE, iPXE, and UEFI HTTP Boot clients
  - Option 93 (`client-system-architecture`) to detect BIOS vs UEFI
- For iPXE clients (identified by option 175), respond with an HTTP URL pointing to the HTTP boot endpoint instead of a TFTP path
- For UEFI HTTP Boot clients (arch type 0x0010), respond with the HTTP boot URL directly

**Boot directory layout**:

```
/data/netboot/
├── vlan1/
│   ├── pxelinux.0
│   ├── ldlinux.c32
│   ├── pxelinux.cfg/
│   │   └── default
│   ├── vmlinuz
│   ├── initrd.img
│   └── boot.ipxe
├── vlan20/
│   └── ...
└── default/          # fallback if no VLAN-specific dir exists
    └── ...
```

- Each VLAN can have its own boot directory, or fall back to a `default/` directory
- The boot directory root is configurable (default `/data/netboot/`)

**Per-VLAN configuration**:

Each VLAN's `netboot` block controls:
- `enabled` — whether netboot is active on this VLAN
- `tftp` — enable/disable TFTP serving (default true when netboot enabled)
- `http` — enable/disable HTTP boot serving (default true when netboot enabled)
- `bootDir` — path to boot artifacts for this VLAN (default `<netbootDir>/<vlanName>/`)
- `defaultBoot` — filename sent as DHCP option 67 for legacy PXE clients (default `pxelinux.0`)
- `ipxeScript` — URL or filename sent to iPXE clients (default `boot.ipxe`)
- `httpPort` — HTTP boot server port (default `:8069`, shared across VLANs)

**MAC-to-IP and netboot image mapping**:

The existing MAC mapping file (TOML) is extended with optional `ip` and `netboot_image` fields per client:

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
hostname = "tado-bridge"
ip = "10.0.20.50"

[[clients]]
mac = "de:ad:be:ef:00:01"
vlan = 1
name = "workstation"
```

- **`ip`** (optional) — Static DHCP reservation. When set, the DHCP server always assigns this IP to the client instead of allocating from the dynamic range. The IP must fall within the VLAN's subnet but may be outside the DHCP dynamic range.
- **`netboot_image`** (optional) — Subdirectory name within the VLAN's boot directory. When set, the TFTP and HTTP boot servers serve files from `<bootDir>/<netboot_image>/` instead of the VLAN's default boot directory. The DHCP boot-filename option is adjusted to point to the image-specific path. If the subdirectory does not exist, the VLAN's default boot directory is used as fallback.

The DHCP server integrates with the MAC map as follows:
1. On DISCOVER/REQUEST, look up the client's MAC in the map
2. If an `ip` field is present, return that IP (skip dynamic allocation)
3. If netboot is enabled on the client's VLAN and `netboot_image` is set, adjust the boot-filename DHCP option to the image-specific path
4. Static IPs are excluded from the dynamic allocation pool to prevent conflicts

#### 7. Netboot Image Management API

The router daemon exposes netboot image management endpoints on the existing `:8080` HTTP server. The `grcli` tool provides a CLI interface to these endpoints.

**Endpoints**:

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/netboot/images` | List all images (directories under `netboot.dir`) |
| `GET` | `/netboot/images/{name}` | List files in a specific image |
| `POST` | `/netboot/images/{name}` | Upload a tar archive, extracted to `<dir>/<name>/` |
| `PUT` | `/netboot/images/{name}/{path...}` | Upload a single file to `<dir>/<name>/<path>` |
| `DELETE` | `/netboot/images/{name}` | Delete an entire image directory |
| `DELETE` | `/netboot/images/{name}/{path...}` | Delete a single file from an image |

**Response format** (JSON):

- `GET /netboot/images` returns `{"images": [{"name": "ubuntu-22.04", "files": 5, "size": 123456789}]}`
- `GET /netboot/images/{name}` returns `{"name": "ubuntu-22.04", "files": [{"path": "vmlinuz", "size": 12345678}, ...]}`
- `POST` accepts `Content-Type: application/x-tar` or `application/gzip` (tar.gz). Returns `{"name": "ubuntu-22.04", "files": 5, "size": 123456789}`
- `PUT` accepts any `Content-Type`. Returns `{"path": "ubuntu-22.04/vmlinuz", "size": 12345678}`
- `DELETE` returns `204 No Content` on success

**Safety**:

- Path traversal is rejected (no `..` components)
- Image names must match `[a-zA-Z0-9._-]+`
- Maximum upload size is configurable (default 2 GiB)

#### 8. CLI Tool (`cmd/grcli`)

The `grcli` command-line tool queries the router's HTTP API for status, MAC export, and netboot image management. It replaces the previous `gokrazy-router-status` tool.

**Usage**:

```
grcli [flags] [command]

Commands:
  status              Show port and client status (default)
  netboot list        List netboot images
  netboot upload      Upload a netboot image (tar archive or single file)
  netboot delete      Delete a netboot image or file

Flags:
  --host string       Router API address (default "10.0.0.1:8080")
  --json              Output raw JSON

Status flags:
  --export-toml       Export known MACs as TOML mac-vlan-map
  --merge string      Merge new MACs into existing TOML file

Netboot upload flags:
  --name string       Image name (required)
  --file string       Path to tar/tar.gz archive or single file
  --dest string       Destination path within image (for single file upload)

Netboot delete flags:
  --name string       Image name (required)
  --path string       File path within image (omit to delete entire image)
```

**Examples**:

```bash
# Show router status
grcli --host 10.0.1.1:8080

# List netboot images
grcli --host 10.0.1.1:8080 netboot list

# Upload a tar archive as a netboot image
grcli --host 10.0.1.1:8080 netboot upload --name ubuntu-22.04 --file ubuntu-netboot.tar.gz

# Upload a single kernel to an existing image
grcli --host 10.0.1.1:8080 netboot upload --name ubuntu-22.04 --file vmlinuz --dest vmlinuz

# Delete an image
grcli --host 10.0.1.1:8080 netboot delete --name ubuntu-22.04

# Delete a single file from an image
grcli --host 10.0.1.1:8080 netboot delete --name ubuntu-22.04 --path initrd.img
```

#### 9. Configuration (`pkg/config`)

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
    },
    {
      "id": 1,
      "name": "trusted",
      "ports": ["lan1"],
      "address": "10.0.1.1/24",
      "dhcp": {
        "enabled": true,
        "rangeStart": "10.0.1.100",
        "rangeEnd": "10.0.1.250",
        "dns": ["1.1.1.1", "8.8.8.8"]
      },
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
  "nat": {
    "enabled": true,
    "outInterface": "wan"
  },
  "netboot": {
    "dir": "/data/netboot",
    "httpPort": ":8069"
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

#### 10. Main Entry Point (`cmd/gokrazy-router`)

- Loads configuration
- Runs network setup
- Starts WiFi AP (if enabled) — launches hostapd subprocess
- Starts DHCP server(s) — with PXE options on netboot-enabled VLANs
- Starts netboot TFTP and HTTP servers (if any VLAN has netboot enabled)
- Installs NAT rules
- Blocks forever (supervised by gokrazy init)
- Cleans up nftables rules and stops hostapd on SIGTERM

## Dependencies (Go libraries)

| Library | Purpose |
|---------|---------|
| `github.com/vishvananda/netlink` | Bridge, VLAN, interface, address, route management |
| `github.com/google/nftables` | NAT masquerade rules |
| `github.com/insomniacslk/dhcp` | DHCPv4 server |
| `github.com/pin/tftp/v3` | TFTP server for PXE netboot |
| `github.com/pelletier/go-toml/v2` | MAC-to-VLAN mapping file parsing |

### External binaries

| Binary | Purpose |
|--------|---------|
| `hostapd` | WiFi AP mode (statically compiled for ARMv7, bundled via gokrazy ExtraFilePaths) |

## Project Structure

```
gokrazy-router/
├── cmd/
│   ├── gokrazy-router/
│   │   └── main.go
│   └── grcli/
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
│   └── netboot/
│       ├── netboot.go
│       ├── tftp.go
│       └── http.go
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
7. Start DHCP server(s) on bridge interface(s) — inject PXE boot options on netboot-enabled VLANs
8. If netboot enabled on any VLAN: start TFTP server (UDP :69) and HTTP boot server (TCP :8069)
9. Wait for signals; stop hostapd and clean up nftables on SIGTERM

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
