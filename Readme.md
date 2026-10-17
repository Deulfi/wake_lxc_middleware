# Wake-LXC-Middleware: Proxmox Container On-Demand Auto Start Service

** This is a slork (AI slop fork) and there is no docker image **
**wake_lxc_middleware** is a lightweight wake-on-demand middleware and status monitor that automatically starts Proxmox LXC/QEMU containers based on incoming traffic and stops them after a defined idle delay. It saves resources while maintaining seamless user access: stopped containers wake on first request and sleep again after inactivity. While a container boots you land on a live progress page on `WAKE_DOMAIN` that jumps back to your URL the moment the app is ready.

## ✨ Features
- **Auto-Wake**: Starts stopped containers on first request via Traefik ForwardAuth
- **Real-time Status**: SSE-based progress page while containers boot
- **Auto-Discovery**: Containers can opt in via `wake.*` labels in their Proxmox notes
- **Persistent Timers**: Stop deadlines persist in `state.json` and resume after a restart
- **Watchdog & Circuit Breaker**: Prevents double-stops and handles API failures gracefully
- **Traefik Native**: Designed to work seamlessly with Traefik v2/v3
- **Secure**: Supports Docker secrets and environment-based credential management

## 📋 Prerequisites
- Docker & Docker Compose
- Proxmox VE (8.x)
- Traefik Reverse Proxy
- LXC/QEMU containers running your services

## 🚀 Quick Start

### 1. Clone & Setup
```bash
git clone https://github.com/Deulfi/wake_lxc_middleware.git
cd wake_lxc_middleware
```

### 2. Create Proxmox API Token
1. Create a dedicated user in Proxmox (`Datacenter → Permissions → Users`)
2. Assign base permissions: `VM.Audit`, `VM.Monitor`, `VM.PowerMgmt` (plus `VM.Config.Options` and `SDN.Audit` when using auto-discovery/IPAM — see Auto-Discovery below). With privilege separation, grant them to the token (`user!tokenId`), not just the user.
3. Generate an API Token (`Datacenter → Permissions → API Tokens`)
   - **Keep** "Privilege Separation" **checked** — the middleware authenticates as `user!tokenId`, which only exists for privilege-separated tokens
   - **Save** the token value immediately (shown only once)

### 3. Configure Secrets & Environment
```bash
mkdir -p secrets
echo -n "YOUR_PROXMOX_TOKEN_VALUE" > secrets/proxmox_token_value.txt
chmod 644 secrets/proxmox_token_value.txt
```

Create `.env` from the example:
```bash
cp .env.example .env
# Edit .env with your Proxmox host, node, and token details
```

### 4. Configure Container Mappings
Edit `config.yaml` to map domains to Proxmox VMIDs:
```yaml
global:
  shutdown_minutes: 30      # graceful stop after 30 idle minutes
  check_interval: 5         # watchdog poll interval, minutes (required)
  discovery_interval: 60    # optional; seconds between discovery passes (default 60)

containers:
  - vmid: 105
    kind: lxc
    domain: docmost.sub.domain.name
    port: 80  # app's port, used for the readiness probe (or use 'backend' for a full URL)
    shutdown_minutes: 15

  - vmid: 106
    kind: lxc
    domains:
      - n8n.sub.domain.name
      - n8n-alt.sub.domain.name
    backend: http://n8n.internal:5678
```
*Note: Each container needs a unique `domain` (or `domains` list) that matches your Traefik router. A `config.yaml` entry always wins over a container discovered from Proxmox notes.*

#### Auto-Discovery (Proxmox notes, optional)

A container can opt in without a `config.yaml` entry — one `wake.key=value` line per flag in its Proxmox notes/description:
```
wake.enable=true
wake.domain=n8n.sub.domain.name
wake.port=5678
wake.shutdown_minutes=30
```
- `wake.domain` sets the FQDN. For LXCs it can be omitted: with `global.domain_template` (e.g. `"{hostname}.sub.domain.name"`) the domain is built from the LXC's own `hostname` field. QEMU VMs have no OS hostname, so they must set `wake.domain`.
- With `domain_template` set, an LXC needs only `wake.enable=true` + `wake.port` — as long as the templated domain and resolved IP match what Traefik routes to.
- Backend resolution order: `wake.backend` → `wake.host`+`wake.port` → static `ip=` off net0 (already in hand from the same /config call — free) → SDN IPAM lookup by vmid (`/cluster/sdn/ipams` + `/{ipam}/status`, cached 30s; needs `SDN.Audit`).
- Discovered containers are merged in memory only — never written to `config.yaml`. A `config.yaml` entry for the same vmid always wins.
- Discovery runs at startup, then every `global.discovery_interval` seconds (optional, default 60).
- The token needs node-level `VM.Audit` + `VM.Config.Options` (reads the notes field), `VM.PowerMgmt`, plus `SDN.Audit` for IPAM backends.

