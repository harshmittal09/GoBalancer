// Package proxy — pool.go
//
// BackendPool implements a per-backend channel-based connection pool.
// It reuses persistent TCP connections to backends instead of dialling fresh
// on every client request, which eliminates the TCP handshake cost for
// short-lived client sessions.
//
// Design decisions:
//   - Each backend has its own buffered channel acting as a LIFO idle queue.
//     (A buffered channel naturally provides bounded-size, goroutine-safe
//     access with zero lock contention on the hot path.)
//   - Connections are validated before being returned to callers. A zero-byte
//     read with an immediate deadline is used to detect half-open or dead
//     connections without blocking.
//   - The pool is bounded (maxIdlePerBackend). Connections returned when the
//     pool is full are simply closed, preventing unbounded memory growth.
package proxy

import (
	"net"
	"sync"
	"time"
)

const (
	// maxIdlePerBackend is the maximum number of idle, reusable connections
	// the pool will hold open for a single backend address.
	maxIdlePerBackend = 32

	// poolDialTimeout is the timeout for creating a fresh backend connection.
	poolDialTimeout = 5 * time.Second

	// idleConnTTL is the maximum time a pooled connection may sit idle before
	// it is considered stale and discarded.
	idleConnTTL = 90 * time.Second
)

// pooledConn wraps a net.Conn with the time it was returned to the pool,
// so we can evict connections that have sat idle too long.
type pooledConn struct {
	conn     net.Conn
	idleSince time.Time
}

// BackendPool manages a set of idle TCP connections to each backend address.
// It is safe for concurrent use by multiple goroutines.
type BackendPool struct {
	mu    sync.Mutex
	pools map[string]chan pooledConn
}

// NewBackendPool returns an initialised, empty BackendPool.
func NewBackendPool() *BackendPool {
	return &BackendPool{
		pools: make(map[string]chan pooledConn),
	}
}

// Get returns a usable connection to addr.  It tries to reuse an idle pooled
// connection first; if none is available (or all pooled connections are stale)
// it dials a fresh connection.
func (p *BackendPool) Get(addr string) (net.Conn, error) {
	ch := p.poolFor(addr)

	// Drain the idle queue until we find a healthy connection or exhaust it.
	for {
		select {
		case pc := <-ch:
			if isStale(pc) {
				// Connection sat idle past its TTL; close and try next.
				pc.conn.Close() //nolint:errcheck
				continue
			}
			if !isAlive(pc.conn) {
				pc.conn.Close() //nolint:errcheck
				continue
			}
			// Happy path: reuse the pooled connection.
			return pc.conn, nil
		default:
			// Pool is empty; fall through to dial a fresh connection.
			return net.DialTimeout("tcp", addr, poolDialTimeout)
		}
	}
}

// Put returns conn to the pool for future reuse.  If the pool for addr is
// already at capacity the connection is closed immediately.
func (p *BackendPool) Put(addr string, conn net.Conn) {
	ch := p.poolFor(addr)

	pc := pooledConn{conn: conn, idleSince: time.Now()}
	select {
	case ch <- pc:
		// Returned to pool successfully.
	default:
		// Pool is full; discard the connection rather than blocking.
		conn.Close() //nolint:errcheck
	}
}

// Close drains and closes every idle connection across all pools.
// Call this during server shutdown.
func (p *BackendPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for addr, ch := range p.pools {
		close(ch)
		for pc := range ch {
			pc.conn.Close() //nolint:errcheck
		}
		delete(p.pools, addr)
	}
}

// poolFor returns (and lazily creates) the idle connection channel for addr.
func (p *BackendPool) poolFor(addr string) chan pooledConn {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ch, ok := p.pools[addr]; ok {
		return ch
	}
	ch := make(chan pooledConn, maxIdlePerBackend)
	p.pools[addr] = ch
	return ch
}

// isStale reports whether a pooled connection has exceeded its idle TTL.
func isStale(pc pooledConn) bool {
	return time.Since(pc.idleSince) > idleConnTTL
}

// isAlive performs a non-blocking health check on conn by setting an immediate
// deadline and attempting a zero-byte read.
//
//   - If the peer closed the connection we will get io.EOF immediately.
//   - If the connection is still healthy, SetDeadline causes Read to return
//     a timeout error, which we treat as "alive".
//   - We always reset the deadline to zero (no timeout) before returning.
func isAlive(conn net.Conn) bool {
	// Set a 1ms deadline so the read returns almost immediately.
	_ = conn.SetReadDeadline(time.Now().Add(time.Millisecond))
	defer conn.SetReadDeadline(time.Time{}) //nolint:errcheck // reset deadline

	var buf [1]byte
	_, err := conn.Read(buf[:])
	if err == nil {
		// Data available on an idle connection is unexpected; treat as unhealthy.
		return false
	}

	netErr, ok := err.(net.Error)
	if ok && netErr.Timeout() {
		// Deadline exceeded with no data — connection is healthy and idle.
		return true
	}

	// Any other error (io.EOF, connection reset, etc.) means the peer closed.
	return false
}
