# Design evidence

Measured on this Windows + WSL host, 2026-11-25.

## What did not work

| Attempt | Result |
|---|---|
| `usque socks -6` / `l4-socks -6` on Windows and WSL | `network is unreachable`; no global IPv6 route on this host |
| `usque register -c <node>` (fresh WARP registration) | timed out; Cloudflare registration path is blocked/rate-limited here |
| Change `endpoint_v4` across 8 public Cloudflare endpoints | egress IPv4 unchanged |
| Swap WARP config files between usque nodes | egress unchanged; all nodes share endpoint and `ipv4: 172.16.0.2` |
| Old DSH IPv6 residential proxy (`2407:5…`, 36642/35642) | no listener/process remains; it was a third-party proxy, not WARP/usque |

`l4-socks` exits on this PC produce only three IPv4 egress groups:
`104.28.222.73`, `104.28.222.75`, `104.28.254.73`.

## What works

`chipmax/o2a-v2` `internal/warp` implements WARP as **userspace WireGuard**:

- `stack: dual`: handshake over IPv4, tunnel address is IPv6. This is the
  documented mode for hosts with no IPv6 route.
- `require_ipv6_egress: true`: rejects exits whose public egress is the shared
  Cloudflare IPv4, keeping only a private IPv6 `/64` per registration.
- `rotate_keys: true`: renews the WireGuard key on the same device instead of
  deleting and re-registering it (which caused ~45 s of adapter flapping in
  older rotators).
- `Pool.Rotate(prefix)` retires a spent bucket and refills from a fresh
  registration in the background.

This project reuses that package, then adds an HTTP `CONNECT` relay and a
small control API so `pi-freeflow` can spend one quota bucket per listener and
rotate a single listener on 429.

## Why CONNECT, not HTTP forwarding

OpenCode Zen free tier fingerprints the TLS stack: a relay that re-originates
the request from Node or an edge worker gets `429 FreeUsageLimitError` even
with a fresh IP. The relay here carries raw TCP only; the Bun sidecar in
`pi-freeflow` still performs the TLS handshake, so the upstream sees Bun's
fingerprint while the packet leaves from the WARP IPv6 egress.

## Verified earlier in this environment

- 10 local `usque` exits gave three IPv4 groups; `/rotate` swapped a listener
  from `104.28.222.73` to `104.28.222.75`, proving relay-level rotation.
- A mixed pool of local + WarpGate VPS exits ran all 33 omp models twice:
  33/33 OK both passes (mean 5.8 s / 5.7 s, max 27.5 s / 32.5 s).
- Log showed 429 buckets being cooled and a sibling relay serving the same
  turn; no user-visible failure.

## Tradeoffs

- GPL-3.0 because the WARP package is derived from o2a-v2 (GPL-3.0).
- Depends on Cloudflare WARP free registrations; rate limits can delay first
  start. `preload` keeps exits warm.
- `warp-state.json` contains confidential device keys and must not be
  committed. Deleting it on a new machine registers fresh devices.
- Respect the upstream free-tier terms; this is a personal reliability tool,
  not a quota-bypass service for third parties.
