package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePVE is a mutable fake Proxmox API.
type fakePVE struct {
	mu        sync.Mutex
	status    string
	started   bool
	stopped   bool
	stopMode  string
	desc      string
	hostname  string
	net0      string
	vmID      uint64
	startErr  bool
	listCalls int // counts /lxc and /qemu list calls (for discovery-loop liveness)
}

func (f *fakePVE) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	p := req.URL.Path
	if strings.HasSuffix(p, "/lxc") && strings.Contains(p, "/nodes/") {
		f.mu.Lock()
		f.listCalls++
		f.mu.Unlock()
		writeJSON(rw, map[string]interface{}{
			"data": []map[string]interface{}{
				{"vmid": f.vmID, "name": "n8n", "status": f.status},
			},
		})
		return
	}
	if strings.HasSuffix(p, "/qemu") && strings.Contains(p, "/nodes/") {
		writeJSON(rw, map[string]interface{}{"data": []interface{}{}})
		return
	}
	if strings.HasSuffix(p, "/config") {
		writeJSON(rw, map[string]interface{}{
			"data": map[string]string{
				"description": f.desc,
				"hostname":    f.hostname,
				"net0":        f.net0,
			},
		})
		return
	}
	if strings.HasSuffix(p, "/status/current") {
		f.mu.Lock()
		st := f.status
		f.mu.Unlock()
		writeJSON(rw, map[string]interface{}{"data": map[string]string{"status": st}})
		return
	}
	if strings.HasSuffix(p, "/status/start") {
		f.mu.Lock()
		f.started = true
		err := f.startErr
		f.mu.Unlock()
		if err {
			rw.WriteHeader(500)
			writeJSON(rw, map[string]string{"error": "start failed"})
			return
		}
		writeJSON(rw, map[string]interface{}{"data": true})
		return
	}
	if strings.HasSuffix(p, "/status/stop") || strings.HasSuffix(p, "/status/shutdown") {
		f.mu.Lock()
		f.stopped = true
		if strings.HasSuffix(p, "/status/shutdown") {
			f.stopMode = "shutdown"
		} else {
			f.stopMode = "stop"
		}
		f.mu.Unlock()
		writeJSON(rw, map[string]interface{}{"data": true})
		return
	}
	// IPAM endpoints (no IPAMs configured by default)
	writeJSON(rw, map[string]interface{}{"data": []interface{}{}})
}

func (f *fakePVE) setRunning() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = "running"
}

func (f *fakePVE) setStopped() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = "stopped"
}

func writeJSON(rw http.ResponseWriter, v interface{}) {
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(v)
}

// newTestMW creates a middleware wired to a fake PVE, with a passthrough backend.
func newTestMW(t *testing.T, pve *fakePVE, overrides *Config) (*Middleware, *httptest.Server) {
	t.Helper()
	pveServer := httptest.NewServer(pve)
	c := &Config{
		ApiEndpoint:       pveServer.URL,
		ApiTokenId:        "test",
		ApiToken:          "tok",
		ApiValidateSSL:    "true",
		ApiLogging:        "info",
		NodeName:          "pve1",
		DiscoveryInterval: "1h", // no background loop during tests
		CheckInterval:     "1s",
		ShutdownMinutes:   "30",
	}
	if overrides != nil {
		if overrides.CheckInterval != "" {
			c.CheckInterval = overrides.CheckInterval
		}
		if overrides.StateFile != "" {
			c.StateFile = overrides.StateFile
		}
		if overrides.DomainTemplate != "" {
			c.DomainTemplate = overrides.DomainTemplate
		}
		if overrides.NodeName != "" {
			c.NodeName = overrides.NodeName
		}
	}

	backend := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(200)
		rw.Write([]byte("backend-response"))
	})
	mw, err := New(context.Background(), backend, c, "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return mw, pveServer
}

func TestConfigDefaults(t *testing.T) {
	cfg := CreateConfig()
	if cfg.ApiValidateSSL != "true" {
		t.Errorf("ApiValidateSSL = %q, want true", cfg.ApiValidateSSL)
	}
	if cfg.ApiLogging != "info" {
		t.Errorf("ApiLogging = %q, want info", cfg.ApiLogging)
	}
	if cfg.DiscoveryInterval != "60s" {
		t.Errorf("DiscoveryInterval = %q, want 60s", cfg.DiscoveryInterval)
	}
	if cfg.CheckInterval != "5m" {
		t.Errorf("CheckInterval = %q, want 5m", cfg.CheckInterval)
	}
	if cfg.ShutdownMinutes != "30" {
		t.Errorf("ShutdownMinutes = %q, want 30", cfg.ShutdownMinutes)
	}
}

