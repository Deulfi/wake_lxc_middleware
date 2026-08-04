# wake_lxc_middleware: Proxmox Container On-Demand Auto Start Service

**wake_lxc_middleware** is a Traefik ForwardAuth middleware that automatically starts a stopped Proxmox LXC/VM on first request and stops it again after a set period.

> **Docker: unverified / unsupported.** This project is developed and run directly via systemd on a Proxmox host (see Manual Installation below), not in Docker. The Docker instructions below are kept from an earlier version of this repo but have not been tested against the current code and may not work. If you get it running in Docker, a PR is welcome.

## ✨ Features
- **Auto-Wake**: Starts a stopped container on first request, via Traefik ForwardAuth
- **Real-time Status**: SSE-based progress page while the container boots
- **Backend Readiness Check**: Optionally waits for the app inside the container to actually respond, not just for the OS to report "running"
- **Watchdog & Circuit Breaker**: Cancels pending stop timers if a container is stopped externally; backs off on repeated Proxmox API failures

## 📋 Prerequisites
- Proxmox VE
- Traefik (v2/v3) already routing your other services
- Python 3.11+ (for manual install)

---

## 🐧 Manual Installation

### 1. Install dependencies
```bash
apt update
apt install -y python3 python3-pip git curl ca-certificates
pip install -r requirements.txt --break-system-packages
```

### 2. Clone the repository
```bash
cd /opt/
git clone https://github.com/Deulfi/wake_lxc_middleware.git
cd wake_lxc_middleware
```

### 3. Create a Proxmox API token
1. `Datacenter → Permissions → Users` — create a dedicated user (e.g. `svc-wake@pve`)
2. Assign minimal permissions: `VM.PowerMgmt` and `VM.Audit`
3. `Datacenter → Permissions → API Tokens` — generate a token for that user
   - Uncheck "Privilege Separation" for simpler permission inheritance
   - Save the token secret immediately — it's shown only once

### 4. Configure environment variables
```bash
cp .env.example .env
```
Fill in your Proxmox host/node/token details and `WAKE_DOMAIN` (see table below).

### 5. Configure container mappings
```bash
cp config.yaml.example config.yaml
```
Map each domain to its Proxmox VMID (see table below).

### 6. Configure Traefik
On your Traefik instance, add a router + middleware for each domain you want to gate, plus one ungated router for `WAKE_DOMAIN`. Example (replace domains, IPs, and ports with your own):

```yaml
http:
  middlewares:
    wake-lxc-auth:
      forwardAuth:
        address: "http://<middleware-host>:8080/auth"
        trustForwardHeader: true

  routers:
    wake-lxc-ui:
      rule: "Host(`wake.example.com`)"
      service: wake-lxc-middleware
      # no middlewares — this router must NOT be gated by wake-lxc-auth
      entryPoints: [websecure]
      tls: {}

    myapp:
      rule: "Host(`myapp.example.com`)"
      service: myapp
      middlewares: [wake-lxc-auth]
      entryPoints: [websecure]
      tls: {}

  services:
    myapp:
      loadBalancer:
        servers:
          - url: "http://<myapp-backend-ip>:<port>"
    wake-lxc-middleware:
      loadBalancer:
        servers:
          - url: "http://<middleware-host>:8080"
```

**Important:** `WAKE_DOMAIN`'s router must have no `forwardAuth` middleware attached. It's where the "starting..." progress page and its status stream live — if it's gated by the same auth check it's trying to get past, the whole flow breaks.

### 7. Create the systemd service
Create `/etc/systemd/system/wake_lxc_middleware.service`:
```ini
[Unit]
Description=Wake LXC Middleware
After=network.target

[Service]
EnvironmentFile=/opt/wake_lxc_middleware/.env
WorkingDirectory=/opt/wake_lxc_middleware
ExecStart=/usr/local/bin/uvicorn asgi_app:app --host ${BIND_HOST} --port ${BIND_PORT}
Restart=always
User=root
StandardOutput=journal
StandardError=journal
SyslogIdentifier=wake-lxc-middleware

[Install]
WantedBy=multi-user.target
```

### 8. Start the service
```bash
systemctl daemon-reload
systemctl enable --now wake_lxc_middleware
journalctl -u wake_lxc_middleware -f
```

---

## 🐳 Docker (unverified, may not work as-is)

```bash
git clone https://github.com/Deulfi/wake_lxc_middleware.git
cd wake_lxc_middleware
cp .env.example .env      # fill in your values
cp config.yaml.example config.yaml
docker compose up -d
docker compose logs -f
```
Check `docker-compose.yml` and adjust network names/volumes to your setup. Again: this path is untested against the current code — treat it as a starting point, not a guarantee.

