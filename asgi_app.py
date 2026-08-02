import asyncio
import logging
import os
import time
from typing import Optional, Dict, Any

import httpx
import yaml
from fastapi import FastAPI, Request
from fastapi.responses import HTMLResponse, StreamingResponse, JSONResponse

# Configure logging to stdout (visible in journalctl for systemd, or terminal)
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)

app = FastAPI(title="wake_lxc_middleware")

# Global state
config: Dict[str, Any] = {}
active_timers: Dict[str, asyncio.Task] = {}
watchdog_tasks: Dict[str, asyncio.Task] = {}
circuit_breaker: Dict[str, float] = {}
FAILURE_TIMEOUT = 300  # seconds

def load_proxmox_config() -> dict:
    config_path = os.getenv("CONFIG_PATH", "config.yaml")
    with open(config_path, "r") as f:
        return yaml.safe_load(f)

def validate_config(cfg: dict) -> None:
    if "global" not in cfg or "containers" not in cfg:
        raise ValueError("Invalid config: missing global or containers section")
    if "stop_minutes" not in cfg["global"]:
        raise ValueError("Invalid config: global.stop_minutes required")
    if "check_interval" not in cfg["global"]:
        raise ValueError("Invalid config: global.check_interval required")
    for i, container in enumerate(cfg.get("containers", [])):
        if "vmid" not in container or "domain" not in container:
            raise ValueError(f"Invalid config: container {i} missing 'vmid' or 'domain'")

def get_container_by_domain(domain: str) -> Optional[dict]:
    for c in config.get("containers", []):
        if c.get("domain") == domain:
            return c
    return None

def get_proxmox_headers() -> dict:
    token_user = os.getenv("PROXMOX_TOKEN_USER", "")
    token_id = os.getenv("PROXMOX_TOKEN_ID", "")
    token_value = os.getenv("PROXMOX_TOKEN_VALUE", "")
    return {
        "Authorization": f"PVEAPIToken={token_user}!{token_id}={token_value}",
        "Content-Type": "application/json"
    }

def get_proxmox_client() -> httpx.AsyncClient:
    return httpx.AsyncClient(
        base_url=f"https://{os.getenv('PROXMOX_HOST', '')}:8006",
        headers=get_proxmox_headers(),
        timeout=10.0,
        verify=False
    )

def check_circuit_breaker(vmid: str) -> bool:
    if vmid in circuit_breaker:
        if time.time() - circuit_breaker[vmid] < FAILURE_TIMEOUT:
            return False
        else:
            del circuit_breaker[vmid]
    return True

def record_container_failure(vmid: str):
    circuit_breaker[vmid] = time.time()

def reset_container_failures(vmid: str):
    if vmid in circuit_breaker:
        del circuit_breaker[vmid]

async def check_container_status(vmid: str, kind: str = "lxc") -> bool:
    if not check_circuit_breaker(vmid):
        logger.warning(f"Container {vmid} is in circuit breaker state, skipping status check.")
        return False
    try:
        async with get_proxmox_client() as client:
            resp = await client.get(f"/api2/json/nodes/{os.getenv('PROXMOX_NODE')}/{kind}/{vmid}/status/current")
            if resp.status_code == 200:
                data = resp.json()
                status = data.get("data", {}).get("status", "")
                if status == "running":
                    reset_container_failures(vmid)
                    logger.info(f"Container {vmid} is running.")
                    return True
                else:
                    logger.info(f"Container {vmid} is {status}.")
                    return False
    except Exception as e:
        logger.error(f"Failed to check status for container {vmid}: {e}")
        record_container_failure(vmid)
        return False
    return False

async def start_container(vmid: str, kind: str = "lxc") -> bool:
    try:
        logger.info(f"Starting container {vmid}...")
        async with get_proxmox_client() as client:
            resp = await client.post(f"/api2/json/nodes/{os.getenv('PROXMOX_NODE')}/{kind}/{vmid}/status/start", json={})
            if resp.status_code == 200:
                logger.info(f"Container {vmid} start command sent successfully.")
                return True
            else:
                logger.error(f"Failed to start container {vmid}: {resp.status_code} - {resp.text}")
                record_container_failure(vmid)
                return False
    except Exception as e:
        logger.error(f"Exception while starting container {vmid}: {e}")
        record_container_failure(vmid)
        return False

async def shutdown_container(vmid: str, kind: str = "lxc", stop_mode: str = "shutdown") -> bool:
    try:
        logger.info(f"Shutting down container {vmid} using mode '{stop_mode}'...")
        async with get_proxmox_client() as client:
            resp = await client.post(f"/api2/json/nodes/{os.getenv('PROXMOX_NODE')}/{kind}/{vmid}/status/{stop_mode}", json={})
            if resp.status_code == 200:
                logger.info(f"Container {vmid} shutdown command sent successfully.")
                return True
            else:
                logger.error(f"Failed to shutdown container {vmid}: {resp.status_code} - {resp.text}")
                record_container_failure(vmid)
                return False
    except Exception as e:
        logger.error(f"Exception while shutting down container {vmid}: {e}")
        record_container_failure(vmid)
        return False

