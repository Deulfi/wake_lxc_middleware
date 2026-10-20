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
	"sync/atomic"
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

// SharedState is the discovery/state machinery shared by every Middleware
// wrapper that targets the same apiEndpoint+nodeName. It is created once
// (see instanceRegistry) and its background goroutines (discoveryLoop,
// per-container watchdogs) run for the process lifetime.
type SharedState struct {
	client  *internal.ProxmoxClient
	nodeName string
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

// Middleware is the per-router wrapper that Traefik calls ServeHTTP on.
// Each router gets its own Middleware instance (and therefore its own next
// handler), but they all share the same SharedState for discovery and
// stop/start state. This is what fixes the redirect-to-wrong-backend bug:
// the next handler is no longer overwritten by a sibling router's reload.
type Middleware struct {
	shared *SharedState
	next   atomic.Value // holds http.Handler, per-router
	name   string
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

// instanceRegistry holds the one live SharedState per apiEndpoint+nodeName,
// at package level so it survives Traefik re-instantiating the plugin.
//
// Without this, every New call (Traefik calls it on every dynamic-config
// reload anywhere in the instance, not just changes to this middleware)
// would start a brand new discoveryLoop goroutine and a fresh set of
// per-container watchdogs. On Traefik v3 the old instance's ctx is
// cancelled on reload so those goroutines exit cleanly; on Traefik v2 it is
// not, so they leak forever, each one polling the Proxmox API on its own
// schedule. Reusing the SharedState removes the dependency on that behavior
// entirely — it is then correct on any Traefik version.
//
// Only the cheap, non-background-affecting scalars (checkInterval,
// shutdownMinutes, domainTemplate, stateFile) are refreshed on reuse.
// apiToken/apiEndpoint/apiValidateSSL/apiLogging and discoveryInterval are
// intentionally NOT hot-swapped — changing Proxmox credentials or the
// discovery interval still requires a real Traefik restart to take effect,
// since the client and the discovery ticker are already running.
var instanceRegistry = struct {
	sync.Mutex
	byKey map[string]*SharedState
}{byKey: make(map[string]*SharedState)}

func registryKey(apiEndpoint, nodeName string) string {
	return apiEndpoint + "|" + nodeName
}

// New creates (or reuses) the SharedState and returns a fresh per-router
// Middleware wrapper. The SharedState (discovery loop, watchdogs, stop
// timers) is shared across all routers targeting the same apiEndpoint+
// nodeName; the Middleware wrapper (next handler) is per-router, so each
// router's backend handler is independent and never overwritten by a
// sibling router's reload.
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

	// Get or create the shared state for this apiEndpoint+nodeName.
	key := registryKey(config.ApiEndpoint, config.NodeName)
	instanceRegistry.Lock()
	shared, ok := instanceRegistry.byKey[key]
	if !ok {
		shared = &SharedState{
			client:              internal.NewProxmoxClient(config.ApiEndpoint, config.ApiTokenId, config.ApiToken, validateSSL, logLevel),
			nodeName:            config.NodeName,
			discoveryInterval:   discoveryInterval,
			checkInterval:       checkInterval,
			shutdownMinutes:     shutdownMinutes,
			domainTemplate:      config.DomainTemplate,
			stateFile:           config.StateFile,
			domains:             make(map[string]*container),
			byVMID:              make(map[uint64]*container),
			starting:            make(map[string]*startState),
			startLocks:          make(map[string]*tryLock),
			watchdogs:           make(map[string]struct{}),
			breakers:            make(map[string]time.Time),
			ipamCache:           make(map[string]string),
			probeClient:         &http.Client{Timeout: 2 * time.Second},
		}
		// Register before the discovery pass so a concurrent New for the
		// same key can never create a second SharedState.
		instanceRegistry.byKey[key] = shared
	} else {
		// Refresh the cheap scalars on reload.
		shared.mu.Lock()
		shared.checkInterval = checkInterval
		shared.shutdownMinutes = shutdownMinutes
		shared.domainTemplate = config.DomainTemplate
		shared.stateFile = config.StateFile
		shared.mu.Unlock()
	}
	instanceRegistry.Unlock()

	// Always return a fresh per-router wrapper.
	m := &Middleware{shared: shared, name: name}
	m.next.Store(next)

	// First discovery pass + state resume + background loop only on first
	// creation (the SharedState was just created, not reused).
	if !ok {
		if err := shared.discover(ctx); err != nil {
			log.Printf("wake-lxc: initial discovery pass failed: %v", err)
		}
		if config.StateFile != "" {
			shared.loadState(ctx)
		}
		go shared.discoveryLoop(context.Background())
	}

	return m, nil
}

// ServeHTTP is the request path.
func (m *Middleware) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	host := strings.ToLower(req.Host)
	c := m.shared.lookup(host)
	if c == nil {
		m.write404(rw)
		return
	}
	if req.URL.Path == "/__wake__/status" {
		m.shared.writeStatus(rw, c)
		return
	}
	if m.shared.isRunning(c) {
		m.shared.clearStarting(c)
		m.shared.scheduleStop(c)
		m.shared.ensureWatchdog(c)
		m.next.Load().(http.Handler).ServeHTTP(rw, req)
		return
	}
	m.shared.ensureStartCycle(c)
	m.shared.ensureWatchdog(c)
	m.serveStartingPage(rw, c)
}