### 5. Setup Docker Networks
Ensure your Traefik networks exist, or create them:
```bash
docker network create frontend
docker network create backend
```
Update `docker-compose.yml` if your network names differ.

### 6. Configure Traefik
Copy `traefik.config.yaml.example` to `traefik.config.yaml` and update the placeholders. Two kinds of router:
- **Each container domain** gets a router with the `wake-lxc-middleware` forwardAuth middleware — Traefik asks the middleware before forwarding.
- **`WAKE_DOMAIN`** gets a router pointing at the middleware **without** forwardAuth — it serves the `/starting` progress page.

*The example sets `trustForwardHeader: true` on the forwardAuth middleware — keep it.*

### 7. Deploy
```bash
docker compose up -d
docker compose logs -f wake-lxc
```

## 🐧 Manual Installation (No Docker)

For those who prefer running the middleware directly on a Proxmox host or LXC container without Docker, follow these steps:

### 1. Install Dependencies
```bash
apt update
apt install -y python3 python3-pip git vim curl ca-certificates tzdata
```

### 2. Clone the Repository
```bash
cd /opt/
git clone https://github.com/Deulfi/wake_lxc_middleware.git
cd wake_lxc_middleware
```

### 3. Install Python Dependencies
```bash
pip install --break-system-packages -r requirements.txt
```

### 4. Create Token File
Create a file to store your Proxmox API token securely:
```bash
echo -n "YOUR_PROXMOX_TOKEN_VALUE" > /opt/wake_lxc_middleware/token.txt
```

### 5. Configure Environment Variables
Edit `/etc/environment` and add the following:
```bash
# Logging
LOG_LEVEL=INFO

# Timezone
TZ="Europe/London"

# Proxmox Configuration
PROXMOX_HOST="proxhome.lan"
PROXMOX_NODE="proxmox"
PROXMOX_TOKEN_USER="svc-wake@pve"
PROXMOX_TOKEN_ID="wake"
PROXMOX_VERIFY_TLS=false
PROXMOX_TOKEN_VALUE="YOUR_ACTUAL_TOKEN_VALUE"
WAKE_DOMAIN="wake.sub.domain.name"
CONFIG_PATH="/opt/wake_lxc_middleware/config.yaml"
STATE_FILE="/opt/wake_lxc_middleware/state.json"
```
*Note: Update `PROXMOX_HOST`, `PROXMOX_NODE`, and other values to match your setup. `PROXMOX_TOKEN_VALUE` may be replaced by `PROXMOX_TOKEN_VALUE_FILE` pointing at the token file from step 4.*

### 6. Configure Traefik & Middleware
- Add Traefik routing rules to `/etc/traefik/conf.d/wakemiddleware.yaml` (see Traefik Configuration section above).
- Ensure your `config.yaml` is correctly placed in `/opt/wake_lxc_middleware/config.yaml`.

### 7. Create Systemd Service
Create a service file at `/etc/systemd/system/wake_lxc_middleware.service`:
```ini
[Unit]
Description=Wake LXC Middleware
After=network.target

[Service]
EnvironmentFile=/etc/environment
WorkingDirectory=/opt/wake_lxc_middleware
ExecStart=/usr/local/bin/uvicorn asgi_app:app --host 0.0.0.0 --port 8080 --log-level info
Restart=always
User=root

[Install]
WantedBy=multi-user.target
```

### 8. Start the Service
```bash
systemctl daemon-reload
systemctl enable --now wake_lxc_middleware
```

## 📖 Configuration Guide