func TestNewValidation(t *testing.T) {
	base := &Config{
		ApiEndpoint:       "https://pve:8006",
		ApiTokenId:        "user",
		ApiToken:          "tok",
		ApiValidateSSL:    "false",
		ApiLogging:        "info",
		NodeName:          "pve1",
		DiscoveryInterval: "60s",
		CheckInterval:     "5m",
		ShutdownMinutes:   "30",
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid", func(c *Config) {}, false},
		{"missing endpoint", func(c *Config) { c.ApiEndpoint = "" }, true},
		{"missing token id", func(c *Config) { c.ApiTokenId = "" }, true},
		{"missing token", func(c *Config) { c.ApiToken = "" }, true},
		{"missing node", func(c *Config) { c.NodeName = "" }, true},
		{"bad validate ssl", func(c *Config) { c.ApiValidateSSL = "maybe" }, true},
		{"bad log level", func(c *Config) { c.ApiLogging = "verbose" }, true},
		{"bad discovery interval", func(c *Config) { c.DiscoveryInterval = "abc" }, true},
		{"bad check interval", func(c *Config) { c.CheckInterval = "xyz" }, true},
		{"bad shutdown minutes", func(c *Config) { c.ShutdownMinutes = "soon" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := *base
			tt.mutate(&cfg)
			_, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &cfg, "test")
			if (err != nil) != tt.wantErr {
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDiscovery(t *testing.T) {
	pve := &fakePVE{
		vmID:     100,
		status:   "stopped",
		desc:     "wake.enable=true\nwake.domain=n8n.lan\nwake.port=80\nwake.host=10.0.0.5",
		hostname: "n8n",
		net0:     "name=eth0,ip=10.0.0.1/24",
	}
	mw, srv := newTestMW(t, pve, &Config{})
	defer srv.Close()

	mw.mu.Lock()
	c, ok := mw.domains["n8n.lan"]
	mw.mu.Unlock()
	if !ok {
		t.Fatal("expected n8n.lan in domains")
	}
	if c.vmid != 100 {
		t.Errorf("vmid = %d, want 100", c.vmid)
	}
	if c.backend != "http://10.0.0.5:80" {
		t.Errorf("backend = %q, want http://10.0.0.5:80", c.backend)
	}
}

func TestUnknownDomain404(t *testing.T) {
	pve := &fakePVE{vmID: 100, status: "stopped", desc: "wake.enable=true\nwake.domain=n8n.lan\nwake.port=80"}
	mw, srv := newTestMW(t, pve, &Config{})
	defer srv.Close()

	req := httptest.NewRequest("GET", "http://unknown.lan/", nil)
	req.Host = "unknown.lan"
	rw := httptest.NewRecorder()
	mw.ServeHTTP(rw, req)

	if rw.Code != 404 {
		t.Errorf("status = %d, want 404", rw.Code)
	}
}

func TestRunningPassthrough(t *testing.T) {
	pve := &fakePVE{
		vmID:   100,
		status: "running",
		desc:   "wake.enable=true\nwake.domain=n8n.lan\nwake.port=80\nwake.host=10.0.0.5",
	}
	mw, srv := newTestMW(t, pve, &Config{})
	defer srv.Close()

	req := httptest.NewRequest("GET", "http://n8n.lan/path", nil)
	req.Host = "n8n.lan"
	rw := httptest.NewRecorder()
	mw.ServeHTTP(rw, req)

	if rw.Code != 200 {
		t.Errorf("status = %d, want 200", rw.Code)
	}
	if body := rw.Body.String(); body != "backend-response" {
		t.Errorf("body = %q, want backend-response", body)
	}
}

func TestStoppedStartsAndServesPage(t *testing.T) {
	pve := &fakePVE{
		vmID:   100,
		status: "stopped",
		desc:   "wake.enable=true\nwake.domain=n8n.lan\nwake.port=80\nwake.host=10.0.0.5",
	}
	mw, srv := newTestMW(t, pve, &Config{})
	defer srv.Close()

	req := httptest.NewRequest("GET", "http://n8n.lan/", nil)
	req.Host = "n8n.lan"
	rw := httptest.NewRecorder()
	mw.ServeHTTP(rw, req)

	if rw.Code != 200 {
		t.Errorf("status = %d, want 200", rw.Code)
	}
	body := rw.Body.String()
	if !strings.Contains(body, "Starting...") {
		t.Errorf("body missing starting page: %q", body)
	}
	pve.mu.Lock()
	defer pve.mu.Unlock()
	if !pve.started {
		t.Error("expected start command to be sent")
	}
}

func TestStatusReady(t *testing.T) {
	// backend server for the probe
	backendSrv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(200)
	}))
	defer backendSrv.Close()

	pve := &fakePVE{
		vmID:   100,
		status: "running",
		desc:   fmt.Sprintf("wake.enable=true\nwake.domain=n8n.lan\nwake.port=80\nwake.backend=%s", backendSrv.URL),
	}
	mw, srv := newTestMW(t, pve, &Config{})
	defer srv.Close()

	req := httptest.NewRequest("GET", "http://n8n.lan/__wake__/status", nil)
	req.Host = "n8n.lan"
	rw := httptest.NewRecorder()
	mw.ServeHTTP(rw, req)

	var resp struct {
		Ready   bool   `json:"ready"`
		Message string `json:"message"`
	}
	json.Unmarshal(rw.Body.Bytes(), &resp)
	if !resp.Ready {
		t.Errorf("expected ready=true, got %+v", resp)
	}
}

func TestStopTimer(t *testing.T) {
	pve := &fakePVE{
		vmID:   100,
		status: "running",
		desc:   "wake.enable=true\nwake.domain=n8n.lan\nwake.port=80\nwake.host=10.0.0.5\nwake.stop_minutes=0.01",
	}
	mw, srv := newTestMW(t, pve, &Config{CheckInterval: "100ms"})
	defer srv.Close()

	// Trigger the running path (schedules the stop timer).
	req := httptest.NewRequest("GET", "http://n8n.lan/", nil)
	req.Host = "n8n.lan"
	rw := httptest.NewRecorder()
	mw.ServeHTTP(rw, req)

	// Wait for the stop timer to fire (0.01 min = 0.6s).
	time.Sleep(1500 * time.Millisecond)

	pve.mu.Lock()
	defer pve.mu.Unlock()
	if !pve.stopped {
		t.Error("expected stop command to be sent")
	}
	if pve.stopMode != "stop" {
		t.Errorf("stopMode = %q, want stop", pve.stopMode)
	}
}

func TestReloadReusesInstance(t *testing.T) {
	pve := &fakePVE{
		vmID:     100,
		status:   "stopped",
		desc:     "wake.enable=true\nwake.domain=n8n.lan\nwake.port=80\nwake.host=10.0.0.5",
		hostname: "n8n",
	}
	pveServer := httptest.NewServer(pve)
	defer pveServer.Close()

	c := &Config{
		ApiEndpoint:       pveServer.URL,
		ApiTokenId:        "test",
		ApiToken:          "tok",
		ApiValidateSSL:    "true",
		ApiLogging:        "info",
		NodeName:          "reload-test-node", // unique key so other tests can't collide with this one
		DiscoveryInterval: "1h",
		CheckInterval:     "1h",
		ShutdownMinutes:   "30",
	}

	backend1Hit := false
	backend1 := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		backend1Hit = true
		rw.WriteHeader(200)
	})
	mw1, err := New(context.Background(), backend1, c, "wake-1")
	if err != nil {
		t.Fatalf("first New: %v", err)
	}

	// Simulate a Traefik reload: New is called again for the same
	// apiEndpoint+nodeName, with a new next handler (Traefik always builds a
	// fresh chain) but the plugin's own config unchanged.
	backend2Hit := false
	backend2 := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		backend2Hit = true
		rw.WriteHeader(200)
	})
	mw2, err := New(context.Background(), backend2, c, "wake-1")
	if err != nil {
		t.Fatalf("second New: %v", err)
	}

	if mw1 != mw2 {
		t.Fatal("expected the second New to reuse the first instance, got a different one")
	}

	// The reused instance must route to the NEW next handler, not the old one.
	pve.setRunning()
	req := httptest.NewRequest("GET", "http://n8n.lan/", nil)
	req.Host = "n8n.lan"
	rw := httptest.NewRecorder()
	mw2.ServeHTTP(rw, req)

	if backend1Hit {
		t.Error("stale next handler from the first New was invoked")
	}
	if !backend2Hit {
		t.Error("expected the reused instance to route to the latest next handler")
	}
}

