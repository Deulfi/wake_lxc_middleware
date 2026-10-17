***PROXMOX SETUP***

# Proxmox API token (for starting/stopping guests)
```using CT 111 as an example replace with your actual ID```
### Create service user, custom role, scoped token (CT 111 only)

1.  Create a dedicated local user

```
pveum user add svc-wake@pve --comment "Wake-on-request service"
```

2.  Create a minimal custom role (audit+monitor+power only)

```
pveum role add WakeRole --privs "VM.Audit,VM.Monitor,VM.PowerMgmt"
```

3.  Grant that role ONLY on CT 111

```
pveum aclmod /vms/111 --user svc-wake@pve --role WakeRole
```

4.  Create a privilege-separated API token for that user

### IMP(copy the returned 'value' somewhere safe; it will be shown only once)

```
pveum user token add svc-wake@pve wake --comment "wake-lxc token" --privsep 1
```

Grant the role to the token principal:

### Show token(s) for sanity

```
pveum user token list svc-wake@pve
```

### Add ACL directly to the token (NOT just the user) for CT 111

```
pveum aclmod /vms/111 --token 'svc-wake@pve!wake' --role WakeRole
```

### Verify ACLs (should list /vms/111 -> svc-wake@pve!wake -> WakeRole)

```
pveum acl list | egrep 'svc-wake@pve|/vms/111|WakeRole'
```

## Auto-Discovery & IPAM (optional)

Auto-discovery lists every guest on the node and reads its config to find `wake.*` labels, so the token needs node-level access instead of a single-CT scope:

```
pveum aclmod /nodes/<node> --token 'svc-wake@pve!wake' --role WakeRole
```

Two extra permissions on top of the base role:
- `VM.Config.Options` — discovery reads the notes/description field (a guest config option, not covered by `VM.Audit` alone)
- `SDN.Audit` — resolving a DHCP-via-IPAM backend queries the cluster SDN endpoint

```
pveum role mod WakeRole --privs "VM.Audit,VM.Monitor,VM.PowerMgmt,VM.Config.Options,SDN.Audit"
```
