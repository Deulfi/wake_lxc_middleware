import asyncio
import logging
import os
import time
from typing import Dict, Optional, Any

import httpx
import yaml
from fastapi import FastAPI, Request
from fastapi.responses import HTMLResponse, JSONResponse, RedirectResponse, Response, StreamingResponse

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
LOG_LEVEL = os.getenv("LOG_LEVEL", "INFO").upper()
DEBUG_MODE = LOG_LEVEL == "DEBUG"

logging.basicConfig(
    level=getattr(logging, LOG_LEVEL, logging.INFO),
    format="%(asctime)s - %(levelname)-8s - %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
logger = logging.getLogger("wake_lxc_middleware")

if not DEBUG_MODE:
    for name in ["httpx", "httpcore"]:
        logging.getLogger(name).setLevel(logging.WARNING)

# ---------------------------------------------------------------------------
# Env var validation (with *_FILE / Docker-secrets support)
# ---------------------------------------------------------------------------
def get_secret(env_var: str) -> Optional[str]:
    """Read a value from <ENV_VAR>_FILE if set, else from <ENV_VAR> directly."""
    file_path = os.getenv(f"{env_var}_FILE")
    if file_path:
        if not os.path.exists(file_path):
            raise RuntimeError(f"{env_var}_FILE points to a non-existent file: {file_path}")
        with open(file_path, "r") as f:
            return f.read().strip()
    return os.getenv(env_var)


def validate_environment() -> dict:
    host = os.getenv("PROXMOX_HOST")
    node = os.getenv("PROXMOX_NODE")
    token_user = os.getenv("PROXMOX_TOKEN_USER")
    token_id = os.getenv("PROXMOX_TOKEN_ID")
    token_value = get_secret("PROXMOX_TOKEN_VALUE")
    wake_domain = os.getenv("WAKE_DOMAIN")

    missing = []
    if not host: missing.append("PROXMOX_HOST")
    if not node: missing.append("PROXMOX_NODE")
    if not token_user: missing.append("PROXMOX_TOKEN_USER")
    if not token_id: missing.append("PROXMOX_TOKEN_ID")
    if not token_value: missing.append("PROXMOX_TOKEN_VALUE (or PROXMOX_TOKEN_VALUE_FILE)")
    if not wake_domain: missing.append("WAKE_DOMAIN")

    if missing:
        raise RuntimeError("Missing required environment variables:\n  - " + "\n  - ".join(missing))

    logger.info("Environment variables validated OK.")
    return {
        "host": host,
        "node": node,
        "token_user": token_user,
        "token_id": token_id,
        "token_value": token_value,
        "verify_tls": os.getenv("PROXMOX_VERIFY_TLS", "false").lower() == "true",
        "wake_domain": wake_domain,
    }


PVE = validate_environment()
WAKE_DOMAIN = PVE["wake_domain"]

# ---------------------------------------------------------------------------
# Config file
# ---------------------------------------------------------------------------
config: Dict[str, Any] = {}
DOMAIN_TO_CONTAINER: Dict[str, dict] = {}


def load_config() -> dict:
    path = os.getenv("CONFIG_PATH", "config.yaml")
    with open(path, "r") as f:
        cfg = yaml.safe_load(f)
    if "global" not in cfg or "containers" not in cfg:
        raise ValueError("config.yaml must have 'global' and 'containers' sections")
    if "stop_minutes" not in cfg["global"]:
        raise ValueError("config.yaml: global.stop_minutes is required")
    if "check_interval" not in cfg["global"]:
        raise ValueError("config.yaml: global.check_interval is required")
    for i, c in enumerate(cfg["containers"]):
        if "vmid" not in c:
            raise ValueError(f"config.yaml: container {i} missing 'vmid'")
        if "domain" not in c and "domains" not in c:
            raise ValueError(f"config.yaml: container {i} missing 'domain' or 'domains'")
    return cfg


def build_domain_map():
    global DOMAIN_TO_CONTAINER
    DOMAIN_TO_CONTAINER = {}
    for c in config["containers"]:
        domains = c.get("domains") or [c["domain"]]
        for d in domains:
            DOMAIN_TO_CONTAINER[d] = c


def get_container_by_domain(domain: str) -> Optional[dict]:
    return DOMAIN_TO_CONTAINER.get(domain)

# ---------------------------------------------------------------------------
# Proxmox API client (shared, pooled — not recreated per request)
# ---------------------------------------------------------------------------
_proxmox_client: Optional[httpx.AsyncClient] = None


def get_proxmox_client() -> httpx.AsyncClient:
    global _proxmox_client
    if _proxmox_client is None:
        _proxmox_client = httpx.AsyncClient(
            base_url=f"https://{PVE['host']}:8006",
            headers={
                "Authorization": f"PVEAPIToken={PVE['token_user']}!{PVE['token_id']}={PVE['token_value']}",
                "Content-Type": "application/json",
            },
            timeout=10.0,
            verify=PVE["verify_tls"],
        )
    return _proxmox_client

# ---------------------------------------------------------------------------
# Circuit breaker (only trips on real API/connection failures)
# ---------------------------------------------------------------------------
circuit_breaker: Dict[str, float] = {}
FAILURE_TIMEOUT = 300  # seconds


def check_circuit_breaker(vmid: str) -> bool:
    """Returns True if OK to proceed, False if circuit is open."""
    ts = circuit_breaker.get(vmid)
    if ts is None:
        return True
    if time.time() - ts >= FAILURE_TIMEOUT:
        del circuit_breaker[vmid]
        return True
    return False


def record_failure(vmid: str):
    circuit_breaker[vmid] = time.time()


def reset_failures(vmid: str):
    circuit_breaker.pop(vmid, None)

# ---------------------------------------------------------------------------
# Proxmox operations
# ---------------------------------------------------------------------------
async def check_container_status(vmid: str, kind: str = "lxc") -> bool:
    """True if running. Only records circuit-breaker failures on real errors,
    never on a legitimate 'stopped' status."""
    if not check_circuit_breaker(vmid):
        logger.warning(f"VMID {vmid}: circuit breaker open, skipping status check.")
        return False
    try:
        client = get_proxmox_client()
        resp = await client.get(f"/api2/json/nodes/{PVE['node']}/{kind}/{vmid}/status/current")
        resp.raise_for_status()
        status = resp.json().get("data", {}).get("status")
        if status == "running":
            reset_failures(vmid)
            return True
        logger.info(f"VMID {vmid} is {status}.")
        return False
    except Exception as e:
        logger.error(f"VMID {vmid}: status check failed: {e}")
        record_failure(vmid)
        return False


async def start_container(vmid: str, kind: str = "lxc") -> bool:
    try:
        client = get_proxmox_client()
        resp = await client.post(f"/api2/json/nodes/{PVE['node']}/{kind}/{vmid}/status/start", json={})
        resp.raise_for_status()
        logger.info(f"VMID {vmid}: start command sent.")
        reset_failures(vmid)
        return True
    except Exception as e:
        logger.error(f"VMID {vmid}: start failed: {e}")
        record_failure(vmid)
        return False


async def shutdown_container(vmid: str, kind: str = "lxc", stop_mode: str = "shutdown") -> bool:
    try:
        client = get_proxmox_client()
        resp = await client.post(f"/api2/json/nodes/{PVE['node']}/{kind}/{vmid}/status/{stop_mode}", json={})
        resp.raise_for_status()
        logger.info(f"VMID {vmid}: shutdown command sent.")
        return True
    except Exception as e:
        logger.error(f"VMID {vmid}: shutdown failed: {e}")
        return False


async def wait_for_backend_ready(backend_url: Optional[str], timeout: int = 90) -> bool:
    """Poll the real application's own port, not just the Proxmox 'running' state.
    A container being 'running' only means the OS booted -- it says nothing about
    whether the app inside has finished starting and is actually listening yet."""
    if not backend_url:
        return True  # no backend configured for this container, skip probing
    deadline = time.time() + timeout
    async with httpx.AsyncClient(timeout=2.0) as probe:
        while time.time() < deadline:
            try:
                resp = await probe.get(backend_url)
                if resp.status_code < 500:
                    return True
            except Exception:
                pass
            await asyncio.sleep(2)
    logger.warning(f"Backend {backend_url} did not become ready within {timeout}s.")
    return False

# ---------------------------------------------------------------------------
# Per-vmid start lock (prevents duplicate concurrent start calls)
# ---------------------------------------------------------------------------
_start_locks: Dict[str, asyncio.Lock] = {}


def get_start_lock(vmid: str) -> asyncio.Lock:
    if vmid not in _start_locks:
        _start_locks[vmid] = asyncio.Lock()
    return _start_locks[vmid]

# ---------------------------------------------------------------------------
# Fixed-duration stop timer + external-stop watchdog
#
# NOTE: both _run() and _loop() check "is this dict entry still MY task"
# before popping it. Without that check, an old (already-cancelled) task's
# `finally` block can race with a newer task that just registered itself,
# deleting the NEW task's registration and silently orphaning it -- which
# is what caused the double-stop / double-timer bug during testing.
# ---------------------------------------------------------------------------
active_timers: Dict[str, asyncio.Task] = {}
watchdog_tasks: Dict[str, asyncio.Task] = {}


def schedule_stop(container: dict):
    vmid = str(container["vmid"])
    if vmid in active_timers:
        active_timers[vmid].cancel()

    stop_minutes = container.get("stop_minutes", config["global"]["stop_minutes"])

    async def _run():
        this_task = asyncio.current_task()
        try:
            await asyncio.sleep(stop_minutes * 60)
            logger.info(f"VMID {vmid}: stop timer fired.")
            if await check_container_status(vmid, container.get("kind", "lxc")):
                await shutdown_container(vmid, container.get("kind", "lxc"), container.get("stop_mode", "shutdown"))
            else:
                logger.info(f"VMID {vmid}: already stopped, nothing to do.")
        except asyncio.CancelledError:
            pass
        finally:
            if active_timers.get(vmid) is this_task:
                active_timers.pop(vmid, None)

    active_timers[vmid] = asyncio.create_task(_run())
    logger.info(f"VMID {vmid}: stop scheduled in {stop_minutes} min.")


def start_watchdog(container: dict):
    vmid = str(container["vmid"])
    if vmid in watchdog_tasks:
        watchdog_tasks[vmid].cancel()

    check_interval = container.get("check_interval", config["global"]["check_interval"])
    interval_sec = check_interval * 60

    async def _loop():
        this_task = asyncio.current_task()
        try:
            while True:
                await asyncio.sleep(interval_sec)
                if vmid not in active_timers:
                    break  # stop timer already fired/removed, nothing to watch
                if not await check_container_status(vmid, container.get("kind", "lxc")):
                    logger.info(f"VMID {vmid}: stopped externally, cancelling pending stop timer.")
                    task = active_timers.pop(vmid, None)
                    if task:
                        task.cancel()
                    break
        except asyncio.CancelledError:
            pass
        finally:
            if watchdog_tasks.get(vmid) is this_task:
                watchdog_tasks.pop(vmid, None)

    watchdog_tasks[vmid] = asyncio.create_task(_loop())

# ---------------------------------------------------------------------------
# FastAPI app
# ---------------------------------------------------------------------------
app = FastAPI(title="wake_lxc_middleware")


@app.on_event("startup")
async def on_startup():
    global config
    config = load_config()
    build_domain_map()
    logger.info(f"Loaded {len(config['containers'])} container(s), {len(DOMAIN_TO_CONTAINER)} domain(s).")
    logger.info(f"WAKE_DOMAIN = {WAKE_DOMAIN} (must be routed WITHOUT forwardAuth in Traefik)")


@app.on_event("shutdown")
async def on_shutdown():
    for t in list(active_timers.values()):
        t.cancel()
    for t in list(watchdog_tasks.values()):
        t.cancel()
    if _proxmox_client:
        await _proxmox_client.aclose()


@app.get("/auth")
async def forward_auth(request: Request):
    """Traefik ForwardAuth endpoint. Gated by Traefik on protected routers only."""
    host = request.headers.get("x-forwarded-host") or request.headers.get("host", "")
    container = get_container_by_domain(host)

    if not container:
        return JSONResponse(status_code=404, content={"detail": f"No container mapped for host: {host}"})

    vmid = str(container["vmid"])
    kind = container.get("kind", "lxc")

    if await check_container_status(vmid, kind):
        schedule_stop(container)      # extend the window on every access
        start_watchdog(container)
        return JSONResponse(status_code=200, content={})

    # Not running -- start it (locked, so concurrent requests don't double-start)
    lock = get_start_lock(vmid)
    if not lock.locked():
        async with lock:
            if not await check_container_status(vmid, kind):
                await start_container(vmid, kind)

    schedule_stop(container)
    start_watchdog(container)

    target = f"https://{WAKE_DOMAIN}/starting?target={host}"
    return RedirectResponse(url=target, status_code=302)


@app.get("/starting")
async def starting_page(target: str):
    """Served on WAKE_DOMAIN only -- never behind forwardAuth."""
    html = f"""<!DOCTYPE html>
<html><head><title>Starting service</title></head>
<body style="font-family:sans-serif;text-align:center;margin-top:15%;">
  <h2>Starting container...</h2>
  <p id="status">Checking status...</p>
  <script>
    const evtSource = new EventSource('/status-stream?target={target}');
    evtSource.onmessage = (event) => {{
      document.getElementById('status').innerText = event.data;
      if (event.data === 'ready') {{
        evtSource.close();
        window.location.href = 'https://{target}/';
      }}
    }};
    evtSource.onerror = () => {{
      document.getElementById('status').innerText = 'Connection lost, retrying...';
    }};
  </script>
</body></html>"""
    return HTMLResponse(content=html)


@app.get("/status-stream")
async def status_stream(target: str):
    """Served on WAKE_DOMAIN only -- never behind forwardAuth."""
    container = get_container_by_domain(target)
    if not container:
        return StreamingResponse(iter([]), media_type="text/event-stream")

    vmid = str(container["vmid"])
    kind = container.get("kind", "lxc")
    backend_url = container.get("backend")

    async def gen():
        while True:
            if await check_container_status(vmid, kind):
                if await wait_for_backend_ready(backend_url):
                    yield "data: ready\n\n"
                    break
                yield "data: waiting for service to respond...\n\n"
            else:
                yield "data: starting container...\n\n"
            await asyncio.sleep(2)

    return StreamingResponse(gen(), media_type="text/event-stream")


@app.get("/healthz")
async def healthz():
    return Response(content="ok", media_type="text/plain")


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="0.0.0.0", port=8080)
