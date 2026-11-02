# Project Context

## Objective
- Enable IPv6 (SLAAC/RA + DHCPv6) on the deployed BPI-R1 gokrazy router and validate live; NAT66 was attempted but is blocked by kernel support.

## Important Details
- Repo: `~/GIT/gokrazy/gokrazy-router`; deploy target: gokrazy instance `bpi-r1` at `~/gokrazy/bpi-r1`; node IP `192.168.168.142`, HTTP auth `gokrazy:0IEC4TtD7LnWYCHihKJJ`.
- Branch: `feature/ipv6`; pushed commits: `be3382e` (IPv6 feature), `905239b` (link-local wait), `42d41e4` (DAD wait), `5d6aacb` (NODAD fe80::1), `c50584a` (strip zone before ndp.Addr), `5002593` (skip option 59 for iPXE). Local checkout used via `replace` in instance `go.mod`.
- RFC 5970 netboot: bootfile URL (option 59) only offered to clients that advertise arch via option 61 AND are NOT iPXE (user class 15); iPXE clients already got their script from IPv4 DHCP, and option 59 pointing at IPv6 TFTP (server listens 0.0.0.0:69 only) made them boot-loop (`OACK error for undionly.kpxe: timeout`). Legacy = arch list exactly `{0}`; UEFI = any other arch (incl. mixed `{0,7}` CSM case).
- Deploy verb: `gok -i bpi-r1 update` (builds for linux/arm, GOARM=7, uploads root+boot, reboots). Docker use: `rtk docker run --rm -v "$(pwd)":/workspace -w /workspace golang:1.25 bash -c '...'` — only reliable build/test env on macOS node host.
- IPv6 config is opt-in per scope; host macOS cannot build `pkg/dhcp`, `pkg/pxe` (missing `unix.SOCK_CLOEXEC`, `BindToDevice`), so cross/docker checks required.
- `router.json` changed: `wan.mode6=auto`, global `dns6` (Cloudflare+Google), per-VLAN `address6=fd00:N::1/64`, `ra=true`, `dhcp6=true`, WiFi `address6=fd00:31:1::1/64`; `nat.enabled6` was added but then removed (see Blocked). Also kept `ExtraFilePaths` hostapd/smb, `router-extras.toml` on `/mnt/sata`.
- NETBOOT/IPXE vs IPv6 (Lenovo `5c:ff:35:0d:b3:49` on br-vlan31): any v6 state broke the `chain http://boot.netboot.xyz/menu.ipxe` download (`could not boot. could not start download. Operation not supported.` — the shown error is the `https://` fallback chain, ENOTSUP since undionly.kpxe has no HTTPS; it masks the real http failure). Root cause: client got v6 DNS (RDNSS/DHCPv6) → resolved AAAA → tried IPv6 HTTP from ULA `fd00:31::2` → no NAT66 → internet drops ULA → download fails. Bisected live: full RA+DHCP6 broke it, SLAAC-only (RA+RDNSS) broke it, v6 fully off worked. FIX: per-scope `"dns6": []` in `router.json` suppresses v6 DNS for that scope (`applyGlobalDNS6` only inherits when `DNS6 == nil`; empty non-nil = advertise none). vlan31 now `ra=true, dhcp6=false, dns6:[]` (SLAAC-only, v4 DNS) — Lenovo boots AND IPv6 SLAAC works.
- The TFTP `OACK error for undionly.kpxe: timeout after 12 retries` is benign noise: PXE ROM sends a `tsize`-only probe RRQ (e.g. src port 2070) + a `blksize` RRQ (2071); probe is abandoned, server OACK times out exactly 36s later. Not a regression, ignore.
- Per-scope `dns6` semantics: absent/nil = inherit global `dns6`; `"dns6": []` = advertise no IPv6 DNS (RDNSS + DHCPv6 option 23). `router-extras.toml` `dns6` (non-empty) still overrides all scopes.
- RFC 5970 netboot: bootfile URL only offered to clients that advertise option 61; legacy = arch list exactly `{0}`; UEFI = any other arch (including mixed `{0,7}` CSM case).
- `mdlayher/ndp` `Listen(ifi, ndp.Addr("fe80::1"))` — literal must be passed UNZONED; `findAddr` matches `netip.Addr` with zone equality, iface addrs from `ifi.Addrs()` are unzoned. `Listen` then re-zones for the socket bind.
- Kernel fe80:: generation is async (addrconf/DAD) and unbindable while tentative; custom kernel flag reporting is unreliable, so don't poll `IFA_F_TENTATIVE`.

