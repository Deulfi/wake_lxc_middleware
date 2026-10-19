package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Deulfi/wake_lxc_middleware/internal"
)

// Config is the plugin configuration. All fields are strings; parsing happens
// in New (strconv/time), mirroring the reference provider's convention.
type Config struct {
	ApiEndpoint       string `json:"apiEndpoint" yaml:"apiEndpoint" toml:"apiEndpoint"`
	ApiTokenId        string `json:"apiTokenId" yaml:"apiTokenId" toml:"apiTokenId"`
	ApiToken          string `json:"apiToken" yaml:"apiToken" toml:"apiToken"`
	ApiValidateSSL    string `json:"apiValidateSSL" yaml:"apiValidateSSL" toml:"apiValidateSSL"`
	ApiLogging        string `json:"apiLogging" yaml:"apiLogging" toml:"apiLogging"`
	NodeName          string `json:"nodeName" yaml:"nodeName" toml:"nodeName"`
	DiscoveryInterval string `json:"discoveryInterval" yaml:"discoveryInterval" toml:"discoveryInterval"`
	CheckInterval     string `json:"checkInterval" yaml:"checkInterval" toml:"checkInterval"`
	ShutdownMinutes   string `json:"shutdownMinutes" yaml:"shutdownMinutes" toml:"shutdownMinutes"`
	DomainTemplate    string `json:"domainTemplate" yaml:"domainTemplate" toml:"domainTemplate"`
	StateFile         string `json:"stateFile" yaml:"stateFile" toml:"stateFile"`
}

// CreateConfig returns the default configuration.
func CreateConfig() *Config {
	return &Config{
		ApiValidateSSL:    "true",
		ApiLogging:        "info",
		DiscoveryInterval: "60s",
		CheckInterval:     "5m",
		ShutdownMinutes:   "30",
	}
}

// Middleware is the wake-lxc plugin middleware.
type Middleware struct {
	next    http.Handler
	name    string
	client  *internal.ProxmoxClient
	ctx     context.Context

	nodeName          string
	discoveryInterval time.Duration
	checkInterval     time.Duration
	shutdownMinutes   float64
	domainTemplate    string
	stateFile         string

	mu          sync.Mutex
	domains     map[string]*container
	byVMID      map[uint64]*container
	starting    map[string]*startState
	startLocks  map[string]*tryLock
	watchdogs   map[string]struct{}
	breakers    map[string]time.Time
	ipamCache   map[string]string
	ipamCacheAt time.Time
	probeClient *http.Client
}

// container is one wake-enabled Proxmox VM/container.
type container struct {
	vmid       uint64
	kind       string
	labels     internal.WakeLabels
	domain     string
	backend    string
	lastStatus string
}

// startState is the live status for a starting container.
type startState struct {
	startedAt time.Time
	message   string
	level     string
}

// tryLock is a simple non-blocking lock (Go has no TryLock).
type tryLock struct {
	mu   sync.Mutex
	held bool
}

func (t *tryLock) try() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.held {
		return false
	}
	t.held = true
	return true
}

func (t *tryLock) unlock() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.held = false
}


// lastSeen holds the previous discovery snapshot at package level so the
// label-change and membership diff keep working across Middleware
// recreations. Traefik re-instantiates this plugin between discovery passes
// (each pass is a fresh instance with an empty m.byVMID); a per-instance
// baseline therefore never sees an oldC and the change detector is dead.
// Keyed by node so different nodes don't cross-contaminate.
var lastSeen = struct {
	sync.Mutex
	byNode map[string]map[uint64]*container
}{byNode: make(map[string]map[uint64]*container)}

// stopState holds the armed stop timers at package level so cancellation
// keeps working across Middleware recreations. Traefik re-instantiates this
// plugin between discovery passes; a per-instance map loses the armed timer
// when a label change schedules a new one, and the old timer's goroutine
// (holding the old instance's map) fires unguarded. Keyed by node so
// different nodes don't cross-contaminate.
var stopState = struct {
	sync.Mutex
	byNode map[string]*nodeStopState
}{byNode: make(map[string]*nodeStopState)}

type nodeStopState struct {
	timers    map[string]*time.Timer
	deadlines map[string]time.Time
	lastDelay map[string]time.Duration
}