func (s *SharedState) lookup(host string) *container {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.domains[host]
}

func (s *SharedState) vmidKey(c *container) string {
	return strconv.FormatInt(int64(c.vmid), 10)
}

// isRunning checks the container's Proxmox status. Error → false (records a
// failure); running → true (resets failures); stopped → false.
func (s *SharedState) isRunning(c *container) bool {
	status, err := s.client.GetStatus(context.Background(), s.nodeName, c.kind, c.vmid)
	if err != nil {
		log.Printf("wake-lxc: VMID %d: status check failed: %v", c.vmid, err)
		s.recordFailure(c)
		return false
	}
	if status != c.lastStatus {
		log.Printf("wake-lxc: VMID %d is %s", c.vmid, status)
		c.lastStatus = status
	}
	if status == "running" {
		s.resetFailures(c)
		return true
	}
	return false
}

func (s *SharedState) recordFailure(c *container) {
	key := s.vmidKey(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.breakers[key] = time.Now()
}

func (s *SharedState) resetFailures(c *container) {
	key := s.vmidKey(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.breakers, key)
}

func (s *SharedState) clearStarting(c *container) {
	key := s.vmidKey(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.starting, key)
}

func (s *SharedState) setStartState(key, message, level string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.starting[key]; ok {
		st.message = message
		st.level = level
	}
}

func (s *SharedState) getStartLock(key string) *tryLock {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.startLocks[key]; ok {
		return l
	}
	l := &tryLock{}
	s.startLocks[key] = l
	return l
}

// ensureStartCycle sends a start command for a stopped container (once per
// request, guarded by the start lock and circuit breaker).
func (s *SharedState) ensureStartCycle(c *container) {
	key := s.vmidKey(c)

	s.mu.Lock()
	if _, ok := s.starting[key]; ok {
		s.mu.Unlock()
		return
	}
	s.starting[key] = &startState{startedAt: time.Now(), message: "Container is stopped, starting now...", level: "info"}
	s.mu.Unlock()

	lock := s.getStartLock(key)
	if !lock.try() {
		return
	}
	defer lock.unlock()

	if s.isRunning(c) {
		s.setStartState(key, "Container is already running.", "info")
		return
	}

	s.mu.Lock()
	if last, ok := s.breakers[key]; ok {
		if time.Since(last) < 300*time.Second {
			s.mu.Unlock()
			s.setStartState(key, "Recent start failures, waiting before retrying...", "error")
			return
		}
		delete(s.breakers, key)
	}
	s.mu.Unlock()

	if err := s.client.StartVM(context.Background(), s.nodeName, c.kind, c.vmid); err != nil {
		s.mu.Lock()
		s.breakers[key] = time.Now()
		s.mu.Unlock()
		s.setStartState(key, "Failed to start container: "+err.Error(), "error")
		log.Printf("wake-lxc: VMID %d: start failed: %v", c.vmid, err)
	} else {
		s.mu.Lock()
		delete(s.breakers, key)
		s.mu.Unlock()
		s.setStartState(key, "Start command sent successfully.", "info")
		log.Printf("wake-lxc: VMID %d: start command sent", c.vmid)
	}
}

// scheduleStop schedules a stop timer with the full duration (reschedule-on-access).
func (s *SharedState) scheduleStop(c *container) {
	key := s.vmidKey(c)
	minutes, mode := internal.ResolveStop(c.labels, s.shutdownMinutes)
	s.scheduleStopWithDelay(c, key, mode, time.Duration(minutes*float64(time.Minute)))
}

func (s *SharedState) scheduleStopWithDelay(c *container, key, mode string, delay time.Duration) {
	ns := nodeStop(s.nodeName)
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
		s.onStopTimer(c, key, mode, timer)
	}()

	s.saveState()
}

