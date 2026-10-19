package internal

// Version holds the Proxmox release info
type Version struct {
	Release string `json:"release"`
}

// VMRef is a node's VM/container list entry (GET /nodes/{node}/lxc|qemu)
type VMRef struct {
	VMID   uint64 `json:"vmid"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// VMConfig holds the fields of a VM/container config we care about
// (GET /nodes/{node}/{kind}/{vmid}/config)
type VMConfig struct {
	Description string `json:"description"`
	Hostname    string `json:"hostname"`
	Net0        string `json:"net0"`
}

// IPAM is a cluster IPAM plugin (GET /cluster/sdn/ipams)
type IPAM struct {
	IPAMID string `json:"ipam"`
	Name   string `json:"name"`
}

// IPAMStatus holds the IP records held by an IPAM plugin
type IPAMStatus struct {
	Data []IPAMRecord `json:"data"`
}

// IPAMRecord is one allocated IP in an IPAM plugin
type IPAMRecord struct {
	VMID uint64 `json:"vmid"`
	IP   string `json:"ip"`
}
