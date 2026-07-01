// Package monitor implements an active TCP health-checking background worker.
// It probes every registered physical backend every 5 seconds using a short
// dial timeout. If a backend fails 3 consecutive probes the monitor evicts it
// from the consistent hash ring so no new traffic is routed to it.
package monitor

import (
	"log"
	"net"
	"sync"
	"time"

	"github.com/tcp-proxy/ring"
)

const (
	// probeInterval is the cadence at which all backends are probed.
	probeInterval = 5 * time.Second

	// dialTimeout is the maximum time allowed for a TCP dial health check.
	dialTimeout = 2 * time.Second

	// maxConsecutiveFailures is the number of consecutive probe failures that
	// must occur before a backend is removed from the ring.
	maxConsecutiveFailures = 3
)

// backendState tracks consecutive failure counts for a single physical backend.
type backendState struct {
	consecutiveFailures int
	evicted             bool
}

// HealthMonitor manages the lifecycle of active health probes.
// It holds a reference to the hash ring so it can remove dead backends.
type HealthMonitor struct {
	mu       sync.Mutex
	backends map[string]*backendState // physical addr → probe state
	ring     *ring.HashRing
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// New creates a HealthMonitor that will probe the supplied backends against
// the given ring. The monitor is not started until Run is called.
func New(r *ring.HashRing, backends []string) *HealthMonitor {
	states := make(map[string]*backendState, len(backends))
	for _, addr := range backends {
		states[addr] = &backendState{}
	}
	return &HealthMonitor{
		backends: states,
		ring:     r,
		stopCh:   make(chan struct{}),
	}
}

// Run starts the background probing loop in its own goroutine. It returns
// immediately. Call Stop to shut the monitor down gracefully.
func (m *HealthMonitor) Run() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.loop()
	}()
}

// Stop signals the monitor to cease probing and waits for the goroutine to
// exit cleanly.
func (m *HealthMonitor) Stop() {
	close(m.stopCh)
	m.wg.Wait()
}

// loop is the main probe loop. It fires a full probe cycle immediately on
// startup and then waits probeInterval before each subsequent cycle.
func (m *HealthMonitor) loop() {
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()

	// Probe immediately on start so the system is validated before the
	// first client connection arrives.
	m.probeAll()

	for {
		select {
		case <-ticker.C:
			m.probeAll()
		case <-m.stopCh:
			log.Println("[monitor] health monitor stopped")
			return
		}
	}
}

// probeAll iterates over every registered backend and probes it once.
func (m *HealthMonitor) probeAll() {
	m.mu.Lock()
	// Snapshot the backend list to avoid holding the lock during I/O.
	addrs := make([]string, 0, len(m.backends))
	for addr := range m.backends {
		addrs = append(addrs, addr)
	}
	m.mu.Unlock()

	for _, addr := range addrs {
		m.probe(addr)
	}
}

// probe performs a single TCP dial check against addr and updates its
// failure counter. If the counter crosses the threshold the backend is
// removed from the ring.
func (m *HealthMonitor) probe(addr string) {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		m.recordFailure(addr, err)
		return
	}
	// Close the probe connection; we only need to confirm reachability.
	if closeErr := conn.Close(); closeErr != nil {
		log.Printf("[monitor] warning: could not close probe connection to %s: %v", addr, closeErr)
	}
	m.recordSuccess(addr)
}

// recordSuccess resets the consecutive failure counter for addr and, if the
// backend was previously evicted, re-adds it to the ring.
func (m *HealthMonitor) recordSuccess(addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, ok := m.backends[addr]
	if !ok {
		return
	}

	if state.evicted {
		log.Printf("[monitor] backend %s is healthy again – re-adding to ring", addr)
		m.ring.AddNode(addr)
		state.evicted = false
	}
	state.consecutiveFailures = 0
}

// recordFailure increments the consecutive failure counter for addr.
// If the counter reaches maxConsecutiveFailures the backend is evicted from
// the ring (but kept in m.backends so it can be re-added when it recovers).
func (m *HealthMonitor) recordFailure(addr string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, ok := m.backends[addr]
	if !ok {
		return
	}

	state.consecutiveFailures++
	log.Printf("[monitor] probe FAILED for %s (%v) – consecutive failures: %d/%d",
		addr, err, state.consecutiveFailures, maxConsecutiveFailures)

	if state.consecutiveFailures >= maxConsecutiveFailures && !state.evicted {
		log.Printf("[monitor] evicting backend %s from ring after %d consecutive failures",
			addr, state.consecutiveFailures)
		m.ring.RemoveNode(addr)
		state.evicted = true
	}
}

// RegisterBackend dynamically adds a new backend to the monitor and the ring.
// If the backend is already tracked this call is a no-op.
func (m *HealthMonitor) RegisterBackend(addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.backends[addr]; exists {
		return
	}
	m.backends[addr] = &backendState{}
	m.ring.AddNode(addr)
	log.Printf("[monitor] registered new backend %s", addr)
}

// DeregisterBackend removes a backend from monitoring and from the ring.
// This is intended for planned maintenance rather than failure-driven eviction.
func (m *HealthMonitor) DeregisterBackend(addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.backends, addr)
	m.ring.RemoveNode(addr)
	log.Printf("[monitor] deregistered backend %s", addr)
}