## Work State
### Completed
- IPv4/VLAN DHCP + PXE (legacy/UEFI/iPXE/MAC overrides) already working; DHCPv6 RFC 5970 bootfile config (`SetPXEBootFile6`, option 59 URL) implemented and tested (`pkg/ra` tests 10 passed incl. new `TestDHCP6PXEBootfile*`).
- Docker `go test ./...` all pass on linux (incl. pkg/dhcp, pkg/pxe, pkg/ra); `go build ./cmd/...` OK for amd64/arm/arm64.
- Crash-loop on old revision (nat6: nftables flush: operation not supported) fixed by removing `nat.enabled6`; redeployed to clean revision.
- RA now RUNNING on all five scopes after `c50584a`: `ra: advertising fd00:N::1/64 on br-vlanN (link-local fe80::1%br-vlanN, managed=true)` — see Active/blocked history below.
- Netboot regression FIXED: `applyGlobalDNS6` (main.go) changed to inherit global `dns6` only when scope `DNS6 == nil`; explicit `"dns6": []` suppresses v6 DNS. vlan31 = SLAAC-only (`ra=true, dhcp6=false, dns6:[]`) — Lenovo netboot works with IPv6 still enabled on the VLAN. Deployed + verified live.

### Active
- RA servers advertising on br-vlan1, br-vlan20, br-vlan30, br-vlan31, wlan0 with source `fe80::1`; DHCPv6 servers confirmed serving on br-vlan1/20/30/wlan0 (`dhcp6: serving on br-vlanN ... 2 DNS servers`); vlan31 is SLAAC-only (no DHCPv6, no v6 DNS). `netsetup: IPv6 forwarding enabled`; `wan: wan in IPv6 SLAAC mode`.
- Next: validate a live client on a VLAN actually obtains an SLAAC/DHCPv6 address and IPv6 connectivity through the router; test RA from a real client.

### Blocked
- NAT66 permanently disabled in `router.json` (kernel lacks IPv6 nftables NAT; `operation not supported`). Re-enable only after rebuilding `github.com/consolving/gokrazy-kernel-a20` kernel with IPv6 nftables/NAT config; kernel repo has no shipped `.config`.

## Next Move
1. Validate end-to-end IPv6 on a real client (e.g. `ip -6 addr`, ping `fd00:N::1`, DHCPv6 via `dhclient -6` or `radvd`-free `rdisc6`) on a VLAN the router serves (vlan1/20/30 or wlan0 — full RA+DHCPv6).
2. If NAT66 desired, rebuild A20 kernel with IPv6 nftables config; if gated, keep `nat.enabled6` absent from `router.json`.

## Relevant Files
- `pkg/ra/ra.go` — `ensureLinkLocal` installs `fe80::1/64` with `unix.IFA_F_NODAD`, waits for it in `netlink.AddrList`, returns zoned addr; `Start()` passes `ndp.Addr(ll.WithZone("").String())`.
- `pkg/ra/dhcp6.go` — `DHCP6Server`, `SetPXEBootFile6`, `pxeBootfileURL`, `clientArchs`, `clientIsLegacy`, `ipxeClient` (skips option 59).
- `cmd/gokrazy-router/main.go` — `applyGlobalDNS6` (inherit global dns6 only when scope `DNS6 == nil`; empty = suppress), `startIPv6Scope` wires RA/DHCP6 + PXE (line ~318); NAT6 setup at lines ~228-262.
- `cmd/gokrazy-router/main_test.go` — `TestApplyGlobalDNS6` (nil-inherit vs empty-suppress).
- `~/gokrazy/bpi-r1/router.json` — active deployed IPv6 settings; `nat.enabled6` removed; vlan31 has `ra=true, dhcp6=false, "dns6": []`.
- `~/gokrazy/bpi-r1/config.json` / `go.mod` — instance `replace` → local repo; build env `GOARCH=arm GOARM=7`.
- `/Users/philipp/GIT/gokrazy/gokrazy-A20-kernel` — kernel for BPI-R1; needs IPv6 nftables config for NAT66.
- `pkg/ra/dhcp6_test.go` — new IPv6 netboot tests; `pkg/dhcp/dhcp_test.go` — existing IPv4 PXE precedence tests.