func nodeStop(node string) *nodeStopState {
	stopState.Lock()
	defer stopState.Unlock()
	ns, ok := stopState.byNode[node]
	if !ok {
		ns = &nodeStopState{
			timers:    make(map[string]*time.Timer),
			deadlines: make(map[string]time.Time),
			lastDelay: make(map[string]time.Duration),
		}
		stopState.byNode[node] = ns
	}
	return ns
}
// New creates the middleware. It validates config, runs a first discovery
// pass (error → log, don't fail), resumes the stop-timer state file, and
// starts the background discovery loop.
func New(ctx context.Context, next http.Handler, config *Config, name string) (*Middleware, error) {
	if config.ApiEndpoint == "" {
		return nil, fmt.Errorf("apiEndpoint is required")
	}
	if config.ApiTokenId == "" {
		return nil, fmt.Errorf("apiTokenId is required")
	}
	if config.ApiToken == "" {
		return nil, fmt.Errorf("apiToken is required")
	}
	if config.NodeName == "" {
		return nil, fmt.Errorf("nodeName is required")
	}

	validateSSL := true
	if config.ApiValidateSSL != "" {
		v, err := strconv.ParseBool(config.ApiValidateSSL)
		if err != nil {
			return nil, fmt.Errorf("invalid apiValidateSSL: %w", err)
		}
		validateSSL = v
	}

	logLevel := "info"
	if config.ApiLogging != "" {
		if config.ApiLogging != "info" && config.ApiLogging != "debug" {
			return nil, fmt.Errorf("apiLogging must be 'info' or 'debug'")
		}
		logLevel = config.ApiLogging
	}

	discoveryInterval := 60 * time.Second
	if config.DiscoveryInterval != "" {
		d, err := time.ParseDuration(config.DiscoveryInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid discoveryInterval: %w", err)
		}
		discoveryInterval = d
	}

	checkInterval := 5 * time.Minute
	if config.CheckInterval != "" {
		d, err := time.ParseDuration(config.CheckInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid checkInterval: %w", err)
		}
		checkInterval = d
	}

	shutdownMinutes := 30.0
	if config.ShutdownMinutes != "" {
		f, err := strconv.ParseFloat(config.ShutdownMinutes, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid shutdownMinutes: %w", err)
		}
		shutdownMinutes = f
	}

	client := internal.NewProxmoxClient(config.ApiEndpoint, config.ApiTokenId, config.ApiToken, validateSSL, logLevel)

	m := &Middleware{
		next:              next,
		name:              name,
		client:            client,
		ctx:               ctx,
		nodeName:          config.NodeName,
		discoveryInterval: discoveryInterval,
		checkInterval:     checkInterval,
		shutdownMinutes:   shutdownMinutes,
		domainTemplate:    config.DomainTemplate,
		stateFile:         config.StateFile,
		domains:           make(map[string]*container),
		byVMID:            make(map[uint64]*container),
		starting:          make(map[string]*startState),
		startLocks:        make(map[string]*tryLock),
		watchdogs:         make(map[string]struct{}),
		breakers:          make(map[string]time.Time),
		ipamCache:         make(map[string]string),
		probeClient:       &http.Client{Timeout: 2 * time.Second},
	}

	// First discovery pass (error → log, don't fail Traefik startup).
	if err := m.discover(ctx); err != nil {
		log.Printf("wake-lxc: initial discovery pass failed: %v", err)
	}

	// State resume.
	if config.StateFile != "" {
		m.loadState(ctx)
	}

	// Background discovery loop.
	go m.discoveryLoop(ctx)

	return m, nil
}

// ServeHTTP is the request path.
func (m *Middleware) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	host := strings.ToLower(req.Host)
	c := m.lookup(host)
	if c == nil {
		m.write404(rw)
		return
	}
	if req.URL.Path == "/__wake__/status" {
		m.writeStatus(rw, c)
		return
	}
	if m.isRunning(c) {
		m.clearStarting(c)
		m.scheduleStop(c)
		m.ensureWatchdog(c)
		m.next.ServeHTTP(rw, req)
		return
	}
	m.ensureStartCycle(c)
	m.ensureWatchdog(c)
	m.serveStartingPage(rw, c)
}

