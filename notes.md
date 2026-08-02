# Proxmox LXC Wake Middleware - Operational Notes

## Container Lifecycle
- **Start**: Triggered automatically by Traefik on first request to a domain.
- **Stop**: Triggered automatically after `stop_minutes` of uptime.
- **External Stop**: If an admin stops the container via Proxmox UI/API, the watchdog detects it within `check_interval` seconds and cancels the pending stop timer. No double-stop attempts will occur.

## Troubleshooting
- **Container not starting**: Check `PROXMOX_*` environment variables and network connectivity to Proxmox.
- **Watchdog not canceling timer**: Ensure `check_interval` is reasonable. The watchdog polls `/api2/json/nodes/{node}/lxc/{vmid}/status/current`.
- **Traefik not relaying progress page**: Ensure Traefik is configured with `forwardAuth` and `trustForwardHeader: true`. The middleware returns `503` with HTML when starting, which Traefik relays to the browser.

## Manual Commands
- Restart middleware: `docker restart wake_lxc_middleware`
- Check logs: `docker logs -f wake_lxc_middleware`
- Force stop a container (will cancel timer): `pct stop <vmid>`