func TestBreaker(t *testing.T) {
	pve := &fakePVE{
		vmID:     100,
		status:   "stopped",
		desc:     "wake.enable=true\nwake.domain=n8n.lan\nwake.port=80\nwake.host=10.0.0.5",
		startErr: true,
	}
	mw, srv := newTestMW(t, pve, &Config{})
	defer srv.Close()

	// First request: start fails, breaker set.
	req := httptest.NewRequest("GET", "http://n8n.lan/", nil)
	req.Host = "n8n.lan"
	rw := httptest.NewRecorder()
	mw.ServeHTTP(rw, req)

	// The starting map should have an error-level message.
	mw.mu.Lock()
	key := "100"
	s, ok := mw.starting[key]
	mw.mu.Unlock()
	if !ok {
		t.Fatal("expected starting entry")
	}
	if s.level != "error" {
		t.Errorf("level = %q, want error", s.level)
	}

	// Clear the starting map so the next request re-enters ensureStartCycle.
	mw.mu.Lock()
	delete(mw.starting, key)
	mw.mu.Unlock()

	// Second request: breaker gates the start.
	req2 := httptest.NewRequest("GET", "http://n8n.lan/", nil)
	req2.Host = "n8n.lan"
	rw2 := httptest.NewRecorder()
	mw.ServeHTTP(rw2, req2)

	mw.mu.Lock()
	s2, ok := mw.starting[key]
	mw.mu.Unlock()
	if !ok {
		t.Fatal("expected starting entry after breaker")
	}
	if !strings.Contains(s2.message, "Recent start failures") {
		t.Errorf("message = %q, want breaker message", s2.message)
	}
}

