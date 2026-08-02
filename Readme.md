# wake_lxc_middleware: Proxmox Container On-Demand Auto Start Service

**wake_lxc_middleware** is a lightweight reverse proxy and status monitor that automatically starts Proxmox LXC containers based on incoming traffic. It saves resources while maintaining seamless user access through a smart proxy layer.

## ✨ Features
- **Auto-Wake**: Starts stopped containers on first request via Traefik ForwardAuth
- **Real-time Status**: SSE-based progress page while containers boot
- **Watchdog & Circuit Breaker**: Prevents double-stops and handles API failures gracefully
- **Traefik Native**: Designed to work seamlessly with Traefik v2/v3
- **Secure**: Supports Docker secrets and environment-based credential management

## 📋 Prerequisites
- Docker & Docker Compose
- Proxmox VE (8.x)
- Traefik Reverse Proxy
- LXC Containers running your services

## 🚀 Quick Start

### 1. Clone & Setup
```bash
git clone https://github.com/Deulfi/wake_lxc_middleware.git
cd wake_lxc_middleware
```

### 2. Create Proxmox API Token
1. Create a dedicated user in Proxmox (`Datacenter → Permissions → Users`)
2. Assign minimal permissions: `VM.PowerMgmt` and `VM.Audit`
3. Generate an API Token (`Datacenter → Permissions → API Tokens`)
   - **Uncheck** "Privilege Separation" for simpler inheritance
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
  stop_minutes: 10
  check_interval: 30

containers:
  - vmid: 105
    kind: lxc
    domain: docmost.sub.domain.name
    stop_minutes: 60
    stop_mode: shutdown
    check_interval: 15
  
  - vmid: 106
    kind: lxc
    domain: n8n.sub.domain.name
    stop_minutes: 120
    stop_mode: shutdown
```
*Note: Each container should have a unique `domain` that matches your Traefik router.*

### 5. Setup Docker Networks
Ensure your Traefik networks exist, or create them:
```bash
docker network create frontend
docker network create backend
```
Update `docker-compose.yml` if your network names differ.

### 6. Configure Traefik
Add the following routers and services to your Traefik dynamic config (`traefik.config.yaml`):
```yaml
routers:
    docmost:
      entryPoints:
        - "https"
      rule: "Host(`docmost.sub.domain.name`)"
      middlewares:
        - default-headers
        - https-redirectscheme
      tls: {}
      service: wake-docmost
    
    photos:
      entryPoints:
        - "https"
      rule: "Host(`photos.sub.domain.name`)"
      middlewares:
        - default-headers
        - https-redirectscheme
      tls: {}
      service: wake-photos

    n8n:
      entryPoints:
        - "https"
      rule: "Host(`n8n.sub.domain.name`)"
      middlewares:
        - default-headers
        - https-redirectscheme
      tls: {}
      service: wake-n8n
    
services:
    wake-docmost:
      loadBalancer:
        servers:
          - url: "http://wake-lxc:8080"
          - url: "http://<CONTAINER_IP>:<PORT>"
        passHostHeader: true
        healthCheck:
          path: "/healthz"
          interval: "10s"
          timeout: "5s"
    wake-photos:
      loadBalancer:
        servers:
          - url: "http://wake-lxc:8080"
          - url: "http://<CONTAINER_IP>:<PORT>"
        passHostHeader: true
        healthCheck:
          path: "/healthz"
          interval: "10s"
          timeout: "5s"
    wake-n8n:
      loadBalancer:
        servers:
          - url: "http://wake-lxc:8080"
          - url: "http://<CONTAINER_IP>:<PORT>"
        passHostHeader: true
        healthCheck:
          path: "/healthz"
          interval: "10s"
          timeout: "5s"

middlewares:
    https-redirectscheme:
      redirectScheme:
        scheme: https
        permanent: true
        
    default-headers:
      headers:
        frameDeny: true
        browserXssFilter: true
        contentTypeNosniff: true
        forceSTSHeader: true
        stsIncludeSubdomains: true
        stsPreload: true
        stsSeconds: 31536000
        customFrameOptionsValue: "SAMEORIGIN"
        referrerPolicy: "strict-origin-when-cross-origin"
        customRequestHeaders:
          X-Forwarded-Proto: "https"
          X-Forwarded-Port: "443"
        customResponseHeaders:
          Server: ""
          X-Powered-By: ""
```
*Ensure `trustForwardHeader: true` is set in Traefik if using forward auth.*

### 7. Deploy
```bash
docker compose up -d
docker compose logs -f wake-lxc
```

## 📖 Configuration Guide

### `config.yaml` Structure
| Field | Type | Description |
|-------|------|-------------|
| `global.stop_minutes` | int | Default idle timeout before shutdown |
| `global.check_interval` | int | Seconds between watchdog status checks |
| `containers[].vmid` | int | Proxmox container ID |
| `containers[].kind` | string | `lxc` or `qemu` |
| `containers[].domain` | string | FQDN that triggers this container |
| `containers[].stop_minutes` | int | Override global idle timeout |
| `containers[].stop_mode` | string | `shutdown` (graceful) or `stop` (force) |
| `containers[].check_interval` | int | Override global check interval |

### `.env` Variables
| Variable | Description | Example |
|----------|-------------|---------|
| `PROXMOX_HOST` | Proxmox IP/Hostname | `192.168.1.100` |
| `PROXMOX_NODE` | Proxmox node name | `pve` |
| `PROXMOX_TOKEN_USER` | API token user | `svc-wake@pve` |
| `PROXMOX_TOKEN_ID` | API token ID | `wake` |
| `PROXMOX_TOKEN_VALUE` | API token secret | *(loaded from file or env)* |
| `PROXMOX_VERIFY_TLS` | Verify Proxmox SSL | `false` |
| `LOG_LEVEL` | Logging verbosity | `INFO` |

## 🔧 Troubleshooting

- **Container won't start**: Verify token permissions (`VM.PowerMgmt`) and network connectivity to Proxmox.
- **Watchdog cancels stop prematurely**: Increase `check_interval` or ensure the container takes longer to boot than the interval.
- **Traefik shows 503/blank page**: Ensure Traefik is configured with `forwardAuth` and `trustForwardHeader: true`. The middleware returns a `503` with an SSE progress page during boot.
- **Circuit breaker open**: Check Proxmox API logs. The breaker resets after 5 minutes of consecutive failures.

## 🏗️ Architecture
```
User Request → Traefik (Auth/Router) → wake-lxc:8080 → Proxmox API → LXC Container
```
1. Traefik forwards request to `wake-lxc`
2. `wake-lxc` checks `config.yaml` for domain match
3. If stopped, triggers Proxmox start & returns SSE progress page
4. If running, proxies request to backend
5. Watchdog monitors status & cancels pending stops if externally stopped
6. Idle timer triggers graceful shutdown after inactivity

## 📜 License
MIT License
