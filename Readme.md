# Wake LXC — Traefik Middleware Plugin

A Traefik middleware plugin that wakes idle Proxmox LXC containers and QEMU VMs on request, shows a starting page while they boot, and stops them after a configurable idle window. Stopped guests wake on the first request and sleep again after inactivity.

## Features

- **Auto-Wake**: Starts a stopped container on the first request to its domain.
- **Starting page**: While a container boots, the client lands on a progress page that reloads when the app is up.
- **Auto-Stop**: Stops the container after an idle window (graceful `shutdown`, or hard `stop` via `wake.stop_minutes`).
- **Auto-Discovery**: Containers opt in via `wake.*` labels in their Proxmox notes — no per-container config file.

## How it works

```
Request → Traefik router (Host rule)
              └→ wake-lxc middleware (gate)
                   ├─ running  → let request through → Traefik proxies to backend
                   └─ stopped  → send start cmd → serve starting page
```

1. Traefik routes the request to the `wake-lxc` middleware (via a `Host` rule per domain).
2. If the container is running, the middleware lets the request through — Traefik proxies it to the backend. If it's stopped, the middleware sends a Proxmox start command and serves the starting page (which polls `__wake__/status` until ready, then reloads).
3. An idle timer stops the container after inactivity (graceful `shutdown`, or hard `stop` via `wake.stop_minutes`).

A watchdog cancels a pending stop if the container is stopped externally; a circuit breaker gates repeated start attempts when the API is flaky.

## Prerequisites

- Proxmox VE (8.x)
- Traefik (v2 or v3) with the plugin loader enabled
- LXC/QEMU containers running your services

## Installation

### Proxmox setup

The middleware drives Proxmox with a dedicated API token. Create a service user and a scoped token — the `pveum` commands are in [`pve_commands.md`](pve_commands.md).

- Base: `VM.Audit`, `VM.Monitor`, `VM.PowerMgmt`
- Auto-discovery adds: `VM.Config.Options` (reads the notes where `wake.*` labels live)
- IPAM backends add: `SDN.Audit`

The token's `value` (shown once) and its ID go into `apiToken` / `apiTokenId` below.

### Load the plugin (static config)

Add to your Traefik static config (`traefik.yaml`):

```yaml
experimental:
  plugins:
    wake-lxc:
      moduleName: github.com/Deulfi/wake_lxc_middleware
      version: v0.1.0
```

### Configure the middleware (dynamic config)

Add a middleware to your Traefik dynamic config:

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

## Configuration

The middleware's global settings come from the plugin config above; per-container settings come from Proxmox notes (`wake.*` labels).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `apiEndpoint` | string | — | Proxmox API base URL (required) |
| `apiTokenId` | string | — | API token ID (required) |
| `apiToken` | string | — | API token value (required) |
| `apiValidateSSL` | string | `true` | Verify Proxmox SSL certificate |
| `apiLogging` | string | `info` | Log level (`info` or `debug`) |
| `nodeName` | string | — | Proxmox node name (required) |
| `discoveryInterval` | string | `60s` | Interval between discovery passes |
| `checkInterval` | string | `5m` | Watchdog poll interval |
| `shutdownMinutes` | string | `30` | Fallback idle timeout (minutes) before stop |
| `domainTemplate` | string | — | Domain for discovered LXCs without `wake.domain`, e.g. `"{hostname}.sub.domain.name"` (LXC only) |
| `stateFile` | string | — | Path to the stop-deadline persistence file |

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

`wake.backend` → `wake.host`+`wake.port` → static `ip=` off net0 → SDN IPAM lookup by vmid (cached 30s) → **no probe** (the container is treated as ready the moment it's running).

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

The middleware lets the request through to the backend (the `url` in the service) when the container is running. When stopped, it serves the starting page.

## State file

`stateFile` is empty by default — with it unset, stop deadlines live only in memory and are **lost on a Traefik restart** (a pending stop simply doesn't resume). Set it to a path on a persistent volume to resume pending stops across restarts. The file is plain JSON:

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
- **Changed `apiToken`/`apiEndpoint`/`discoveryInterval` but nothing happened**: Traefik calls this plugin's `New` again on every dynamic-config reload, but the middleware reuses its existing background instance (same `apiEndpoint`+`nodeName`) instead of restarting it, so an already-running Proxmox client and discovery ticker don't pick up those changes. `checkInterval`, `shutdownMinutes`, `domainTemplate`, and `stateFile` do hot-reload; a credential or `discoveryInterval` change needs a real Traefik restart.

## Building from source

The plugin is a Go module. Build it with:

```bash
go build ./...
```

Or run the Yaegi test target (see `Makefile`):

```bash
make yaegi_test
```

## License

MIT License