func (m *Middleware) lookup(host string) *container {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.domains[host]
}

func (m *Middleware) vmidKey(c *container) string {
	return strconv.FormatInt(int64(c.vmid), 10)
}

// isRunning checks the container's Proxmox status. Error → false (records a
// failure); running → true (resets failures); stopped → false.
func (m *Middleware) isRunning(c *container) bool {
	status, err := m.client.GetStatus(context.Background(), m.nodeName, c.kind, c.vmid)
	if err != nil {
		log.Printf("wake-lxc: VMID %d: status check failed: %v", c.vmid, err)
		m.recordFailure(c)
		return false
	}
	if status != c.lastStatus {
		log.Printf("wake-lxc: VMID %d is %s", c.vmid, status)
		c.lastStatus = status
	}
	if status == "running" {
		m.resetFailures(c)
		return true
	}
	return false
}

func (m *Middleware) recordFailure(c *container) {
	key := m.vmidKey(c)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.breakers[key] = time.Now()
}

func (m *Middleware) resetFailures(c *container) {
	key := m.vmidKey(c)
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.breakers, key)
}

func (m *Middleware) clearStarting(c *container) {
	key := m.vmidKey(c)
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.starting, key)
}

func (m *Middleware) setStartState(key, message, level string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.starting[key]; ok {
		s.message = message
		s.level = level
	}
}

func (m *Middleware) getStartLock(key string) *tryLock {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.startLocks[key]; ok {
		return l
	}
	l := &tryLock{}
	m.startLocks[key] = l
	return l
}

// ensureStartCycle sends a start command for a stopped container (once per
// request, guarded by the start lock and circuit breaker).
func (m *Middleware) ensureStartCycle(c *container) {
	key := m.vmidKey(c)

	m.mu.Lock()
	if _, ok := m.starting[key]; ok {
		m.mu.Unlock()
		return
	}
	m.starting[key] = &startState{startedAt: time.Now(), message: "Container is stopped, starting now...", level: "info"}
	m.mu.Unlock()

	lock := m.getStartLock(key)
	if !lock.try() {
		return
	}
	defer lock.unlock()

	if m.isRunning(c) {
		m.setStartState(key, "Container is already running.", "info")
		return
	}

	m.mu.Lock()
	if last, ok := m.breakers[key]; ok {
		if time.Since(last) < 300*time.Second {
			m.mu.Unlock()
			m.setStartState(key, "Recent start failures, waiting before retrying...", "error")
			return
		}
		delete(m.breakers, key)
	}
	m.mu.Unlock()

	if err := m.client.StartVM(context.Background(), m.nodeName, c.kind, c.vmid); err != nil {
		m.mu.Lock()
		m.breakers[key] = time.Now()
		m.mu.Unlock()
		m.setStartState(key, "Failed to start container: "+err.Error(), "error")
		log.Printf("wake-lxc: VMID %d: start failed: %v", c.vmid, err)
	} else {
		m.mu.Lock()
		delete(m.breakers, key)
		m.mu.Unlock()
		m.setStartState(key, "Start command sent successfully.", "info")
		log.Printf("wake-lxc: VMID %d: start command sent", c.vmid)
	}
}

// scheduleStop schedules a stop timer with the full duration (reschedule-on-access).
func (m *Middleware) scheduleStop(c *container) {
	key := m.vmidKey(c)
	minutes, mode := internal.ResolveStop(c.labels, m.shutdownMinutes)
	m.scheduleStopWithDelay(c, key, mode, time.Duration(minutes*float64(time.Minute)))
}

func (m *Middleware) scheduleStopWithDelay(c *container, key, mode string, delay time.Duration) {
	ns := nodeStop(m.nodeName)
	stopState.Lock()
	if t, ok := ns.timers[key]; ok {
		t.Stop()
	}
	deadline := time.Now().Add(delay)
	ns.deadlines[key] = deadline
	timer := time.NewTimer(delay)
	ns.timers[key] = timer
	if prev, ok := ns.lastDelay[key]; !ok || prev != delay {
		log.Printf("wake-lxc: VMID %d: shutdown scheduled in %.1f min", c.vmid, delay.Minutes())
		ns.lastDelay[key] = delay
	}
	stopState.Unlock()

	go func() {
		<-timer.C
		m.onStopTimer(c, key, mode, timer)
	}()

	m.saveState()
}