func (s *SharedState) onStopTimer(c *container, key, mode string, timer *time.Timer) {
	ns := nodeStop(s.nodeName)
	stopState.Lock()
	if cur, ok := ns.timers[key]; ok && cur != timer {
		stopState.Unlock()
		return
	}
	delete(ns.timers, key)
	delete(ns.deadlines, key)
	delay := ns.lastDelay[key]
	stopState.Unlock()

	s.saveState()
	log.Printf("wake-lxc: VMID %d: stop timer fired (scheduled: %.1f min)", c.vmid, delay.Minutes())

	if s.isRunning(c) {
		if err := s.client.StopVM(context.Background(), s.nodeName, c.kind, c.vmid, mode); err != nil {
			log.Printf("wake-lxc: VMID %d: shutdown failed: %v", c.vmid, err)
		} else {
			log.Printf("wake-lxc: VMID %d: shutdown command sent", c.vmid)
		}
	} else {
		log.Printf("wake-lxc: VMID %d: already stopped, nothing to do", c.vmid)
	}
}

func (s *SharedState) cancelStop(key string) {
	ns := nodeStop(s.nodeName)
	stopState.Lock()
	if t, ok := ns.timers[key]; ok {
		t.Stop()
		delete(ns.timers, key)
	}
	delete(ns.deadlines, key)
	stopState.Unlock()
	s.saveState()
}

// ensureWatchdog starts one watchdog goroutine per container. It self-
// terminates when the stop timer is removed or the container stops externally.
func (s *SharedState) ensureWatchdog(c *container) {
	key := s.vmidKey(c)
	s.mu.Lock()
	if _, ok := s.watchdogs[key]; ok {
		s.mu.Unlock()
		return
	}
	s.watchdogs[key] = struct{}{}
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.watchdogs, key)
			s.mu.Unlock()
		}()
		ticker := time.NewTicker(s.checkInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ns := nodeStop(s.nodeName)
				stopState.Lock()
				hasTimer := ns.timers[key] != nil
				stopState.Unlock()
				if !hasTimer {
					return
				}
				if !s.isRunning(c) {
					log.Printf("wake-lxc: VMID %d: stopped externally, cancelling pending stop timer", c.vmid)
					s.cancelStop(key)
					return
				}
			}
		}
	}()
}

// discover runs one discovery pass: lists LXC/QEMU, reads configs, resolves
// wake labels, and swaps the domain/VMID maps.
func (s *SharedState) discover(ctx context.Context) error {
	ipamMap := s.getIPAMMap(ctx)

	domains := make(map[string]*container)
	byVMID := make(map[uint64]*container)

	lxcRefs, err := s.client.ListLXC(ctx, s.nodeName)
	if err != nil {
		return err
	}
	for _, ref := range lxcRefs {
		s.discoverOne(ctx, "lxc", ref, ipamMap, domains, byVMID)
	}

	qemuRefs, err := s.client.ListQEMU(ctx, s.nodeName)
	if err != nil {
		return err
	}
	for _, ref := range qemuRefs {
		s.discoverOne(ctx, "qemu", ref, ipamMap, domains, byVMID)
	}

	// Detect label changes and membership changes against the package-level
	// baseline (lastSeen), which survives Middleware recreation. An empty
	// baseline means no prior pass has run; that's initialization, not a
	// change, so it's reported silently.
	lastSeen.Lock()
	prev := lastSeen.byNode[s.nodeName]
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
	lastSeen.byNode[s.nodeName] = byVMID
	lastSeen.Unlock()

	s.mu.Lock()
	s.domains = domains
	s.byVMID = byVMID
	s.mu.Unlock()

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
		key := s.vmidKey(c)
		ns := nodeStop(s.nodeName)
		stopState.Lock()
		hasTimer := ns.timers[key] != nil
		stopState.Unlock()
		if hasTimer {
			s.scheduleStop(c)
		}
	}
	return nil
}

