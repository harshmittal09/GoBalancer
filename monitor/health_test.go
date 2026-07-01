package monitor

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/tcp-proxy/ring"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// fakeServer is a controllable TCP listener used to simulate healthy/failing
// backends.
type fakeServer struct {
	l    net.Listener
	addr string

	mu      sync.Mutex
	paused  bool   // when true, Accept returns immediately (simulates failure)
	stopped bool
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakeServer listen: %v", err)
	}
	fs := &fakeServer{l: l, addr: l.Addr().String()}
	go fs.serve()
	return fs
}

func (fs *fakeServer) serve() {
	for {
		conn, err := fs.l.Accept()
		if err != nil {
			return
		}
		go conn.Close() //nolint:errcheck
	}
}

// pause closes the listener so that dial attempts fail.
func (fs *fakeServer) pause(t *testing.T) {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !fs.paused {
		fs.l.Close() //nolint:errcheck
		fs.paused = true
	}
}

// resume re-opens the listener on the same address.
func (fs *fakeServer) resume(t *testing.T) {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.paused && !fs.stopped {
		l, err := net.Listen("tcp", fs.addr)
		if err != nil {
			t.Logf("fakeServer resume: %v", err)
			return
		}
		fs.l = l
		fs.paused = false
		go fs.serve()
	}
}

func (fs *fakeServer) stop() {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !fs.stopped {
		fs.l.Close() //nolint:errcheck
		fs.stopped = true
	}
}

// overrideProbeInterval patches the package-level constant for fast tests.
// It returns a restore function. Call defer restore() after this.
// NOTE: Since Go constants cannot be changed at runtime, we expose helpers
// that build a monitor with a custom ticker – see newFastMonitor below.

// newFastMonitor creates a HealthMonitor that uses a 100 ms probe interval
// and a 200 ms dial timeout, allowing tests to observe eviction quickly.
func newFastMonitor(r *ring.HashRing, backends []string) *HealthMonitor {
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

// runFastLoop starts a probe loop that fires every interval instead of the
// package-level 5 s constant. Intended for unit tests only.
func runFastLoop(m *HealthMonitor, interval, dialTO time.Duration) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				for _, addr := range m.snapshotAddrs() {
					conn, err := net.DialTimeout("tcp", addr, dialTO)
					if err != nil {
						m.recordFailure(addr, err)
					} else {
						conn.Close() //nolint:errcheck
						m.recordSuccess(addr)
					}
				}
			case <-m.stopCh:
				return
			}
		}
	}()
}

// snapshotAddrs returns a copy of the monitored backend addresses.
func (m *HealthMonitor) snapshotAddrs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	addrs := make([]string, 0, len(m.backends))
	for a := range m.backends {
		addrs = append(addrs, a)
	}
	return addrs
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestMonitor_HealthyBackendStaysInRing verifies that a healthy backend is
// never evicted.
func TestMonitor_HealthyBackendStaysInRing(t *testing.T) {
	fs := newFakeServer(t)
	defer fs.stop()

	r := ring.New()
	m := newFastMonitor(r, []string{fs.addr})
	r.AddNode(fs.addr) // seed the ring as main.go does

	runFastLoop(m, 100*time.Millisecond, 200*time.Millisecond)
	defer m.Stop()

	time.Sleep(600 * time.Millisecond) // 6 probe cycles

	if r.Len() == 0 {
		t.Error("healthy backend was evicted from ring")
	}
	if r.GetNode("any-client-ip") != fs.addr {
		t.Errorf("expected ring to route to %s, got %q", fs.addr, r.GetNode("any-client-ip"))
	}
}

// TestMonitor_FailingBackendEvicted verifies that a backend is removed after
// maxConsecutiveFailures consecutive probe failures.
func TestMonitor_FailingBackendEvicted(t *testing.T) {
	fs := newFakeServer(t)
	r := ring.New()
	m := newFastMonitor(r, []string{fs.addr})
	r.AddNode(fs.addr)

	// Pause the server before the monitor starts – every probe will fail.
	fs.pause(t)
	defer fs.stop()

	runFastLoop(m, 100*time.Millisecond, 50*time.Millisecond)
	defer m.Stop()

	// Wait long enough for maxConsecutiveFailures (3) probes to complete.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if r.Len() == 0 {
			return // evicted as expected
		}
	}
	t.Errorf("backend was not evicted after %d consecutive failures", maxConsecutiveFailures)
}