### `config.yaml` Structure
| Field | Type | Description |
|-------|------|-------------|
| `global.shutdown_minutes` | int | Idle timeout (minutes) before graceful stop |
| `global.check_interval` | int | Watchdog poll interval in minutes (required) |
| `global.discovery_interval` | int | Optional; seconds between Proxmox-notes discovery passes (default 60) |
| `global.domain_template` | string | Optional; domain for discovered LXCs without `wake.domain`, e.g. `"{hostname}.sub.domain.name"` (LXC only — VMs must set `wake.domain`) |
| `containers[].vmid` | int | Proxmox container ID |
| `containers[].kind` | string | `lxc` or `qemu` (default `lxc`) |
| `containers[].domain` | string | FQDN that triggers this container |
| `containers[].domains` | list | Multiple FQDNs for one container |
| `containers[].port` | int | Backend port, used to build the backend URL |
| `containers[].host` | string | Backend host override |
| `containers[].backend` | string | Full backend URL override |
| `containers[].shutdown_minutes` | int | Graceful stop after N idle minutes |
| `containers[].stop_minutes` | int | Hard poweroff after N idle minutes (wins over `shutdown_minutes`) |

### `.env` Variables
| Variable | Description | Example |
|----------|-------------|---------|
| `PROXMOX_HOST` | Proxmox IP/Hostname | `192.168.1.100` |
| `PROXMOX_NODE` | Proxmox node name | `pve` |
| `PROXMOX_TOKEN_USER` | API token user | `svc-wake@pve` |
| `PROXMOX_TOKEN_ID` | API token ID | `wake` |
| `PROXMOX_TOKEN_VALUE` | API token secret | *(or via `PROXMOX_TOKEN_VALUE_FILE`)* |
| `PROXMOX_VERIFY_TLS` | Verify Proxmox SSL | `false` |
| `WAKE_DOMAIN` | Domain hosting `/starting` + `/status-stream` (no forwardAuth) | `wake.sub.domain.name` |
| `LOG_LEVEL` | Logging verbosity | `INFO` |
| `CONFIG_PATH` | Path to the application config file | `/opt/wake_lxc_middleware/config.yaml` |
| `STATE_FILE` | Stop-deadline persistence file | `state.json` |

### `wake.*` Labels (Proxmox notes)
| Label | Type | Description |
|-------|------|-------------|
| `wake.enable` | string | `true` opts the container into auto-discovery (required) |
| `wake.domain` | string | FQDN that triggers this container; wins over `global.domain_template` |
| `wake.port` | int | Backend port, combined with the resolved IP to build the backend URL |
| `wake.host` | string | Backend host override (skips IP resolution) |
| `wake.backend` | string | Full backend URL override (highest priority) |
| `wake.shutdown_minutes` | float | Graceful stop after N idle minutes |
| `wake.stop_minutes` | float | Hard poweroff after N idle minutes (wins over `wake.shutdown_minutes`) |

## 🔧 Troubleshooting

- **Container won't start**: Verify token permissions (`VM.PowerMgmt`) and network connectivity to Proxmox.
- **Watchdog cancels stop prematurely**: Increase `check_interval` (minutes) or ensure the container takes longer to boot than the interval.
- **Traefik shows blank/503 page**: The middleware never proxies and never returns 503 — it 302-redirects stopped-container requests to `WAKE_DOMAIN/starting`. Ensure `WAKE_DOMAIN` routes to the middleware **without** forwardAuth, and domain routers use forwardAuth with `trustForwardHeader: true`.
- **Circuit breaker open**: Check Proxmox API logs. The breaker resets after 5 minutes of consecutive failures.
- **Discovery finds no containers**: The token needs node-level `VM.Audit` + `VM.Config.Options`; IPAM backends additionally need `SDN.Audit`.

## 🏗️ Architecture
```
User Request → Traefik (forwardAuth → wake-lxc:8080)
                    ├→ 200 (running) → Traefik routes request to the container
                    └→ 302 (stopped) → WAKE_DOMAIN/starting (SSE progress)
```
1. Traefik asks `wake-lxc` to authenticate the request (forwardAuth)
2. `wake-lxc` matches the domain against `config.yaml` entries and Proxmox-notes discovery
3. If stopped, triggers Proxmox start + watchdog and 302-redirects to `WAKE_DOMAIN/starting`
4. If running, returns 200 and Traefik forwards the request to the container (the middleware never proxies)
5. Watchdog monitors status & cancels pending stops if externally stopped
6. Idle timer triggers graceful shutdown (or hard poweroff via `stop_minutes`) after inactivity; deadlines persist in `state.json`

## 📜 License
MIT License
