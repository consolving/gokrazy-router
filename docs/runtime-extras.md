# Runtime Extras Config

You can place a TOML file on the mounted disk and edit it with a text editor. The file is read on every gokrazy restart. If the file does not exist yet, a minimal initial version is created automatically from the JSON config (reservations, PXE images and VLAN addresses are seeded from `router.json`), so runtime editing always has a starting point. To apply edits without a reboot, `POST /api/reload` on the admin API (default `127.0.0.1:8080`, see [Admin API](../admin-api.md)).

```json
"services": {
  "extrasFile": "/mnt/data/router-extras.toml"
}
```

## Supported keys

| Key | Purpose |
|-----|---------|
| `[reservations]` | `"mac" = "ip"` fixed leases, merged into every DHCP scope (VLANs, LAN, WiFi) |
| `[macImages]` | `"mac" = "file"` per-MAC TFTP image (DHCP boot-file precedence: iPXE > per-MAC > Apple > UEFI/legacy) |
| `[macRootPaths]` | `"mac" = "nfs:…"` per-MAC NFS root-path (option 17); overrides the per-arch Apple default, also applies to non-Apple clients |
| `defaultImage` | TFTP fallback for per-MAC requests that have no `macImages` entry |
| `pxeBootFile` | DHCP option 67 boot file (applied to all subnets) |
| `legacyBootFile` | Option 67 for legacy BIOS PXE clients |
| `uefiBootFile` | Option 67 for UEFI PXE clients |
| `ipxeScript` | Option 67 once a client identifies itself as iPXE (option 77) |
| `appleNetBoot` | Answer Apple NetBoot (BSDP) clients (see [Apple NetBoot (Mac)](../services.md#apple-netboot-mac)); boot files/root-paths per arch, defaults `macnbi-<arch>/booter` |
| `[vlanAddresses]` | `id = "cidr"` address override per VLAN (e.g. for per-VLAN TFTP/DHCP scopes). Applying changes requires a router restart — `/api/reload` rejects them with HTTP 409 |
| `[[smbUsers]]` | Extra SMB users: `name`, `password`. Granted access at startup; adding/removing users requires a router restart — `/api/reload` rejects the change with HTTP 409 |

## Boot-file precedence

The PXE boot-file precedence is `iPXE (ipxeScript) > per-MAC (macImages) > Apple NetBoot (appleNetBoot, AAPLBSDPC clients only) > uefiBootFile/legacyBootFile > pxeBootFile/defaultImage`. This is deliberate: once a client runs iPXE, its `ipxeScript` must win over the MAC mapping or the client would never leave the initial boot loader.

TFTP images themselves are served directly from disk, so dropping a new file into the TFTP root is enough — no restart needed. Extras-driven reservations and PXE settings require a restart or `/api/reload`.

## Extras file behavior

The extras file **replaces** (does not merge with) the base `services.pxe` overrides while it is present: `macImages`, `defaultImage`, `pxeBootFile`, `legacyBootFile`, `uefiBootFile` and `ipxeScript` from the JSON config are ignored once an extras file is in place. Values removed from the TOML file are removed at runtime as well — set `defaultImage = ""` to clear it.

When the extras file does not exist yet, it is auto-created and seeded from `router.json`, so nothing is lost until the file is edited.

## Example

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