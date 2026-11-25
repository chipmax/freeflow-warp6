# freeflow-warp6

Standalone **IPv6-only WARP egress relays** for `pi-freeflow` in omp.

No VPS. No running o2a service. No Cloudflare WARP desktop client. Each relay
is an HTTP `CONNECT` tunnel backed by its own userspace WireGuard WARP
registration, so free-tier quota is keyed to a private IPv6 `/64` instead of a
shared Cloudflare IPv4.

> Status: experimental. Single-relay end-to-end IPv6 egress and `/rotate`
> were verified on this host; a full 33-model load pass is still limited by
> Cloudflare's registration pacing. See `docs/DESIGN.md` for evidence. GPL-3.0, derived from `chipmax/o2a-v2` `internal/warp`
> (see `NOTICE`).

## Why

- A host with no global IPv6 can still establish WARP over IPv4 and route
  traffic out through the tunnel's IPv6 address (`stack: dual`).
- `require_ipv6_egress: true` discards exits whose public egress is the shared
  Cloudflare IPv4 pool, which is normally already quota-limited.
- `rotate_keys: true` renews a device key in place instead of
  `registration delete` + `new`, avoiding ~45 s adapter flapping.
- One relay URL per exit lets `pi-freeflow` spread concurrent sessions across
  independent quota buckets and cool a single bucket on 429.

## Layout (planned / being implemented)

```
cmd/freeflow-warp6/     main binary: CONNECT listeners + control API
warp/                   userspace WARP pool, derived from o2a-v2 (GPL-3.0)
config.example.json     all knobs, no secrets
scripts/                install + freeflow state wiring for Windows
docs/DESIGN.md          evidence, tradeoffs, measured results
```

## Quick start (new machine)

1. Install Go 1.24+ and Node/Bun (Bun 1.1+ for `PI_FREEFLOW_ZEN_TRANSPORT`).
2. Install/link `pi-freeflow` into omp.
3. Clone this repo and run `scripts/install.ps1` from PowerShell as the normal
   user. It builds the binary, creates `config.json` from the example, writes
   the relay URLs into `~/.pi/agent/pi-freeflow-relay-state.json`, sets:

   ```text
   PI_FREEFLOW_ZEN_TRANSPORT=bun-sidecar
   PI_FREEFLOW_ALLOW_UNSAFE_RELAY=1
   ```

   and starts the relay at logon via the user Startup folder. No admin rights
   are required; no path to a VPS or o2a is embedded.
4. `warp-state.json` is created on first start with mode 0600/ACL. **Treat it
   as a secret.** Copy it to another machine only if you want to keep the same
   WARP devices; delete it to register fresh ones.

## Control API

- `GET /health` — pool states, active egress `/64` prefixes, rotation counters.
- `POST /rotate?relay=18110` — rotate the WARP key behind that listener; the
  control server rate-limits repeated rotations (default 240 s per relay).

`pi-freeflow` only needs `/rotate`: its 429/quota hook calls this endpoint so
a limited bucket is moved to a fresh egress.

## Model compatibility

The upstream free lanes are `opencode.ai`, `api.kilo.ai` and OVHcloud AI
Endpoints. The allowlist in `config.json` must include exactly those hosts;
the tunnel payload is opaque, so the Bun TLS fingerprint is preserved.

### Known model caveats on this host

- `cline/*` models require an authenticated Cline browser session. Without it
  omp returns `401 ... re-authenticate your Cline account`; this is an account
  state issue, not an egress failure.
- `longcat-2.5-free` is retired upstream and is excluded from the verdict.
- `ovh-free/*` models return `405 CONNECT only` on this CONNECT-only relay
  path (verified with `ovh-free/Qwen3.5-9B`); they need a different transport
  and are excluded from the whitelist.
- The `nvidia/*:free`, `openrouter/free`, `particle-ai/particle-1:free` and
  `tidepool-alpha-free` probes timed out at 120s even with stdin closed; they
  are not in the verified no-auth whitelist.
- A bare `OK` reply proves reachability only; it does not prove a model can
  answer the real omp prompt and produce parseable output.

## License

GPL-3.0-or-later. See `LICENSE` and `NOTICE`. This repository includes code
derived from o2a-v2 under the same license. It is not affiliated with
Cloudflare.
