# Proxmox LXC Wake Middleware - Operational Notes

## Container Lifecycle
- **Start**: Triggered automatically by Traefik on first request to a domain.
- **Stop**: A fixed-duration timer starts on every access. After `shutdown_minutes` (graceful) or `stop_minutes` (hard poweroff, wins) of idleness, the container is stopped.
- **External Stop**: If an admin stops the container via Proxmox UI/API, the watchdog detects it within `check_interval` minutes and cancels the pending stop timer. No double-stop attempts will occur.
- **Middleware Restart**: Pending stop deadlines persist in `state.json` and resume after a restart; overdue stops fire immediately.

## Auto-Discovery
- Containers with `wake.enable=true` + `wake.domain` in their Proxmox notes are discovered automatically — no `config.yaml` entry needed.
- Discovery runs at startup, then every `global.discovery_interval` seconds.
- A `config.yaml` entry for the same vmid always wins over a discovered one.

## Troubleshooting
- **Container not starting**: Check `PROXMOX_*` environment variables and network connectivity to Proxmox.
- **Watchdog not canceling timer**: `check_interval` (minutes) controls how often the watchdog polls `/api2/json/nodes/{node}/{kind}/{vmid}/status/current`.
- **Starting page not showing**: The middleware returns a `302` redirect to the `WAKE_DOMAIN` starting page; `WAKE_DOMAIN` must be routed to the middleware without forwardAuth.

## Manual Commands
- Restart middleware: `docker restart wake-lxc`
- Check logs: `docker logs -f wake-lxc`
- Force stop a container (will cancel timer): `pct stop <vmid>`
