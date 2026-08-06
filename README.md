# gokrazy-router

A Go daemon that turns a BananaPi R1 (Lamobo R1) into a home router, designed to run on [gokrazy](https://gokrazy.org).

## Features

- **VLAN support** — Per-port network isolation using separate bridges (one bridge per VLAN)
- **DHCP server** — Per-VLAN DHCP servers, each with its own address range
- **NAT/masquerade** — nftables-based masquerade for outbound traffic via `wan`
- **IPv6 (SLAAC/DHCPv6/NAT66)** — Per-scope Router Advertisements (SLAAC), stateful/stateless DHCPv6, IPv6 forwarding, NAT66 masquerade and dual-stack per-client traffic counters
- **Inter-VLAN isolation** — VLANs marked `isolated` are firewalled from all other VLANs (internet-only)
- **WiFi access point** — Runs the onboard RTL8192CU as an AP via a bundled `hostapd` binary, with automatic restart on crash (exponential backoff)
- **WiFi + LAN shared subnet** — WiFi and a LAN port can share a subnet (split into two /25 ranges), with the router forwarding between them
- **Per-client traffic monitoring** — nftables counters with live throughput rates, session and historical counters, exposed via an HTTP status API on the admin listener (loopback `127.0.0.1:8080` by default)
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
    "ssid": "gokrazy", "passphrase": "${WIFI_PASSPHRASE}",
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
    "ssid": "gokrazy", "passphrase": "${WIFI_PASSPHRASE}",
    "channel": 6, "hwMode": "g", "countryCode": "DE", "wpa": 2
  }
}
```

### WiFi modes

- **Routed** (default): `wlan0` gets its own subnet. A separate DHCP server runs on `wlan0`. The RTL8192CU does not support bridged AP mode (data frames are not forwarded), so routed mode is required.
- **Shared subnet with LAN**: Split a /24 into two /25 subnets — one for WiFi, one for a LAN port. The router forwards between them. See VLAN 31 in the example above.

### IPv6

IPv6 support is optional and opt-in per scope. The router acts as an IPv6 gateway on every interface that has an `address6` set: it assigns itself the address, enables IPv6 forwarding, and — when `ra` is enabled — sends Router Advertisements (SLAAC) so clients autoconfigure. A built-in RA server is used (no external `radvd`), so no extra binaries are needed on gokrazy.

Key settings:

| Field | Scope | Purpose |
|-------|-------|---------|
| `address6` | `wan`, `lan`, `vlans[]`, `wifi` | Router IPv6 address on the interface, CIDR (e.g. `fd00::1/64`). When set on a LAN scope it also defines the prefix advertised to clients |
| `ra` | `lan`, `vlans[]`, `wifi` | Send Router Advertisements (SLAAC). `true` required for any client autoconfiguration |
| `dhcp6` | `lan`, `vlans[]`, `wifi` | Run a DHCPv6 server on the scope (stateful IA_NA assignment + stateless DNS). Sets the RA M/O flags so clients use it |
| `dns6` | global, `lan`, `vlans[]`, `wifi` | IPv6 DNS servers. Announced via RDNSS in RAs and DHCPv6 option 23. Per-scope value falls back to the global `dns6` |
| `mode6` | `wan` | `auto` (SLAAC, accept RAs from upstream — default), `static` (uses `address6` + `gateway6`), `disabled` (IPv6 off on WAN) |
| `enabled6` | `nat` | NAT66 masquerade for non-routed prefixes (e.g. ULA). When off, global-prefix IPv6 is routed as-is (clients must have upstream global addresses) |

Example flat-mode IPv6:

```json
{
  "lan": {
    "address": "10.0.0.1/24",
    "address6": "fd00::1/64",
    "ra": true,
    "dhcp6": true,
    "dns6": ["2606:4700:4700::1111", "2001:4860:4860::8888"]
  },
  "nat": {"enabled": true, "enabled6": true, "outInterface": "wan"},
  "wan": {"interface": "wan", "mode": "dhcp", "mode6": "auto"}
}
```

Client addressing model:

- **SLAAC only** — `address6` + `ra: true`. Clients pick addresses from the prefix (e.g. `fd00::/64`). The RA includes the prefix and the RDNSS option (from `dns6`).
- **Stateful DHCPv6** — add `dhcp6: true`. The RA sets the Managed (M) flag and clients request IA_NA leases; the DHCPv6 server hands out the first free addresses after the router's own. The server also answers Information Requests, so stateless-only clients still get DNS.
- **Stateless DHCPv6** — RA with the M flag off and `dhcp6: true`: clients keep SLAAC addresses but pull DNS from DHCPv6 option 23.

The WAN can be set up three ways:

- `mode6: "auto"` (default) — the WAN interface accepts Router Advertisements from the ISP (`accept_ra=2`), which is required because IPv6 forwarding is on. Clients behind NAT66 get global addresses from the router's own IPv6.
- `mode6: "static"` — the router uses `address6` and installs a default route via `gateway6`.
- `mode6: "disabled"` — IPv6 is turned off on the WAN interface.

Per-client traffic counters are dual-stack: SLAAC/DHCPv6 client addresses are discovered from the kernel neighbor table and counted alongside IPv4 in the status API (`ip6` field on each client entry). Clients are matched by MAC, so a dual-stack device appears as one entry with both `ip` and `ip6`.

For the example VLAN topology above, each VLAN would add `address6` (e.g. `fd00:1::1/64`), `ra: true` and optionally `dhcp6: true`; the WiFi routed subnet gets its own `address6` on `wlan0`.

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

Serve boot images to PXE clients. The TFTP server listens on UDP/69. Its root directory defaults to `<mountTarget>/tftpboot`. The TFTP root is a strict jail: requests for absolute paths, `..` traversal or symlinks that resolve outside the root are rejected with a TFTP error, and every path component is opened with no-follow semantics so a symlink swapped in between validation and open cannot escape the root — a client cannot read arbitrary files from the router. Each transfer runs on its own ephemeral UDP port (standard TFTP transfer ID), which keeps concurrent clients isolated.

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
| `bindInterface` | Optional `SO_BINDTODEVICE` interface (e.g. `br-vlan31`). Leave empty to serve every scope that advertises option 66/67 — replies are routed out the correct bridge automatically |

DHCP replies automatically include option 66 (TFTP server) and option 67 (boot file) when PXE is enabled. Boot-file precedence is `iPXE (ipxeScript) > per-MAC (macImages) > uefiBootFile/legacyBootFile > bootFile/defaultImage`. If `pxeBootServer`/`pxeBootFile` are set on a `dhcp` block, those values override the global `services.pxe` defaults for that scope.

#### Booting netboot.xyz

To load the public [netboot.xyz](https://netboot.xyz) menu on legacy BIOS clients:

1. Download `netboot.xyz.kpxe` into the TFTP root (e.g. `/mnt/data/tftpboot/netboot.xyz.kpxe`). This is the iPXE UNDI binary.
2. Create a real iPXE script named `netboot.xyz.ipxe` that chainloads the netboot.xyz menu. Use `menu.ipxe` (the `ipxe/netboot.xyz.ipxe` path returns 404) and try HTTP first, because the legacy `undionly.kpxe` build has no HTTPS support:

   ```ipxe
   #!ipxe
   chain --autofree http://boot.netboot.xyz/menu.ipxe ||
   chain --autofree https://boot.netboot.xyz/menu.ipxe ||
   shell
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