func (s *SharedState) discoverOne(ctx context.Context, kind string, ref internal.VMRef, ipamMap map[string]string, domains map[string]*container, byVMID map[uint64]*container) {
	cfg, err := s.client.GetVMConfig(ctx, s.nodeName, kind, ref.VMID)
	if err != nil {
		log.Printf("wake-lxc: VMID %d: discovery check failed, skipping this pass: %v", ref.VMID, err)
		return
	}
	l := internal.ParseWakeLabels(cfg.Description)
	if !l.Enable {
		return
	}
	isLXC := kind == "lxc"
	domain, err := internal.ResolveDomain(l, s.domainTemplate, cfg.Hostname, isLXC)
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
func (s *SharedState) getIPAMMap(ctx context.Context) map[string]string {
	s.mu.Lock()
	if !s.ipamCacheAt.IsZero() && time.Since(s.ipamCacheAt) < 30*time.Second {
		cached := s.ipamCache
		s.mu.Unlock()
		return cached
	}
	s.mu.Unlock()

	ipams, err := s.client.GetIPAMs(ctx)
	if err != nil {
		log.Printf("wake-lxc: IPAM lookup failed: %v", err)
		return nil
	}
	result := make(map[string]string)
	for _, ipam := range ipams {
		if ipam.IPAMID == "" {
			continue
		}
		status, err := s.client.GetIPAMStatus(ctx, ipam.IPAMID)
		if err != nil {
			log.Printf("wake-lxc: IPAM %s status failed: %v", ipam.IPAMID, err)
			continue
		}
		for _, rec := range status.Data {
			result[strconv.FormatInt(int64(rec.VMID), 10)] = rec.IP
		}
	}
	s.mu.Lock()
	s.ipamCache = result
	s.ipamCacheAt = time.Now()
	s.mu.Unlock()
	return result
}

func (s *SharedState) discoveryLoop(ctx context.Context) {
	ticker := time.NewTicker(s.discoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := s.discover(ctx); err != nil {
				log.Printf("wake-lxc: discovery loop failed: %v", err)
			}
		}
	}
}

// saveState atomically writes the deadlines map to the state file.
func (s *SharedState) saveState() {
	if s.stateFile == "" {
		return
	}
	ns := nodeStop(s.nodeName)
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
	tmp := s.stateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		log.Printf("wake-lxc: failed to write state: %v", err)
		return
	}
	if err := os.Rename(tmp, s.stateFile); err != nil {
		log.Printf("wake-lxc: failed to rename state: %v", err)
		return
	}
}

// loadState resumes stop timers from the state file.
func (s *SharedState) loadState(ctx context.Context) {
	b, err := os.ReadFile(s.stateFile)
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
		c, ok := s.byVMID[uint64(vmid)]
		if !ok {
			continue
		}
		deadline := time.Unix(deadlineUnix, 0)
		remaining := deadline.Sub(now)
		_, mode := internal.ResolveStop(c.labels, s.shutdownMinutes)
		if remaining <= 0 {
			log.Printf("wake-lxc: VMID %d: stop was overdue while offline, stopping now", c.vmid)
			go func() {
				if err := s.client.StopVM(ctx, s.nodeName, c.kind, c.vmid, mode); err != nil {
					log.Printf("wake-lxc: VMID %d: overdue stop failed: %v", c.vmid, err)
				}
			}()
		} else {
			log.Printf("wake-lxc: VMID %d: resuming stop timer, %.1f min remaining", c.vmid, remaining.Minutes())
			s.scheduleStopWithDelay(c, s.vmidKey(c), mode, remaining)
		}
		s.ensureWatchdog(c)
	}
}

func (s *SharedState) writeStatus(rw http.ResponseWriter, c *container) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")

	var resp struct {
		Ready   bool   `json:"ready"`
		Message string `json:"message"`
		Level   string `json:"level"`
	}

	if !s.isRunning(c) {
		key := s.vmidKey(c)
		s.mu.Lock()
		var msg, level string
		if st, ok := s.starting[key]; ok {
			msg, level = st.message, st.level
		} else {
			msg, level = "Container is stopped, starting now...", "info"
		}
		s.mu.Unlock()
		resp.Ready = false
		resp.Message = msg
		resp.Level = level
	} else {
		if c.backend != "" {
			if s.probeBackend(c.backend) {
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

func (s *SharedState) probeBackend(url string) bool {
	resp, err := s.probeClient.Get(url)
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