func (m *Middleware) onStopTimer(c *container, key, mode string, timer *time.Timer) {
	ns := nodeStop(m.nodeName)
	stopState.Lock()
	if cur, ok := ns.timers[key]; ok && cur != timer {
		stopState.Unlock()
		return
	}
	delete(ns.timers, key)
	delete(ns.deadlines, key)
	delay := ns.lastDelay[key]
	stopState.Unlock()

	m.saveState()
	log.Printf("wake-lxc: VMID %d: stop timer fired (scheduled: %.1f min)", c.vmid, delay.Minutes())

	if m.isRunning(c) {
		if err := m.client.StopVM(context.Background(), m.nodeName, c.kind, c.vmid, mode); err != nil {
			log.Printf("wake-lxc: VMID %d: shutdown failed: %v", c.vmid, err)
		} else {
			log.Printf("wake-lxc: VMID %d: shutdown command sent", c.vmid)
		}
	} else {
		log.Printf("wake-lxc: VMID %d: already stopped, nothing to do", c.vmid)
	}
}

func (m *Middleware) cancelStop(key string) {
	ns := nodeStop(m.nodeName)
	stopState.Lock()
	if t, ok := ns.timers[key]; ok {
		t.Stop()
		delete(ns.timers, key)
	}
	delete(ns.deadlines, key)
	stopState.Unlock()
	m.saveState()
}

// ensureWatchdog starts one watchdog goroutine per container. It self-
// terminates when the stop timer is removed or the container stops externally.
func (m *Middleware) ensureWatchdog(c *container) {
	key := m.vmidKey(c)
	m.mu.Lock()
	if _, ok := m.watchdogs[key]; ok {
		m.mu.Unlock()
		return
	}
	m.watchdogs[key] = struct{}{}
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.watchdogs, key)
			m.mu.Unlock()
		}()
		ticker := time.NewTicker(m.checkInterval)
		defer ticker.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
				ns := nodeStop(m.nodeName)
				stopState.Lock()
				hasTimer := ns.timers[key] != nil
				stopState.Unlock()
				if !hasTimer {
					return
				}
				if !m.isRunning(c) {
					log.Printf("wake-lxc: VMID %d: stopped externally, cancelling pending stop timer", c.vmid)
					m.cancelStop(key)
					return
				}
			}
		}
	}()
}

// discover runs one discovery pass: lists LXC/QEMU, reads configs, resolves
// wake labels, and swaps the domain/VMID maps.
func (m *Middleware) discover(ctx context.Context) error {
	ipamMap := m.getIPAMMap(ctx)

	domains := make(map[string]*container)
	byVMID := make(map[uint64]*container)

	lxcRefs, err := m.client.ListLXC(ctx, m.nodeName)
	if err != nil {
		return err
	}
	for _, ref := range lxcRefs {
		m.discoverOne(ctx, "lxc", ref, ipamMap, domains, byVMID)
	}

	qemuRefs, err := m.client.ListQEMU(ctx, m.nodeName)
	if err != nil {
		return err
	}
	for _, ref := range qemuRefs {
		m.discoverOne(ctx, "qemu", ref, ipamMap, domains, byVMID)
	}

	// Detect label changes and membership changes against the package-level
	// baseline (lastSeen), which survives Middleware recreation. An empty
	// baseline means no prior pass has run; that's initialization, not a
	// change, so it's reported silently.
	lastSeen.Lock()
	prev := lastSeen.byNode[m.nodeName]
	hadBaseline := len(prev) > 0
	var changed, added, removed []uint64
	for vmid, newC := range byVMID {
		oldC, ok := prev[vmid]
		if !ok {
			added = append(added, vmid)
			continue
		}
		if oldC.labels.ShutdownMinutes != newC.labels.ShutdownMinutes || oldC.labels.StopMinutes != newC.labels.StopMinutes {
			changed = append(changed, vmid)
			var parts []string
			if oldC.labels.ShutdownMinutes != newC.labels.ShutdownMinutes {
				parts = append(parts, fmt.Sprintf("shutdown %.2f->%.2f", oldC.labels.ShutdownMinutes, newC.labels.ShutdownMinutes))
			}
			if oldC.labels.StopMinutes != newC.labels.StopMinutes {
				parts = append(parts, fmt.Sprintf("stop %.2f->%.2f", oldC.labels.StopMinutes, newC.labels.StopMinutes))
			}
			log.Printf("wake-lxc: VMID %d: shutdown label changed: %s", newC.vmid, strings.Join(parts, ", "))
		}
	}
	for vmid := range prev {
		if _, ok := byVMID[vmid]; !ok {
			removed = append(removed, vmid)
		}
	}
	lastSeen.byNode[m.nodeName] = byVMID
	lastSeen.Unlock()

	m.mu.Lock()
	m.domains = domains
	m.byVMID = byVMID
	m.mu.Unlock()

	// Log membership changes only when a prior baseline existed; silent on
	// the first pass and when nothing changed.
	if hadBaseline && (len(added) > 0 || len(removed) > 0) {
		log.Printf("wake-lxc: discovery change: +%v -%v (now %d wake-enabled)", added, removed, len(byVMID))
	}

	// Reschedule timers for containers whose stop-related labels changed.
	// Only update an existing timer; do not start a new one (a label change
	// must not arm a shutdown timer on a container that has no pending stop).
	for _, vmid := range changed {
		c := byVMID[vmid]
		key := m.vmidKey(c)
		ns := nodeStop(m.nodeName)
		stopState.Lock()
		hasTimer := ns.timers[key] != nil
		stopState.Unlock()
		if hasTimer {
			m.scheduleStop(c)
		}
	}
	return nil
}

