# Status API

The daemon serves a JSON status endpoint at `http://127.0.0.1:8080/status` (on the router itself) with port statistics and per-client traffic counters. Configure `services.adminAPI.listen` to expose it on another interface.

Use the CLI tool to query it:

```bash
router-cli --host 10.0.31.1
router-cli --host 10.0.31.1 --json
```

## Example output

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

## Columns

- **VIA** — `V<id>` for VLAN clients, `W` for WiFi, `L` for flat-mode LAN
- **UL RATE / DL RATE** — Current throughput (sampled every 5 seconds)
- **LINK** — WiFi link rate (from hostapd control socket)
- **SIGNAL** — WiFi signal strength in dBm (from hostapd control socket)
- **UL / DL** — Current session traffic (reset on reconnect)
- **TOTAL UL / TOTAL DL** — Accumulated traffic across all sessions since boot
- **SPEED** — Negotiated port link speed and duplex