// TestMonitor_RecoveryReAddsBackend verifies that a backend re-joins the ring
// automatically once it starts passing probes again.
func TestMonitor_RecoveryReAddsBackend(t *testing.T) {
	fs := newFakeServer(t)
	r := ring.New()
	m := newFastMonitor(r, []string{fs.addr})
	r.AddNode(fs.addr)

	// Trigger eviction first.
	fs.pause(t)

	runFastLoop(m, 100*time.Millisecond, 50*time.Millisecond)
	defer m.Stop()

	// Wait for eviction.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if r.Len() == 0 {
			break
		}
	}
	if r.Len() != 0 {
		t.Fatal("backend was not evicted; cannot test recovery")
	}

	// Bring the server back up.
	fs.resume(t)
	t.Logf("backend restarted at %s", fs.addr)

	// Wait for re-registration.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if r.Len() > 0 {
			return // recovered
		}
	}
	t.Error("backend was not re-added to ring after recovery")
}

// TestMonitor_DeregisterBackend verifies that explicitly deregistering a
// backend removes it from both monitoring and the ring.
func TestMonitor_DeregisterBackend(t *testing.T) {
	fs1 := newFakeServer(t)
	fs2 := newFakeServer(t)
	defer fs1.stop()
	defer fs2.stop()

	r := ring.New()
	r.AddNode(fs1.addr)
	r.AddNode(fs2.addr)

	m := newFastMonitor(r, []string{fs1.addr, fs2.addr})
	runFastLoop(m, 200*time.Millisecond, 200*time.Millisecond)
	defer m.Stop()

	m.DeregisterBackend(fs2.addr)

	// Ring must still contain fs1's vnodes and none of fs2's.
	if r.Len() != 100 {
		t.Errorf("expected 100 vnodes after deregister, got %d", r.Len())
	}
	for i := 0; i < 500; i++ {
		ip := "10.0.0." + string(rune('0'+i%10))
		if node := r.GetNode(ip); node == fs2.addr {
			t.Errorf("deregistered backend %s still in ring for IP %s", fs2.addr, ip)
			break
		}
	}
}

// TestMonitor_RegisterBackend verifies that dynamically registering a new
// backend adds it to both the ring and probe rotation.
func TestMonitor_RegisterBackend(t *testing.T) {
	fs := newFakeServer(t)
	defer fs.stop()

	r := ring.New()
	m := newFastMonitor(r, nil) // start with no backends
	runFastLoop(m, 200*time.Millisecond, 200*time.Millisecond)
	defer m.Stop()

	if r.Len() != 0 {
		t.Fatalf("expected empty ring before registration, got %d", r.Len())
	}

	m.RegisterBackend(fs.addr)

	if r.Len() != 100 {
		t.Errorf("expected 100 vnodes after register, got %d", r.Len())
	}
}

// TestMonitor_ConcurrentProbes ensures the monitor is race-condition free
// when multiple probe goroutines race to update state simultaneously.
func TestMonitor_ConcurrentProbes(t *testing.T) {
	const n = 5
	servers := make([]*fakeServer, n)
	addrs := make([]string, n)
	for i := range servers {
		servers[i] = newFakeServer(t)
		addrs[i] = servers[i].addr
		defer servers[i].stop()
	}

	r := ring.New()
	for _, a := range addrs {
		r.AddNode(a)
	}
	m := newFastMonitor(r, addrs)

	// Run probes from multiple goroutines simultaneously to trigger the race
	// detector if state is not properly locked.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, a := range addrs {
				conn, err := net.DialTimeout("tcp", a, 200*time.Millisecond)
				if err != nil {
					m.recordFailure(a, err)
				} else {
					conn.Close() //nolint:errcheck
					m.recordSuccess(a)
				}
			}
		}()
	}
	wg.Wait()
	// If the race detector didn't fire, the test passes.
}