func (m *Middleware) discoverOne(ctx context.Context, kind string, ref internal.VMRef, ipamMap map[string]string, domains map[string]*container, byVMID map[uint64]*container) {
	cfg, err := m.client.GetVMConfig(ctx, m.nodeName, kind, ref.VMID)
	if err != nil {
		log.Printf("wake-lxc: VMID %d: discovery check failed, skipping this pass: %v", ref.VMID, err)
		return
	}
	l := internal.ParseWakeLabels(cfg.Description)
	if !l.Enable {
		return
	}
	isLXC := kind == "lxc"
	domain, err := internal.ResolveDomain(l, m.domainTemplate, cfg.Hostname, isLXC)
	if err != nil {
		log.Printf("wake-lxc: VMID %d: %v, skipping", ref.VMID, err)
		return
	}
	backend := internal.ResolveBackend(ref.VMID, l, cfg.Net0, func(vmid uint64) (string, bool) {
		k := strconv.FormatInt(int64(vmid), 10)
		ip, ok := ipamMap[k]
		return ip, ok
	})
	c := &container{vmid: ref.VMID, kind: kind, labels: l, domain: domain, backend: backend}
	if _, exists := domains[domain]; exists {
		log.Printf("wake-lxc: duplicate domain %q, last wins", domain)
	}
	domains[domain] = c
	byVMID[ref.VMID] = c
}

// getIPAMMap fetches the IPAM VMID→IP map once per discovery pass (30s TTL).
func (m *Middleware) getIPAMMap(ctx context.Context) map[string]string {
	m.mu.Lock()
	if !m.ipamCacheAt.IsZero() && time.Since(m.ipamCacheAt) < 30*time.Second {
		cached := m.ipamCache
		m.mu.Unlock()
		return cached
	}
	m.mu.Unlock()

	ipams, err := m.client.GetIPAMs(ctx)
	if err != nil {
		log.Printf("wake-lxc: IPAM lookup failed: %v", err)
		return nil
	}
	result := make(map[string]string)
	for _, ipam := range ipams {
		if ipam.IPAMID == "" {
			continue
		}
		status, err := m.client.GetIPAMStatus(ctx, ipam.IPAMID)
		if err != nil {
			log.Printf("wake-lxc: IPAM %s status failed: %v", ipam.IPAMID, err)
			continue
		}
		for _, rec := range status.Data {
			result[strconv.FormatInt(int64(rec.VMID), 10)] = rec.IP
		}
	}
	m.mu.Lock()
	m.ipamCache = result
	m.ipamCacheAt = time.Now()
	m.mu.Unlock()
	return result
}

