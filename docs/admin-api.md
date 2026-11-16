# Admin API

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

## POST /api/reload

Re-reads the extras file and applies reservations and PXE settings live, then answers `200 ok`.

Changes that require a full restart — `[vlanAddresses]` edits or adding/removing `[[smbUsers]]` — are rejected with `409 Conflict` rather than silently half-applied; restart the router to pick those up.

The admin listener is unauthenticated for `/status` by design; it binds to loopback so remote clients cannot read it. Always set a `token` (or keep the listener loopback-only) if the router is reachable from untrusted networks.