You can place a TOML file on the mounted disk and edit it with a text editor. The file is read on every gokrazy restart. If the file does not exist yet, a minimal initial version is created automatically from the JSON config (reservations, PXE images and VLAN addresses are seeded from `router.json`), so runtime editing always has a starting point. To apply edits without a reboot, `POST /api/reload` on the admin API (default `127.0.0.1:8080`, see [Admin API](#admin-api)).

```json
"services": {
  "extrasFile": "/mnt/data/router-extras.toml"
}
```

Supported keys:

| Key | Purpose |
|-----|---------|
| `[reservations]` | `"mac" = "ip"` fixed leases, merged into every DHCP scope (VLANs, LAN, WiFi) |
| `[macImages]` | `"mac" = "file"` per-MAC TFTP image (DHCP boot-file precedence: iPXE > per-MAC > UEFI/legacy) |
| `defaultImage` | TFTP fallback for per-MAC requests that have no `macImages` entry |
| `pxeBootFile` | DHCP option 67 boot file (applied to all subnets) |
| `legacyBootFile` | Option 67 for legacy BIOS PXE clients |
| `uefiBootFile` | Option 67 for UEFI PXE clients |
| `ipxeScript` | Option 67 once a client identifies itself as iPXE (option 77) |
| `[vlanAddresses]` | `id = "cidr"` address override per VLAN (e.g. for per-VLAN TFTP/DHCP scopes). Applying changes requires a router restart — `/api/reload` rejects them with HTTP 409 |
| `[vlanAddresses6]` | `id = "cidr"` IPv6 address override per VLAN. Changes require a router restart — `/api/reload` rejects them with HTTP 409 |
| `[dns6]` | Global IPv6 DNS override applied to every RA/DHCPv6 scope (LAN, VLANs, WiFi) |
| `[[smbUsers]]` | Extra SMB users: `name`, `password`. Granted access at startup; adding/removing users requires a router restart — `/api/reload` rejects the change with HTTP 409 |

The PXE boot-file precedence is `iPXE (ipxeScript) > per-MAC (macImages) > uefiBootFile/legacyBootFile > pxeBootFile/defaultImage`. This is deliberate: once a client runs iPXE, its `ipxeScript` must win over the MAC mapping or the client would never leave the initial boot loader. TFTP images themselves are served directly from disk, so dropping a new file into the TFTP root is enough — no restart needed. Extras-driven reservations and PXE settings require a restart or `/api/reload`.

### Admin API

The HTTP admin API (default `127.0.0.1:8080`) serves the JSON status endpoint at `/status` and the reload endpoint at `/api/reload`. Both share one listener, so the status page is no longer exposed on all interfaces by default — set `services.adminAPI.listen` to `:8080` to restore the old behavior, or to another address/port to move the API elsewhere.

```json
"services": {
  "adminAPI": {
    "listen": "127.0.0.1:8080",
    "token": "CHANGE_ME",
    "allowUnauthenticatedReload": false
  }
}
```

- `listen` — bind address for the status + admin API (default `127.0.0.1:8080`).
- `token` — bearer token required for `POST /api/reload`. Any header is compared in constant time; missing or wrong token yields `401 Unauthorized`.
- `allowUnauthenticatedReload` — set `true` to allow reloads without a token. Only do this on a trusted network, and prefer a `token` plus SSH or a host firewall to reach it.

`POST /api/reload` re-reads the extras file and applies reservations and PXE settings live, then answers `200 ok`. Changes that require a full restart — `[vlanAddresses]` edits or adding/removing `[[smbUsers]]` — are rejected with `409 Conflict` rather than silently half-applied; restart the router to pick those up. The admin listener is unauthenticated for `/status` by design; it binds to loopback so remote clients cannot read it. Always set a `token` (or keep the listener loopback-only) if the router is reachable from untrusted networks.

The extras file replaces (does not merge with) the base `services.pxe` overrides while it is present: `macImages`, `defaultImage`, `pxeBootFile`, `legacyBootFile`, `uefiBootFile` and `ipxeScript` from the JSON config are ignored once an extras file is in place. Values removed from the TOML file are removed at runtime as well — set `defaultImage = ""` to clear it. When the extras file does not exist yet, it is auto-created and seeded from `router.json`, so nothing is lost until the file is edited.

Example covering all interfaces (VLAN 1, 20, 31 and WiFi), a per-MAC PXE image and an extra SMB user:

```toml
[vlanAddresses]
1  = "10.0.1.1/24"
20 = "10.0.20.1/24"
31 = "10.0.31.1/24"

[reservations]
"5c:ff:35:0d:b3:49" = "10.0.31.150"   # T410i on VLAN 31
"aa:bb:cc:dd:ee:01" = "10.0.1.10"     # workstation on VLAN 1
"aa:bb:cc:dd:ee:02" = "10.0.20.20"    # NAS on VLAN 20
"cc:dd:ee:ff:00:11" = "10.0.31.100"   # WiFi client

[macImages]
"5c:ff:35:0d:b3:49" = "undionly.kpxe"      # T410i: legacy iPXE UNDI
"aa:bb:cc:dd:ee:01" = "ipxe-workstation.efi" # UEFI workstation

pxeBootFile = "netboot.xyz.efi"       # default for UEFI clients
legacyBootFile = "netboot.xyz.efi"    # legacy clients default here too (only MAC-mapped clients get a working legacy chain)
uefiBootFile = "netboot.xyz.efi"
ipxeScript = "boot.ipxe"               # iPXE clients get a script

[[smbUsers]]
name = "alice"
password = "hunter2"
```

A startup log line prints all active services, including extras-driven reservations and PXE images. Putting PXE boot files here is recommended because they change often and do not require a gokrazy redeploy. Do not put SMB credentials or other secrets in the JSON config; `smbUsers` belongs in `router-extras.toml` on the mounted disk.

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
curl -L -o /mnt/data/tftpboot/undionly.kpxe \
  https://boot.ipxe.org/undionly.kpxe
curl -L -o /mnt/data/tftpboot/netboot.xyz.efi \
  https://github.com/netbootxyz/netboot.xyz/releases/latest/download/netboot.xyz.efi
cp netboot/netboot.xyz.ipxe /mnt/data/tftpboot/netboot.xyz.ipxe
cp netboot/boot.ipxe /mnt/data/tftpboot/boot.ipxe
cp netboot/router-extras.toml /mnt/data/router-extras.toml
```

The shipped `netboot/router-extras.toml` sends the T410i (legacy BIOS) through the iPXE chain (`undionly.kpxe` → `boot.ipxe` → netboot.xyz) and defaults every other client to the UEFI bootloader `netboot.xyz.efi` (see [Runtime extras config](#runtime-extras-config)). Do not put SMB credentials or other secrets in this file.

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

The daemon serves a JSON status endpoint at `http://127.0.0.1:8080/status` (on the router itself) with port statistics and per-client traffic counters. Configure `services.adminAPI.listen` to expose it on another interface.

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
| `github.com/vishvananda/netlink` | Bridge, interface, address, neighbor and route management |
| `github.com/google/nftables` | NAT/NAT66 masquerade + isolation rules + traffic counters |
| `github.com/insomniacslk/dhcp` | DHCPv4 + DHCPv6 servers |
| `github.com/mdlayher/ndp` | Router Advertisement (SLAAC) server |
| `github.com/pelletier/go-toml/v2` | MAC-to-VLAN mapping and runtime extras config parsing |

External: `hostapd` and optionally `smbd`/`smbpasswd` (statically compiled, bundled via gokrazy ExtraFilePaths)
