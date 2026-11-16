# Optional Services

The `services` section enables disk mounting, SMB, PXE/TFTP, NFS and static DHCP reservations.

## Disk mount

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

## SMB share

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

## NFS server

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

## PXE/TFTP server

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

### Booting netboot.xyz

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

## Apple NetBoot (Mac)

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

### Creating a NetInstall image

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

### Creating a PowerPC (G4/G5) NetInstall image

PowerPC Macs use the same BSDP flow with the `AAPLBSDPC/ppc` vendor class. The router already answers them out of the box: `macnbi-ppc/booter` via TFTP, image ID `0x81000009`, and the root-path from `applePPCRootPath`. Only the image artifacts and one config line differ from the Intel flow.

**Target OS** — a G4 tops out at **Mac OS X 10.5.8** (10.6 is Intel-only); 10.4 Tiger works too. All G4s are New World ROM Macs, i.e. they speak BSDP — Old World machines (pre-1999) do not and are out of scope.

**Build tools** — System Image Utility from the matching [Server Admin Tools](https://support.apple.com/kb/DL831): STA 10.5.7 for a 10.5 image, STA 10.4.x for 10.4. Run the matching SIU version on a Mac running that OS; a PowerPC Mac is the safe path. The `.nbi` arch directory (`ppc/`) follows the **installer source**, not the build machine's CPU.

**Image type** — NetInstall (boots the installer from the network, installs to the G4's own disk) or NetRestore (wipes and restores a prepared volume — destructive). The Mac Mini G4 is currently mapped to the available Mac OS X 10.5.4 NetInstall image. Plain diskless NetBoot — the OS runs entirely from the server — is **not** supported until an AFP shadow image is added; track that as a follow-up before switching the G4 back to a NetBoot image.

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
- The PPC DHCP flow sends a valid BSDP SELECT payload on REQUEST; malformed legacy option-43 bytes are replaced rather than echoed. BSDP INFORM replies match the client's advertised protocol version (1.0 or 1.1).
- The 10.5 installer wants ≥512 MB RAM and a DVD-quality network path — NFS over the built-in server is plenty.

### Selecting images per client (per-MAC root paths)

`applePPCRootPath`/`appleIntelRootPath` are per-architecture defaults. To serve **different images of the same architecture to different clients at the same time**, map client MACs to root paths via `[macRootPaths]` (extras TOML) or `services.pxe.macRootPaths` (router.json):

```toml
[macRootPaths]
"00:0d:93:aa:bb:cc" = "nfs:10.0.31.1:/mnt/sata/netboot:MacOSX-10.5.4-NetInstall-ppc.dmg"
"00:0e:xx:yy:zz:zz" = "nfs:10.0.31.1:/mnt/sata/netboot:MacOSX-10.4-Tiger-ppc.dmg"
```

- Precedence: per-MAC entry > architecture default. Entries also apply to non-Apple clients (e.g. an NFS-rooted Linux PXE box) — those get option 17 without any BSDP payload.
- The TFTP boot file can be overridden per client independently via `[macImages]` — useful when different images ship different booters (keep one TFTP directory per image, e.g. `macnbi-ppc-1054/`).
- Extras are re-read on `/api/reload`; per-MAC entries take effect without a restart. Removing all entries falls back to the arch defaults.