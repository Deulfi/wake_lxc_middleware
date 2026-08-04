import asyncio
import json
import logging
import os
import time
from collections import deque
from typing import Dict, Optional, Any

import httpx
import yaml
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, RedirectResponse, Response, StreamingResponse

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


def get_container_by_vmid(vmid: str) -> Optional[dict]:
    for c in config.get("containers", []):
        if str(c["vmid"]) == str(vmid):
            return c
    return None

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


# ---------------------------------------------------------------------------
# Status events -- richer SSE messages instead of a generic "starting..."
# string, adopted from the original repo's approach.
# ---------------------------------------------------------------------------
status_events: Dict[str, dict] = {}
status_lock = asyncio.Lock()
container_start_times: Dict[str, float] = {}


async def emit_status_event(domain: str, message: str, level: str = "info"):
    async with status_lock:
        if domain not in status_events:
            status_events[domain] = {"queue": deque(maxlen=50)}
        status_events[domain]["queue"].append({"message": message, "level": level})


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
_stop_deadlines: Dict[str, float] = {}  # vmid -> unix timestamp, mirrors active_timers for persistence

STATE_FILE = os.getenv("STATE_FILE", "state.json")
_state_lock = asyncio.Lock()


async def save_state():
    """Persist pending stop deadlines to disk so a restart doesn't strand
    running containers with no stop timer (they'd otherwise stay up forever
    until someone happens to hit /auth again)."""
    async with _state_lock:
        try:
            tmp = STATE_FILE + ".tmp"
            with open(tmp, "w") as f:
                json.dump(_stop_deadlines, f)
            os.replace(tmp, STATE_FILE)
        except Exception as e:
            logger.error(f"Failed to save state file: {e}")


def load_state() -> Dict[str, float]:
    if not os.path.exists(STATE_FILE):
        return {}
    try:
        with open(STATE_FILE, "r") as f:
            return json.load(f)
    except Exception as e:
        logger.warning(f"State file unreadable ({e}), starting fresh.")
        return {}


def schedule_stop(container: dict, initial_delay_seconds: Optional[float] = None):
    vmid = str(container["vmid"])
    if vmid in active_timers:
        active_timers[vmid].cancel()

    stop_minutes = container.get("stop_minutes", config["global"]["stop_minutes"])
    delay = initial_delay_seconds if initial_delay_seconds is not None else stop_minutes * 60
    _stop_deadlines[vmid] = time.time() + delay
    asyncio.create_task(save_state())

    async def _run():
        this_task = asyncio.current_task()
        try:
            await asyncio.sleep(delay)
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
                _stop_deadlines.pop(vmid, None)
                await save_state()

    active_timers[vmid] = asyncio.create_task(_run())
    logger.info(f"VMID {vmid}: stop scheduled in {delay/60:.1f} min.")


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
    logger.info(f"WAKE_DOMAIN = {WAKE_DOMAIN}")

    restored = load_state()
    now = time.time()
    for vmid, deadline in restored.items():
        container = get_container_by_vmid(vmid)
        if not container:
            continue  # container removed from config since last run
        remaining = deadline - now
        if remaining <= 0:
            logger.info(f"VMID {vmid}: stop was overdue while offline, stopping now.")
            asyncio.create_task(shutdown_container(
                vmid, container.get("kind", "lxc"), container.get("stop_mode", "shutdown")
            ))
        else:
            logger.info(f"VMID {vmid}: resuming stop timer, {remaining/60:.1f} min remaining.")
            schedule_stop(container, initial_delay_seconds=remaining)
        start_watchdog(container)


@app.on_event("shutdown")
async def on_shutdown():
    # NOTE: we deliberately do NOT clear _stop_deadlines here -- state.json
    # should still reflect pending stops so they resume correctly on the
    # next startup. Only cancel the in-memory asyncio tasks.
    for t in list(active_timers.values()):
        t.cancel()
    for t in list(watchdog_tasks.values()):
        t.cancel()
    await save_state()
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
    vmid_key = vmid
    container_start_times[vmid_key] = time.time()
    await emit_status_event(host, "Container is stopped, starting now...")

    lock = get_start_lock(vmid)
    if not lock.locked():
        async with lock:
            if not await check_container_status(vmid, kind):
                if await start_container(vmid, kind):
                    await emit_status_event(host, "Start command sent successfully.")
                else:
                    await emit_status_event(host, "Failed to send start command -- check Proxmox connectivity.", "error")

    schedule_stop(container)
    start_watchdog(container)

    target = f"https://{WAKE_DOMAIN}/starting?target={host}"
    return RedirectResponse(url=target, status_code=302)


