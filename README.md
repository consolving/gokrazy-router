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

### NFS server

Serve the mounted volume via NFSv3. Like SMB, the server is an external binary bundled via ExtraFilePaths: a statically linked `unfsd` (userspace NFS server, [unfs3](https://github.com/unfs3/unfs3)) plus `rpcbind` (portmapper, required for mountd discovery). Build both with `build-nfs.sh` (Alpine armv7 in Docker via qemu).

```json
"nfs": {
  "enabled": true,
  "exportPath": "/mnt/sata/netboot",
  "clients": "10.0.31.0/25"
}
```

Supported fields:

| Field | Purpose |
|-------|---------|
| `unfsdBin` | Path to the `unfsd` binary (default `/usr/local/bin/unfsd`) |
| `rpcbindBin` | Path to the portmapper (default `/usr/local/bin/rpcbind`); set to `"-"` to skip — without a portmapper clients cannot discover the mountd port |
| `exportPath` | Directory to export (default `<mountTarget>/netboot`); must exist on the mounted disk |
| `clients` | Export ACL: host, CIDR or `*` (default) |
| `readWrite` | Export read-write (default: read-only) |
| `rootSquash` | Enable root squashing (default: `no_root_squash`, which Mac NetBoot booters need to read images as root) |
| `extraExports` | Raw exports-file lines appended to the generated entry |
| `exportsFile` | Path for the generated exports file (default `/tmp/nfs-exports`) |
| `nfsPort` / `mountdPort` | Fixed service ports (default 2049 / 20048) |

The exports file is written at startup; changing exports requires a restart. unfs3 speaks NFSv3 over UDP and TCP with fixed ports — sufficient for NetBoot images and file serving; it does not provide NFSv4 or high concurrency.

Appliance requirements: gokrazy ships no `/etc/netconfig`, `/etc/services`, `/etc/passwd` or `/etc/group`, but libtirpc and rpcbind need them. Bundle all four via gokrazy `ExtraFileContents` (see `netboot/gokrazy-router.json` — it is not an example file but the gokrazy instance config in this deployment; `build-nfs.sh` already patches the rpcbind paths from `/var/run` to `/run`, since gokrazy only has a tmpfs there).

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
| `appleNetBoot` | Answer Apple NetBoot (BSDP) clients, see [Apple NetBoot (Mac)](#apple-netboot-mac) |
| `appleIntelBootFile` / `applePPCBootFile` | Option 67 for `AAPLBSDPC/i386` / `AAPLBSDPC/ppc` (default `macnbi-<arch>/booter`) |
| `appleIntelRootPath` / `applePPCRootPath` | Option 17 root-path per arch, e.g. `nfs:192.168.0.10:/export:NetInstall.dmg` |
| `appleImageID` | BSDP boot image ID for option 43 (default `0x81000067`, ppc `0x81000009`) |

DHCP replies automatically include option 66 (TFTP server) and option 67 (boot file) when PXE is enabled. Boot-file precedence is `iPXE (ipxeScript) > per-MAC (macImages) > Apple NetBoot (appleNetBoot, AAPLBSDPC clients only) > uefiBootFile/legacyBootFile > bootFile/defaultImage`. If `pxeBootServer`/`pxeBootFile` are set on a `dhcp` block, those values override the global `services.pxe` defaults for that scope.

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

#### Apple NetBoot (Mac)

Classic Macs can boot over the network via Apple's BSDP protocol (vendor class `AAPLBSDPC`). The router answers these DHCP clients with the BSDP option-43 payload (selected boot image ID), a TFTP booter and an NFS root-path, following the proven [Linux NetBoot recipe](https://kb.pocnet.net/wiki/Mac_OS_X_Maschine_%C3%BCber_Netzwerk_booten). Enable it with `appleNetBoot`:

```json
"services": {
  "nfs": {
    "enabled": true,
    "exportPath": "/mnt/data/netboot",
    "clients": "10.0.31.0/25"
  },
  "pxe": {
    "enabled": true,
    "tftpRoot": "/mnt/data/tftpboot",
    "appleNetBoot": true,
    "appleIntelRootPath": "nfs:10.0.31.1:/mnt/data/netboot:NetInstall-i386.dmg",
    "applePPCRootPath": "nfs:10.0.31.1:/mnt/data/netboot:NetInstall-ppc.dmg"
  }
}
```

Requirements:

1. **TFTP** — the booter must exist under the TFTP root as `macnbi-i386/booter` (Intel) and/or `macnbi-ppc/booter` (PowerPC). Copy the `i386/` and `ppc/` directories of the NBI bundle created with Apple's System Image Utility there:

   ```
   /mnt/data/tftpboot/macnbi-i386/booter
   /mnt/data/tftpboot/macnbi-i386/com.apple.Boot.plist
   /mnt/data/tftpboot/macnbi-i386/x86_64         (kernelcache)
   ```

2. **NFS** — the root-path option (17) points at an NFS server (`nfs:SERVER:/export:IMAGE.dmg`); the exported directory must contain the `.dmg` and be exported read-only. Use the [built-in NFS server](#nfs-server) (`services.nfs` exporting `/mnt/sata/netboot`) or an external one (`async,ro,no_root_squash,insecure`). The NFS server must be reachable from the client's VLAN.

3. **DHCP** — handled automatically: clients identifying as `AAPLBSDPC/i386` get `macnbi-i386/booter`, `AAPLBSDPC/ppc` get `macnbi-ppc/booter`, each with the matching root-path. The option-43 boot image ID defaults to `0x81000067` (i386) / `0x81000009` (ppc) and can be overridden with `appleImageID`. Per-MAC boot files (`macImages`) still take precedence over the Apple classification.

##### Creating a NetInstall image

NetInstall/NetBoot images are produced by Apple's **System Image Utility** (SIU). Availability has been cut back over the years:

| Build Mac runs | Where to get SIU | Notes |
|---|---|---|
| Mac OS X 10.4 – 10.7 | [Server Admin Tools](https://support.apple.com/kb/DL1596) (10.5: DL831, 10.6: DL1403, 10.7: DL1596) | the classic path, used for the [Linux NetBoot recipe](https://kb.pocnet.net/wiki/Mac_OS_X_Maschine_%C3%BCber_Netzwerk_booten) |
| OS X/macOS 10.8 – 10.13 | bundled inside `Server.app` (macOS Server ≤ 5.7) | last versions that can create NetInstall images |
| macOS 10.14 (Mojave) and later | none — Server 5.7.1 removed NetInstall, and the DHCP service GUI went with it | Macs with a T2 chip or Apple silicon cannot NetBoot at all |

The SIU version must match the OS version you want to install: to build a Yosemite NetInstall you need to run the Yosemite-era SIU on a Mac running Yosemite.

Workflow (example: Mac OS X 10.6 NetInstall):

1. Get an install source: the `Install Mac OS X.app`/`InstallESD.dmg` of the target version, or for NetBoot/NetRestore a never-booted system volume (e.g. built with [AutoDMG](https://github.com/MagerValp/AutoDMG)).
2. Open SIU, set the source to the installer/volume, choose **NetInstall Image** (installs to the client's disk), **NetBoot Image** (boots the OS from the server) or **NetRestore Image** (wipes the client disk and restores a prepared volume — destructive!).
3. Work through the wizard (name, admin account, optional packages/profiles, supported models) and save. The result is a `.nbi` bundle:

   ```
   MyImage.nbi/
     NBImageInfo.plist        # metadata (image ID, arch, default flag) — ignored by gokrazy-router
     NetInstall.dmg           # the root image referenced by the root-path option
     i386/
       booter                 # TFTP boot file (DHCP option 67)
       com.apple.Boot.plist   # kernel path + flags, fetched by the booter via TFTP
       x86_64/                # kernelcache / mach.macosx, fetched via TFTP
       PlatformSupport.plist
   ```

4. Copy the bundle to the router (SMB share `\\<router>\sata`):

   ```
   i386/*  → /mnt/sata/tftpboot/macnbi-i386/
   NetInstall.dmg → /mnt/sata/netboot/NetInstall-i386.dmg
   ```

   The `.dmg` name must match the last component of `appleIntelRootPath`; the TFTP and NFS servers serve from disk, so no restart is needed.

5. Boot the client on a scope served by the router (e.g. VLAN 31): power on holding **Option/Alt** and select the network globe. The Mac sends a BSDP DHCPDISCOVER, receives an OFFER with the boot image ID, then a REQUEST/ACK cycle delivers booter path and root-path — the booter loads via TFTP from the router and mounts the `.dmg` via NFS.

The image OS version must be supported by the client's boot ROM; the classic BSDP flow is proven for Mac OS X 10.5/10.6-era Intel Macs (the pocnet recipe) and works for later Intel models that still ship Apple's EFI NetBoot client.

##### Creating a PowerPC (G4/G5) NetInstall image

PowerPC Macs use the same BSDP flow with the `AAPLBSDPC/ppc` vendor class. The router already answers them out of the box: `macnbi-ppc/booter` via TFTP, image ID `0x81000009`, and the root-path from `applePPCRootPath`. Only the image artifacts and one config line differ from the Intel flow.

**Target OS** — a G4 tops out at **Mac OS X 10.5.8** (10.6 is Intel-only); 10.4 Tiger works too. All G4s are New World ROM Macs, i.e. they speak BSDP — Old World machines (pre-1999) do not and are out of scope.

**Build tools** — System Image Utility from the matching [Server Admin Tools](https://support.apple.com/kb/DL831): STA 10.5.7 for a 10.5 image, STA 10.4.x for 10.4. Run the matching SIU version on a Mac running that OS; a PowerPC Mac is the safe path. The `.nbi` arch directory (`ppc/`) follows the **installer source**, not the build machine's CPU.

**Image type** — NetInstall (boots the installer from the network, installs to the G4's own disk) or NetRestore (wipes and restores a prepared volume — destructive). Plain diskless NetBoot — the OS runs entirely from the server — is **not** supported by this setup: it needs an AFP shadow image, which the router does not serve.

**Steps** (example: Mac OS X 10.5 NetInstall for a PowerBook/Mac G4):

1. Source: mount the Mac OS X 10.5 Install DVD (or its `InstallESD.dmg`).
2. SIU (from Server Admin Tools 10.5.7) → source → **NetInstall Image** → wizard (name, admin account, optional packages, supported models — keep the G4 models checked) → save.
3. The bundle is PPC-shaped — no `i386/` or `x86_64/`:

   ```
   MyImage.nbi/
     NBImageInfo.plist        # ignored by gokrazy-router
     NetInstall.dmg           # root image, referenced by applePPCRootPath
     ppc/
       booter                 # bootloader, fetched via TFTP (DHCP option 67)
       mach.macosx            # kernel, fetched by the booter via TFTP
       mach.macosx.mkext      # kernel extension cache, fetched via TFTP
   ```

   The G4's Open Firmware client uses plain 512-byte TFTP blocks (no RFC 2347 options) — the built-in TFTP server handles that unchanged.
4. Copy to the router (SMB share):

   ```
   ppc/*           → /mnt/sata/tftpboot/macnbi-ppc/
   NetInstall.dmg  → /mnt/sata/netboot/NetInstall-ppc.dmg
   ```

5. Point the PPC root-path at it — `router.json` (or the extras TOML, root-level keys before the first table):

   ```json
   "applePPCRootPath": "nfs:10.0.31.1:/mnt/sata/netboot:NetInstall-ppc.dmg"
   ```

   The `.dmg` name must match the last component. If you serve **both** Intel and PPC images, keep two `.dmg` names and set `appleIntelRootPath` and `applePPCRootPath` separately. Leave `appleImageID` unset unless a specific image ID is required — setting it overrides the defaults for **both** architectures.
6. Boot the G4 on a served scope (e.g. VLAN 31): power on holding **Option/Alt**, select the network globe. The G4 sends a `AAPLBSDPC/ppc` DHCPDISCOVER; the ACK carries booter path and root-path, the booter loads `mach.macosx` + `mach.macosx.mkext` via TFTP and mounts the `.dmg` via NFS.

Notes:

- Keep the G4's Boot ROM updated to the last version for the model — early ROM revisions are picky about BSDP replies.
- The PPC DHCP flow uses the fallback option-43 payload on REQUEST (static `00:01:02:…:07` bytes) exactly as the reference recipe does; this is what the recipe validated against 10.5-era clients.
- The 10.5 installer wants ≥512 MB RAM and a DVD-quality network path — NFS over the built-in server is plenty.

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
| `appleNetBoot` | Answer Apple NetBoot (BSDP) clients (see [Apple NetBoot (Mac)](#apple-netboot-mac)); boot files/root-paths per arch, defaults `macnbi-<arch>/booter` |
| `[vlanAddresses]` | `id = "cidr"` address override per VLAN (e.g. for per-VLAN TFTP/DHCP scopes). Applying changes requires a router restart — `/api/reload` rejects them with HTTP 409 |
| `[[smbUsers]]` | Extra SMB users: `name`, `password`. Granted access at startup; adding/removing users requires a router restart — `/api/reload` rejects the change with HTTP 409 |

The PXE boot-file precedence is `iPXE (ipxeScript) > per-MAC (macImages) > Apple NetBoot (appleNetBoot, AAPLBSDPC clients only) > uefiBootFile/legacyBootFile > pxeBootFile/defaultImage`. This is deliberate: once a client runs iPXE, its `ipxeScript` must win over the MAC mapping or the client would never leave the initial boot loader. TFTP images themselves are served directly from disk, so dropping a new file into the TFTP root is enough — no restart needed. Extras-driven reservations and PXE settings require a restart or `/api/reload`.

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
| `github.com/vishvananda/netlink` | Bridge, interface, address management |
| `github.com/google/nftables` | NAT masquerade + isolation rules + traffic counters |
| `github.com/insomniacslk/dhcp` | DHCPv4 server |
| `github.com/pelletier/go-toml/v2` | MAC-to-VLAN mapping and runtime extras config parsing |

External: `hostapd` and optionally `smbd`/`smbpasswd` (statically compiled, bundled via gokrazy ExtraFilePaths)
