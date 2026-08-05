# Proxmox Token Setup via CLI (pveum)

Faster alternative to the GUI steps in the README. Run these on the Proxmox host itself (or any node with `pveum` available) — not inside the CT running wake_lxc_middleware.

## 1. Create a role with the minimum privileges this project needs
wake_lxc_middleware only ever calls `status/current` (read) and `status/start` / `status/shutdown` / `status/stop` (power). It needs exactly `VM.Audit` + `VM.PowerMgmt` — nothing more.

```bash
pveum role add WakeLXC --privs "VM.Audit,VM.PowerMgmt"
```

## 2. Create a service user
```bash
pveum user add svc-wake@pve --comment "wake_lxc_middleware service account"
```

## 3. Assign the role at root scope (propagates to all VMs/CTs)
```bash
pveum acl modify / --users svc-wake@pve --roles WakeLXC
```

## 4. Create the API token
Privilege Separation off = the token inherits the user's role directly, matching what wake_lxc_middleware expects (no separate token-level ACL needed).
```bash
pveum user token add svc-wake@pve wake --privsep 0
```
The token value is printed once. Copy it immediately — Proxmox does not store it and cannot show it again.

## 5. Verify
```bash
curl -k -H "Authorization: PVEAPIToken=svc-wake@pve!wake=YOUR_TOKEN_VALUE" \
  https://YOUR_PROXMOX_IP:8006/api2/json/nodes/YOUR_NODE/lxc
```
A JSON list of your containers confirms the token and permissions work.

## Adjusting later
```bash
pveum acl list /                       # see current assignments
pveum role modify WakeLXC --privs "..." # change the role's privileges
pveum user token remove svc-wake@pve wake  # revoke the token
```