func (m *Middleware) discoveryLoop(ctx context.Context) {
	ticker := time.NewTicker(m.discoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.discover(ctx); err != nil {
				log.Printf("wake-lxc: discovery loop failed: %v", err)
			}
		}
	}
}

// saveState atomically writes the deadlines map to the state file.
func (m *Middleware) saveState() {
	if m.stateFile == "" {
		return
	}
	ns := nodeStop(m.nodeName)
	stopState.Lock()
	deadlines := make(map[string]int64)
	for k, d := range ns.deadlines {
		deadlines[k] = d.Unix()
	}
	stopState.Unlock()

	data := struct {
		Deadlines map[string]int64 `json:"deadlines"`
	}{Deadlines: deadlines}

	b, err := json.Marshal(data)
	if err != nil {
		log.Printf("wake-lxc: failed to marshal state: %v", err)
		return
	}
	tmp := m.stateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		log.Printf("wake-lxc: failed to write state: %v", err)
		return
	}
	if err := os.Rename(tmp, m.stateFile); err != nil {
		log.Printf("wake-lxc: failed to rename state: %v", err)
		return
	}
}

// loadState resumes stop timers from the state file.
func (m *Middleware) loadState(ctx context.Context) {
	b, err := os.ReadFile(m.stateFile)
	if err != nil {
		return
	}
	var data struct {
		Deadlines map[string]int64 `json:"deadlines"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		log.Printf("wake-lxc: state file unreadable (%v), starting fresh", err)
		return
	}

	now := time.Now()
	for vmidStr, deadlineUnix := range data.Deadlines {
		vmid, err := strconv.ParseInt(vmidStr, 10, 64)
		if err != nil {
			continue
		}
		c, ok := m.byVMID[uint64(vmid)]
		if !ok {
			continue
		}
		deadline := time.Unix(deadlineUnix, 0)
		remaining := deadline.Sub(now)
		_, mode := internal.ResolveStop(c.labels, m.shutdownMinutes)
		if remaining <= 0 {
			log.Printf("wake-lxc: VMID %d: stop was overdue while offline, stopping now", c.vmid)
			go func() {
				if err := m.client.StopVM(ctx, m.nodeName, c.kind, c.vmid, mode); err != nil {
					log.Printf("wake-lxc: VMID %d: overdue stop failed: %v", c.vmid, err)
				}
			}()
		} else {
			log.Printf("wake-lxc: VMID %d: resuming stop timer, %.1f min remaining", c.vmid, remaining.Minutes())
			m.scheduleStopWithDelay(c, m.vmidKey(c), mode, remaining)
		}
		m.ensureWatchdog(c)
	}
}

func (m *Middleware) writeStatus(rw http.ResponseWriter, c *container) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")

	var resp struct {
		Ready   bool   `json:"ready"`
		Message string `json:"message"`
		Level   string `json:"level"`
	}

	if !m.isRunning(c) {
		key := m.vmidKey(c)
		m.mu.Lock()
		var msg, level string
		if s, ok := m.starting[key]; ok {
			msg, level = s.message, s.level
		} else {
			msg, level = "Container is stopped, starting now...", "info"
		}
		m.mu.Unlock()
		resp.Ready = false
		resp.Message = msg
		resp.Level = level
	} else {
		if c.backend != "" {
			if m.probeBackend(c.backend) {
				resp.Ready = true
				resp.Message = "Ready"
				resp.Level = "info"
			} else {
				resp.Ready = false
				resp.Message = "Container running, waiting for the app to respond..."
				resp.Level = "info"
			}
		} else {
			resp.Ready = true
			resp.Message = "Running"
			resp.Level = "info"
		}
	}

	json.NewEncoder(rw).Encode(resp)
}

func (m *Middleware) probeBackend(url string) bool {
	resp, err := m.probeClient.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}

func (m *Middleware) serveStartingPage(rw http.ResponseWriter, c *container) {
	rw.Header().Set("Content-Type", "text/html")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Write([]byte(startingPageHTML))
}

func (m *Middleware) write404(rw http.ResponseWriter) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusNotFound)
	json.NewEncoder(rw).Encode(map[string]string{
		"detail": "Domain not configured for wake.",
	})
}