---

## 📖 Configuration Reference

### `config.yaml`
| Field | Required | Description |
|-------|----------|-------------|
| `global.stop_minutes` | yes | Default: stop N minutes after last access |
| `global.check_interval` | yes | Default: watchdog poll interval, in minutes |
| `containers[].vmid` | yes | Proxmox VMID |
| `containers[].kind` | no (default `lxc`) | `lxc` or `qemu` — Proxmox's own terms; note it's `qemu`, not `vm` |
| `containers[].domain` | yes | External hostname; used to look up which container a request is for |
| `containers[].backend` | no | Internal `host:port` of the real app, used only to check the app itself has finished starting (not just that the OS booted). Omit to skip this check. |
| `containers[].stop_minutes` | no | Overrides the global default for this container |
| `containers[].stop_mode` | no (default `shutdown`) | `shutdown` (graceful) or `stop` (force poweroff) |
| `containers[].check_interval` | no | Overrides the global default for this container |

### `.env`
| Variable | Required | Description |
|----------|----------|-------------|
| `PROXMOX_HOST` | yes | Proxmox IP/hostname |
| `PROXMOX_NODE` | yes | Proxmox node name |
| `PROXMOX_TOKEN_USER` | yes | API token user, e.g. `svc-wake@pve` |
| `PROXMOX_TOKEN_ID` | yes | API token ID |
| `PROXMOX_TOKEN_VALUE` | yes* | API token secret |
| `PROXMOX_TOKEN_VALUE_FILE` | yes* | Path to a file containing the token secret, as an alternative to `PROXMOX_TOKEN_VALUE` |
| `PROXMOX_VERIFY_TLS` | no (default `false`) | Verify Proxmox's TLS certificate |
| `WAKE_DOMAIN` | yes | Dedicated domain for the "starting..." page, routed WITHOUT forwardAuth (see step 6 above) |
| `CONFIG_PATH` | no (default `config.yaml`) | Full path to `config.yaml`, including the filename |
| `STATE_FILE` | no | Full path to the state file (where stop timers are saved) |
| `BIND_HOST` | no (default `0.0.0.0`) | Address uvicorn binds to |
| `BIND_PORT` | no (default `8080`) | Port uvicorn binds to |
| `LOG_LEVEL` | no (default `INFO`) | `INFO` or `DEBUG` |

*One of `PROXMOX_TOKEN_VALUE` or `PROXMOX_TOKEN_VALUE_FILE` is required.

`TZ` is not read by the application — set it at the systemd unit level (`Environment=TZ=...`) or system-wide if you need it, not in `.env`.

---

## 🔧 Troubleshooting

- **Container won't start**: check the token has `VM.PowerMgmt`, and that `PROXMOX_HOST`/`PROXMOX_NODE` are reachable from this host.
- **404 "Container not found" on `/auth`**: the `Host`/`X-Forwarded-Host` value Traefik is sending doesn't match any `domain` in `config.yaml`. Check `trustForwardHeader: true` is set on your Traefik forwardAuth middleware.
- **Bad Gateway right after the starting page redirects back**: the container's OS came up but the app inside hadn't started listening yet. Add a `backend` field for that container so the middleware waits for the actual app, not just the OS.
- **"Connection lost" on the starting page, looping start attempts**: `WAKE_DOMAIN` is missing its own Traefik router, or that router still has `forwardAuth` attached to it. It must be routed directly with no auth middleware.
- **Watchdog not cancelling a stop timer after an external stop**: check `check_interval` isn't longer than how quickly you need it to react — the watchdog polls `/api2/json/nodes/{node}/lxc/{vmid}/status/current` on that interval.
- **Circuit breaker open / status checks skipped**: the breaker opens for 5 minutes after a real Proxmox API failure (not for a container being legitimately stopped). Check Proxmox connectivity and token validity if this fires repeatedly.

### Manual commands
- Restart middleware: `systemctl restart wake_lxc_middleware`
- Check logs: `journalctl -u wake_lxc_middleware -f`
- Force-stop a container (cancels any pending stop timer via the watchdog): `pct stop <vmid>`

## 🏗️ Architecture
```
Protected domain (e.g. convertx.pve.lan):
  Browser → Traefik → forwardAuth → /auth
    running  → 200 OK → Traefik proxies to the real backend
    stopped  → starts container → 302 redirect → WAKE_DOMAIN

WAKE_DOMAIN (no forwardAuth):
  Browser → Traefik → /starting (SSE progress page)
    polls /status-stream until the container (and optionally its app) is ready
    → redirects browser back to the original protected domain
```

## 📜 License
MIT License