def schedule_stop(container: dict):
    vmid = str(container['vmid'])
    if vmid in active_timers:
        logger.info(f"Cancelling existing stop timer for container {vmid}.")
        active_timers[vmid].cancel()
    
    stop_minutes = container.get('stop_minutes', config['global']['stop_minutes'])
    logger.info(f"Scheduling stop for container {vmid} in {stop_minutes} minutes.")
    
    async def stop_after_delay():
        try:
            await asyncio.sleep(stop_minutes * 60)
            logger.info(f"Stop timer fired for container {vmid}. Checking status...")
            if await check_container_status(vmid, container.get('kind', 'lxc')):
                logger.info(f"Container {vmid} is still running. Initiating shutdown.")
                await shutdown_container(vmid, container.get('kind', 'lxc'), container.get('stop_mode', 'shutdown'))
            else:
                logger.info(f"Container {vmid} is not running. Skipping shutdown.")
        except asyncio.CancelledError:
            logger.info(f"Stop timer for container {vmid} was cancelled.")
        finally:
            active_timers.pop(vmid, None)

    task = asyncio.create_task(stop_after_delay())
    active_timers[vmid] = task

def start_watchdog(container: dict):
    vmid = str(container['vmid'])
    if vmid in watchdog_tasks:
        logger.info(f"Cancelling existing watchdog for container {vmid}.")
        watchdog_tasks[vmid].cancel()
        
    check_interval = container.get('check_interval', config['global']['check_interval'])
    # Convert check_interval from minutes to seconds for asyncio.sleep
    check_interval_seconds = check_interval * 60
    
    async def watchdog_loop():
        logger.info(f"Watchdog started for container {vmid}, checking every {check_interval} minutes.")
        try:
            while True:
                await asyncio.sleep(check_interval_seconds)
                if vmid not in active_timers:
                    logger.info(f"Watchdog for container {vmid} cancelled (timer finished or removed).")
                    break
                is_running = await check_container_status(vmid, container.get('kind', 'lxc'))
                if not is_running:
                    logger.info(f"Container {vmid} is down. Cancelling stop timer.")
                    if vmid in active_timers:
                        active_timers[vmid].cancel()
                        active_timers.pop(vmid, None)
                    break
                else:
                    logger.info(f"Watchdog check for container {vmid}: Running.")
        except asyncio.CancelledError:
            logger.info(f"Watchdog for container {vmid} cancelled.")

    task = asyncio.create_task(watchdog_loop())
    watchdog_tasks[vmid] = task

@app.on_event("startup")
async def startup_event():
    global config
    logger.info("Application starting up...")
    config = load_proxmox_config()
    validate_config(config)
    logger.info("Configuration loaded and validated successfully.")

@app.on_event("shutdown")
async def shutdown_event():
    logger.info("Application shutting down...")
    for task in list(active_timers.values()):
        task.cancel()
    for task in list(watchdog_tasks.values()):
        task.cancel()
    logger.info("All tasks cancelled. Shutdown complete.")

@app.get("/auth")
async def forward_auth(request: Request):
    """Traefik ForwardAuth endpoint"""
    host = request.headers.get("x-forwarded-host") or request.headers.get("host", "")
    logger.info(f"Auth request received for host: {host}")
    
    container = get_container_by_domain(host)
    
    if not container:
        logger.warning(f"Container not found for host: {host}")
        return JSONResponse(status_code=404, content={"detail": "Container not found"})

    vmid = str(container['vmid'])
    logger.info(f"Checking status for container {vmid}...")
    is_running = await check_container_status(vmid, container.get('kind', 'lxc'))
    
    if not is_running:
        logger.info(f"Container {vmid} is not running. Starting it now.")
        await start_container(vmid, container.get('kind', 'lxc'))
    
    # Always schedule stop timer and watchdog on access, regardless of initial state
    schedule_stop(container)
    start_watchdog(container)
    
    if is_running:
        logger.info(f"Container {vmid} is running. Allowing access.")
        return JSONResponse(status_code=200, content={})
    
    # Return HTML page with SSE client to show progress
    html = f"""
    <!DOCTYPE html>
    <html>
    <head><title>Starting Container {vmid}</title></head>
    <body>
        <h1>Starting container...</h1>
        <p id="status">Checking status...</p>
        <script>
            const evtSource = new EventSource('/status/{host}');
            evtSource.onmessage = function(event) {{
                document.getElementById('status').innerText = event.data;
                if (event.data === 'Container is running.') {{
                    evtSource.close();
                    setTimeout(() => window.location.reload(), 2000);
                }}
            }};
            evtSource.onerror = function() {{
                document.getElementById('status').innerText = 'Connection lost. Please refresh.';
            }};
        </script>
    </body>
    </html>
    """
    return HTMLResponse(content=html, status_code=503)

@app.get("/status/{host}")
async def status_stream(host: str):
    """SSE endpoint for status updates"""
    container = get_container_by_domain(host)
    if not container:
        logger.warning(f"Status stream requested for unknown host: {host}")
        return StreamingResponse(iter([]), media_type="text/event-stream")
        
    vmid = str(container['vmid'])
    logger.info(f"Status stream started for container {vmid} (host: {host})")
    
    async def event_generator():
        while True:
            is_running = await check_container_status(vmid, container.get('kind', 'lxc'))
            if is_running:
                yield f"data: Container is running.\n\n"
                logger.info(f"Container {vmid} is now running. Closing status stream.")
                break
            else:
                yield f"data: Container is starting...\n\n"
            await asyncio.sleep(2)
            
    return StreamingResponse(event_generator(), media_type="text/event-stream")

@app.get("/healthz")
async def healthz():
    return JSONResponse(status_code=200, content={"status": "ok"})

if __name__ == '__main__':
    import uvicorn
    uvicorn.run(app, host="0.0.0.0", port=8000)