// TestDiscoveryLoopSurvivesCtxCancellation verifies the v3 scenario:
// Traefik v3 cancels the old ctx on reload (commit fadee5e87). The
// discoveryLoop must not depend on that ctx — it uses
// context.Background() and runs for the process lifetime.
func TestDiscoveryLoopSurvivesCtxCancellation(t *testing.T) {
	pve := &fakePVE{
		vmID:     100,
		status:   "stopped",
		desc:     "wake.enable=true\nwake.domain=n8n.lan\nwake.port=80\nwake.host=10.0.0.5",
		hostname: "n8n",
	}
	pveServer := httptest.NewServer(pve)
	defer pveServer.Close()

	c := &Config{
		ApiEndpoint:       pveServer.URL,
		ApiTokenId:        "test",
		ApiToken:          "tok",
		ApiValidateSSL:    "true",
		ApiLogging:        "info",
		NodeName:          "ctx-cancel-node", // unique registry key
		DiscoveryInterval: "50ms",
		CheckInterval:     "1h",
		ShutdownMinutes:   "30",
	}

	// First New: simulates the initial Traefik startup.
	ctx, cancel := context.WithCancel(context.Background())
	backend := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(200)
	})
	mw, err := New(ctx, backend, c, "wake-ctx")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Let the discovery loop tick a few times.
	time.Sleep(200 * time.Millisecond)
	pve.mu.Lock()
	before := pve.listCalls
	pve.mu.Unlock()
	if before == 0 {
		t.Fatal("expected the discovery loop to have made list calls")
	}

	// Cancel the ctx: simulates Traefik v3 cancelling the old context on reload.
	cancel()

	// Second New: simulates the reload. The discoveryLoop must still be
	// running (it uses context.Background(), not the Traefik ctx).
	backend2 := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(200)
	})
	mw2, err := New(context.Background(), backend2, c, "wake-ctx")
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	if mw != mw2 {
		t.Fatal("expected the second New to reuse the first instance")
	}

	// Give the discovery loop time to tick after the cancellation.
	time.Sleep(200 * time.Millisecond)

	// The discoveryLoop must still be making API calls after the ctx was
	// cancelled. If it had exited on ctx.Done() (the old bug), listCalls
	// would not have increased.
	pve.mu.Lock()
	after := pve.listCalls
	pve.mu.Unlock()
	if after <= before {
		t.Errorf("discoveryLoop stopped after ctx cancellation: listCalls before=%d after=%d", before, after)
	}
}
