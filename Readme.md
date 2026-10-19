# Wake LXC — Traefik Middleware Plugin

A Traefik middleware plugin that wakes idle Proxmox LXC containers and QEMU VMs on request, proxies the traffic to the backend, and stops the container after a configurable idle window. Stopped containers wake on the first request and sleep again after inactivity — no Docker, no separate process, no SSE.

## Features

- **Auto-Wake**: Starts a stopped container on the first request to its domain.
- **Proxying**: Forwards the request to the container's backend while it's running.
- **Starting page**: While a container boots, the client lands on a live progress page that polls for readiness and reloads when the app is up.
- **Auto-Discovery**: Containers opt in via `wake.*` labels in their Proxmox notes — no per-container config file.
- **Persistent Timers**: Stop deadlines persist in `state.json` and resume after a restart.
- **Watchdog & Circuit Breaker**: Cancels a pending stop if the container is stopped externally; gates repeated start attempts on API failure.
- **Traefik Native**: Runs inside Traefik as a Go middleware plugin (Yaegi).

## How it works

```
Request → Traefik router (Host rule) → wake-lxc middleware
                                   ├→ container running → proxy to backend
                                   └→ container stopped → start + serve starting page
```

1. Traefik routes the request to the `wake-lxc` middleware (via a `Host` rule per domain).
2. The middleware matches the `Host` against discovered containers.
3. If the container is stopped, the middleware sends a Proxmox start command and serves the starting page (which polls `__wake__/status` until ready, then reloads).
4. If the container is running, the middleware proxies the request to the backend.
5. A watchdog monitors the container and cancels a pending stop if it's stopped externally.
6. An idle timer stops the container (graceful `shutdown`, or hard `stop` via `wake.stop_minutes`) after inactivity; deadlines persist in `state.json`.

## Prerequisites

- Go (for building the plugin)
- Proxmox VE (8.x)
- Traefik v3 with the `traefik-iac` / plugin loader enabled
- LXC/QEMU containers running your services

## Installation

The plugin is a Go module. Traefik loads it via the plugin loader (Yaegi). Build it with:

```bash
go build ./...
```

Or install it for the `yaegi_test` target (see `Makefile`):

```bash
make yaegi_test
```

## Configuration

The plugin is configured via the Traefik plugin config section. There is no separate config file — all per-container settings come from Proxmox notes (`wake.*` labels), and global settings come from the plugin config.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `apiEndpoint` | string | — | Proxmox API base URL (required) |
| `apiTokenId` | string | — | API token ID (required) |
| `apiToken` | string | — | API token value (required) |
| `apiValidateSSL` | string | `true` | Verify Proxmox SSL certificate |
| `apiLogging` | string | `info` | Log level (`info` or `debug`) |
| `nodeName` | string | — | Proxmox node name (required) |
| `discoveryInterval` | string | `60s` | Seconds between discovery passes |
| `checkInterval` | string | `5m` | Watchdog poll interval |
| `shutdownMinutes` | string | `30` | Idle timeout (minutes) before stop |
| `domainTemplate` | string | — | Domain for discovered LXCs without `wake.domain`, e.g. `"{hostname}.sub.domain.name"` (LXC only) |
| `stateFile` | string | — | Path to the stop-deadline persistence file |

### Traefik plugin config

```yaml
http:
  middlewares:
    wake:
      plugin:
        wake-lxc:
          apiEndpoint: "https://pve:8006"
          apiTokenId: "wake"
          apiToken: "YOUR_TOKEN_VALUE"
          apiValidateSSL: "false"
          nodeName: "pve1"
          shutdownMinutes: "30"
          checkInterval: "5m"
          discoveryInterval: "60s"
```

## Per-container labels (Proxmox notes)

A container opts in by adding `wake.*` lines to its Proxmox notes/description:

```
wake.enable=true
wake.domain=n8n.sub.domain.name
wake.port=5678
wake.shutdown_minutes=30
```

| Label | Type | Description |
|-------|------|-------------|
| `wake.enable` | string | `true` opts the container into auto-discovery (required) |
| `wake.domain` | string | FQDN that triggers this container; wins over `domainTemplate` |
| `wake.port` | int | Backend port, combined with the resolved IP to build the backend URL |
| `wake.host` | string | Backend host override (skips IP resolution) |
| `wake.backend` | string | Full backend URL override (highest priority) |
| `wake.shutdown_minutes` | float | Graceful stop after N idle minutes |
| `wake.stop_minutes` | float | Hard poweroff after N idle minutes (wins over `wake.shutdown_minutes`) |

### Backend resolution order

`wake.backend` → `wake.host`+`wake.port` → static `ip=` off net0 → SDN IPAM lookup by vmid (cached 30s) → no backend.

### Domain resolution order

`wake.domain` → `domainTemplate` (LXC only, from the LXC's `hostname` field) → error.

QEMU VMs have no OS hostname, so they must set `wake.domain`.

## Traefik routing

Each container domain gets a router with a `Host` rule pointing to the `wake-lxc` middleware and a backend service:

```yaml
http:
  routers:
    n8n:
      rule: "Host(`n8n.sub.domain.name`)"
      entryPoints: [web, websecure]
      middlewares: [wake]
      service: n8n-backend
  services:
    n8n-backend:
      url: "http://10.0.100.5:5678"
```

The middleware proxies to the backend (the `url` in the service) when the container is running. When stopped, it serves the starting page.

## State file

Stop deadlines persist in `state.json` (path from `stateFile`). On restart, pending stops resume. The format is:

```json
{
  "deadlines": {
    "100": 1700000000
  }
}
```

## Troubleshooting

- **Container won't start**: Verify token permissions (`VM.PowerMgmt`) and network connectivity to Proxmox.
- **Watchdog cancels stop prematurely**: Increase `checkInterval` or ensure the container takes longer to boot than the interval.
- **Circuit breaker open**: Check Proxmox API logs. The breaker resets after 5 minutes.
- **Discovery finds no containers**: The token needs node-level `VM.Audit` + `VM.Config.Options`; IPAM backends additionally need `SDN.Audit`.

## License

MIT License
