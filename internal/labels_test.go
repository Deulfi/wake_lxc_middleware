package internal

import (
	"testing"
)

func TestParseWakeLabels(t *testing.T) {
	tests := []struct {
		name        string
		description string
		want        WakeLabels
	}{
		{
			name:        "no wake labels",
			description: "traefik.enable=true\nmy container",
			want:        WakeLabels{},
		},
		{
			name: "full wake block",
			description: `traefik.enable=true
wake.enable=true
wake.domain=n8n.pve.lan
wake.port=5678
wake.host=10.0.0.5`,
			want: WakeLabels{
				Enable:   true,
				Domain:   "n8n.pve.lan",
				Port:     5678,
				Host:     "10.0.0.5",
				Backend:  "",
				ShutdownMinutes: 0,
				StopMinutes:     0,
			},
		},
		{
			name: "shutdown and stop minutes",
			description: `wake.enable=true
wake.domain=x.lan
wake.shutdown_minutes=15.5
wake.stop_minutes=2`,
			want: WakeLabels{
				Enable:          true,
				Domain:          "x.lan",
				ShutdownMinutes: 15.5,
				StopMinutes:     2,
			},
		},
		{
			name: "enable value case-insensitive",
			description: "wake.enable=TRUE\nwake.domain=y.lan",
			want: WakeLabels{
				Enable: true,
				Domain: "y.lan",
			},
		},
		{
			name: "invalid port ignored",
			description: "wake.enable=true\nwake.port=abc\nwake.domain=z.lan",
			want: WakeLabels{
				Enable: true,
				Domain: "z.lan",
				Port:   0,
			},
		},
		{
			name: "empty description",
			description: "",
			want:        WakeLabels{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseWakeLabels(tt.description)
			if got != tt.want {
				t.Errorf("ParseWakeLabels() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseStaticIP(t *testing.T) {
	tests := []struct {
		name string
		net0 string
		want string
	}{
		{"static ip", "name=eth0,bridge=vmbr0,ip=10.0.100.128/24,type=veth", "10.0.100.128"},
		{"dhcp", "name=eth0,bridge=vmbr0,ip=dhcp", ""},
		{"no ip", "name=eth0,bridge=vmbr0", ""},
		{"empty", "", ""},
		{"ip not first", "name=eth0,gw=10.0.0.1,ip=192.168.1.5/24", "192.168.1.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseStaticIP(tt.net0)
			if got != tt.want {
				t.Errorf("ParseStaticIP(%q) = %q, want %q", tt.net0, got, tt.want)
			}
		})
	}
}

func TestResolveBackend(t *testing.T) {
	tests := []struct {
		name   string
		vmid   uint64
		labels WakeLabels
		net0   string
		ipam   map[uint64]string
		want   string
	}{
		{
			name:   "backend wins",
			vmid:   100,
			labels: WakeLabels{Port: 80, Backend: "http://custom:8080"},
			want:   "http://custom:8080",
		},
		{
			name:   "no port",
			vmid:   100,
			labels: WakeLabels{Host: "10.0.0.5"},
			want:   "",
		},
		{
			name:   "host with port",
			vmid:   100,
			labels: WakeLabels{Port: 8080, Host: "10.0.0.5"},
			want:   "http://10.0.0.5:8080",
		},
		{
			name:   "static ip from net0",
			vmid:   100,
			labels: WakeLabels{Port: 80},
			net0:   "name=eth0,ip=10.0.100.128/24",
			want:   "http://10.0.100.128:80",
		},
		{
			name:   "ipam fallback",
			vmid:   200,
			labels: WakeLabels{Port: 80},
			ipam:   map[uint64]string{200: "172.31.0.10"},
			want:   "http://172.31.0.10:80",
		},
		{
			name:   "no backend possible",
			vmid:   300,
			labels: WakeLabels{Port: 80},
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ipamFn := func(vmid uint64) (string, bool) {
				ip, ok := tt.ipam[vmid]
				return ip, ok
			}
			got := ResolveBackend(tt.vmid, tt.labels, tt.net0, ipamFn)
			if got != tt.want {
				t.Errorf("ResolveBackend() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveStop(t *testing.T) {
	tests := []struct {
		name     string
		labels   WakeLabels
		global   float64
		wantMin  float64
		wantMode string
	}{
		{"stop minutes wins", WakeLabels{StopMinutes: 5, ShutdownMinutes: 10}, 30, 5, "stop"},
		{"shutdown minutes", WakeLabels{ShutdownMinutes: 15}, 30, 15, "shutdown"},
		{"global fallback", WakeLabels{}, 30, 30, "shutdown"},
		{"stop overrides shutdown", WakeLabels{StopMinutes: 2, ShutdownMinutes: 99}, 30, 2, "stop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			min, mode := ResolveStop(tt.labels, tt.global)
			if min != tt.wantMin || mode != tt.wantMode {
				t.Errorf("ResolveStop() = (%v, %q), want (%v, %q)", min, mode, tt.wantMin, tt.wantMode)
			}
		})
	}
}

func TestResolveDomain(t *testing.T) {
	tests := []struct {
		name     string
		labels   WakeLabels
		template string
		hostname string
		isLXC    bool
		want     string
		wantErr  bool
	}{
		{"wake.domain wins", WakeLabels{Domain: "x.lan"}, "{hostname}.lan", "host", true, "x.lan", false},
		{"template lxc", WakeLabels{}, "{hostname}.lan", "n8nhost", true, "n8nhost.lan", false},
		{"template qemu no hostname", WakeLabels{}, "{hostname}.lan", "", false, "", true},
		{"no domain", WakeLabels{}, "", "", true, "", true},
		{"template qemu with hostname ignored", WakeLabels{}, "{hostname}.lan", "qemuhost", false, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveDomain(tt.labels, tt.template, tt.hostname, tt.isLXC)
			if (err != nil) != tt.wantErr {
				t.Errorf("ResolveDomain() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ResolveDomain() = %q, want %q", got, tt.want)
			}
		})
	}
}
