package internal

import (
	"fmt"
	"strconv"
	"strings"
)

// WakeLabels holds the wake.* flags parsed out of a Proxmox description.
// A zero value for Port/StopMinutes/ShutdownMinutes means "not set".
type WakeLabels struct {
	Enable          bool    // wake.enable == "true"
	Domain          string  // wake.domain
	Port            int     // wake.port
	Host            string  // wake.host
	Backend         string  // wake.backend (full URL)
	ShutdownMinutes float64 // wake.shutdown_minutes
	StopMinutes     float64 // wake.stop_minutes
}

// ParseWakeLabels extracts the wake.* lines from a Proxmox description.
// Lines not starting with "wake." (traefik.*, comments, blanks) are ignored.
func ParseWakeLabels(description string) WakeLabels {
	var l WakeLabels
	for _, line := range strings.Split(description, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "wake.") || !strings.Contains(line, "=") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		key = strings.TrimSpace(key[len("wake."):])
		value = strings.TrimSpace(value)
		switch key {
		case "enable":
			l.Enable = strings.EqualFold(value, "true")
		case "domain":
			l.Domain = value
		case "port":
			if p, err := strconv.Atoi(value); err == nil {
				l.Port = p
			}
		case "host":
			l.Host = value
		case "backend":
			l.Backend = value
		case "shutdown_minutes":
			if f, err := strconv.ParseFloat(value, 64); err == nil {
				l.ShutdownMinutes = f
			}
		case "stop_minutes":
			if f, err := strconv.ParseFloat(value, 64); err == nil {
				l.StopMinutes = f
			}
		}
	}
	return l
}

// ParseStaticIP extracts the ip= value from a netN config line, e.g.
// "name=eth0,bridge=vmbr0,gw=10.0.100.1,ip=10.0.100.128/24,type=veth".
// Returns "" for "ip=dhcp" or a missing/absent net0.
func ParseStaticIP(net0 string) string {
	for _, part := range strings.Split(net0, ",") {
		if strings.HasPrefix(part, "ip=") {
			value := part[len("ip="):]
			if strings.EqualFold(value, "dhcp") {
				return ""
			}
			ip, _, _ := strings.Cut(value, "/")
			return ip
		}
	}
	return ""
}

// ResolveBackend resolves the backend URL for a container. Order:
// wake.backend > wake.host+port > static ip= from net0 > IPAM lookup.
// Returns "" when no backend can be determined (or no port is set).
func ResolveBackend(vmid uint64, l WakeLabels, net0 string, ipam func(vmid uint64) (string, bool)) string {
	if l.Backend != "" {
		return l.Backend
	}
	if l.Port == 0 {
		return ""
	}
	if l.Host != "" {
		return fmt.Sprintf("http://%s:%d", l.Host, l.Port)
	}
	ip := ParseStaticIP(net0)
	if ip == "" {
		ip, _ = ipam(vmid)
	}
	if ip == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", ip, l.Port)
}

// ResolveStop returns (minutes, mode). A container's own stop_minutes opts
// into a hard poweroff and wins if shutdown_minutes is also set.
// shutdown_minutes alone gets a graceful shutdown. Neither falls back to the
// global shutdown_minutes (graceful).
func ResolveStop(l WakeLabels, globalShutdown float64) (minutes float64, mode string) {
	if l.StopMinutes != 0 {
		return l.StopMinutes, "stop"
	}
	if l.ShutdownMinutes != 0 {
		return l.ShutdownMinutes, "shutdown"
	}
	return globalShutdown, "shutdown"
}

// ResolveDomain determines a container's domain: wake.domain wins, otherwise
// the domain template with {hostname} substituted (LXC only — QEMU has no OS
// hostname). Both missing returns an error (container is skipped).
func ResolveDomain(l WakeLabels, template, hostname string, isLXC bool) (string, error) {
	if l.Domain != "" {
		return l.Domain, nil
	}
	if template != "" && hostname != "" && isLXC {
		return strings.Replace(template, "{hostname}", hostname, 1), nil
	}
	return "", fmt.Errorf("no domain could be determined")
}