@app.get("/starting")
async def starting_page(target: str):
    """Served on WAKE_DOMAIN only -- never behind forwardAuth."""
    html = f"""<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Service Starting</title>
    <style>
        * {{ margin: 0; padding: 0; box-sizing: border-box; }}
        body {{
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            min-height: 100vh;
            display: flex;
            justify-content: center;
            align-items: center;
            text-align: center;
            padding: 20px;
        }}
        .container {{
            background: rgba(255, 255, 255, 0.1);
            backdrop-filter: blur(10px);
            border-radius: 16px;
            padding: 32px 28px;
            box-shadow: 0 8px 32px rgba(31, 38, 135, 0.37);
            border: 1px solid rgba(255, 255, 255, 0.18);
            max-width: 420px;
            width: 90%;
        }}
        .logo {{ font-size: 3rem; margin-bottom: 12px; animation: pulse 2s ease-in-out infinite alternate; }}
        @keyframes pulse {{ from {{ transform: scale(1); }} to {{ transform: scale(1.08); }} }}
        h1 {{ font-size: 1.5rem; margin-bottom: 12px; font-weight: 600; }}
        .spinner {{
            border: 3px solid rgba(255, 255, 255, 0.3);
            border-radius: 50%;
            border-top: 3px solid white;
            width: 40px; height: 40px;
            animation: spin 1s linear infinite;
            margin: 20px auto;
        }}
        @keyframes spin {{ 0% {{ transform: rotate(0deg); }} 100% {{ transform: rotate(360deg); }} }}
        .status-text {{ font-size: 1.05rem; line-height: 1.4; opacity: 0.95; margin: 16px 0; min-height: 28px; }}
        .timer {{ font-size: 0.85rem; opacity: 0.7; margin: 8px 0; }}
        .error {{ background: rgba(255, 107, 107, 0.2); border: 1px solid rgba(255, 107, 107, 0.3); }}
    </style>
</head>
<body>
  <div class="container" id="main-container">
    <div class="logo">🚀</div>
    <h1>{target}</h1>
    <div class="spinner"></div>
    <div class="status-text" id="status-text">Checking status...</div>
    <div class="timer" id="timer">Elapsed: 0s</div>
  </div>
  <script>
    const startedAt = Date.now();
    const statusText = document.getElementById('status-text');
    const timerEl = document.getElementById('timer');
    const container = document.getElementById('main-container');

    setInterval(() => {{
      timerEl.textContent = `Elapsed: ${{Math.floor((Date.now() - startedAt) / 1000)}}s`;
    }}, 1000);

    const evtSource = new EventSource('/status-stream?target={target}');
    evtSource.onmessage = (event) => {{
      const data = JSON.parse(event.data);
      if (data.message) statusText.textContent = data.message;
      if (data.level === 'error') container.classList.add('error');
      if (data.level === 'ready') {{
        evtSource.close();
        window.location.href = 'https://{target}/';
      }}
    }};
    evtSource.onerror = () => {{
      statusText.textContent = 'Connection lost, retrying...';
    }};
  </script>
</body></html>"""
    return Response(content=html, media_type="text/html", headers={"Cache-Control": "no-store"})


@app.get("/status-stream")
async def status_stream(target: str):
    """Served on WAKE_DOMAIN only -- never behind forwardAuth."""
    container = get_container_by_domain(target)
    if not container:
        return StreamingResponse(iter([]), media_type="text/event-stream")

    vmid = str(container["vmid"])
    kind = container.get("kind", "lxc")
    backend_url = container.get("backend")
    last_sent = 0

    async def gen():
        nonlocal last_sent
        while True:
            async with status_lock:
                queued = list(status_events.get(target, {}).get("queue", []))
            for event in queued[last_sent:]:
                yield f"data: {json.dumps(event)}\n\n"
            last_sent = len(queued)

            if await check_container_status(vmid, kind):
                if await wait_for_backend_ready(backend_url, timeout=2):
                    yield f"data: {json.dumps({'message': 'Ready! Redirecting...', 'level': 'ready'})}\n\n"
                    break
                else:
                    yield f"data: {json.dumps({'message': 'Container running, waiting for the app to respond...'})}\n\n"
            await asyncio.sleep(2)

    return StreamingResponse(gen(), media_type="text/event-stream", headers={"Cache-Control": "no-cache"})


@app.get("/healthz")
async def healthz():
    return Response(content="ok", media_type="text/plain")


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="0.0.0.0", port=8080)
