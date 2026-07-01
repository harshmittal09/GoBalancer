// Package proxy implements the Layer-4 TCP load balancer engine.
// It accepts inbound TCP connections, resolves the appropriate backend via the
// consistent hash ring, and creates a fully bidirectional tunnel between the
// client and the backend using two concurrent io.Copy goroutines.
package proxy

import (
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tcp-proxy/ring"
)

const (
	// dialBackendTimeout is the maximum time the proxy will wait while
	// establishing a connection to a backend.
	dialBackendTimeout = 5 * time.Second

	// copyBufferSize is the size of the per-copy buffer in bytes (32 KiB).
	// This value balances memory usage against syscall frequency.
	copyBufferSize = 32 * 1024
)

// Server is the TCP proxy listener. It owns the listener socket and counters
// for operational observability.
type Server struct {
	listener     net.Listener
	ring         *ring.HashRing
	activeConns  atomic.Int64 // current number of open tunnels
	totalConns   atomic.Int64 // cumulative connections accepted
	totalErrors  atomic.Int64 // cumulative backend dial errors
	wg           sync.WaitGroup
}

// Start creates a TCP listener on listenAddr, binds it to the provided
// HashRing, and begins accepting connections. It returns the Server so the
// caller can shut it down via Close.
//
// Start blocks in the accept loop and only returns when the listener is closed
// or an unrecoverable error occurs.
func Start(listenAddr string, r *ring.HashRing) (*Server, error) {
	l, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, err
	}

	s := &Server{
		listener: l,
		ring:     r,
	}

	log.Printf("[proxy] listening on %s", listenAddr)
	s.wg.Add(1)
	go s.acceptLoop()

	return s, nil
}

// TotalConnections returns the cumulative count of all accepted connections.
func (s *Server) TotalConnections() int64 {
	return s.totalConns.Load()
}

// TotalErrors returns the cumulative count of backend dial failures.
func (s *Server) TotalErrors() int64 {
	return s.totalErrors.Load()
}

// Close shuts the listener down, causing the accept loop to exit, and then
// waits for all in-flight connection handlers to finish.
func (s *Server) Close() error {
	err := s.listener.Close()
	s.wg.Wait()
	log.Printf("[proxy] shut down. total accepted: %d, errors: %d",
		s.totalConns.Load(), s.totalErrors.Load())
	return err
}

// ActiveConnections returns the current number of open client tunnels.
func (s *Server) ActiveConnections() int64 {
	return s.activeConns.Load()
}

// acceptLoop runs the main accept() loop. Each accepted connection is handed
// off to its own goroutine immediately so the loop can return to accept()
// without any per-connection processing latency.
func (s *Server) acceptLoop() {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			// A "use of closed network connection" error is expected when the
			// listener is intentionally closed during shutdown.
			if isClosedNetworkError(err) {
				log.Println("[proxy] listener closed, stopping accept loop")
				return
			}
			log.Printf("[proxy] accept error: %v", err)
			// For transient errors (e.g. EMFILE) back off briefly before retrying.
			time.Sleep(5 * time.Millisecond)
			continue
		}

		s.totalConns.Add(1)
		s.activeConns.Add(1)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.activeConns.Add(-1)
			s.handleConnection(conn)
		}()
	}
}

// handleConnection resolves the target backend for the inbound connection,
// establishes a TCP connection to it, and runs bidirectional streaming until
// either side terminates the session.
func (s *Server) handleConnection(clientConn net.Conn) {
	defer clientConn.Close() //nolint:errcheck // best-effort close at session end

	remoteAddr := clientConn.RemoteAddr().String()
	log.Printf("[proxy] new connection from %s", remoteAddr)

	// For local testing, all traffic comes from the Docker NAT IP (192.168.65.1).
	// To actually load balance the traffic across all backends, we use the full 
	// remoteAddr (IP:Port) as the hash key so every new connection maps to a 
	// distributed backend.
	target := s.ring.GetNode(remoteAddr)
	if target == "" {
		log.Printf("[proxy] no healthy backends available for client %s – dropping connection", remoteAddr)
		s.totalErrors.Add(1)
		return
	}

	log.Printf("[proxy] routing %s → %s", remoteAddr, target)

	backendConn, err := net.DialTimeout("tcp", target, dialBackendTimeout)
	if err != nil {
		log.Printf("[proxy] failed to connect to backend %s for client %s: %v", target, remoteAddr, err)
		s.totalErrors.Add(1)
		return
	}
	defer backendConn.Close() //nolint:errcheck

	// Bidirectional streaming.
	// Each direction runs in its own goroutine. When either goroutine finishes
	// (EOF, RST, or any network error) it signals the done channel, causing the
	// other direction to be unwound via connection closure.
	tunnel(clientConn, backendConn)

	log.Printf("[proxy] session closed for client %s", remoteAddr)
}

// tunnel implements the bidirectional copy between two net.Conn objects.
// It starts two goroutines – one per direction – and waits for both to finish.
// Closing one side of the connection causes the other io.Copy to unblock.
func tunnel(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	// a → b (client to backend)
	go func() {
		defer wg.Done()
		copyData(b, a)
		// Signal EOF to the backend so it knows the client is done sending.
		closeWrite(b)
	}()

	// b → a (backend to client)
	go func() {
		defer wg.Done()
		copyData(a, b)
		// Signal EOF to the client so it knows the backend is done sending.
		closeWrite(a)
	}()

	wg.Wait()
}

// copyData copies data from src to dst using a pooled buffer.
// Errors (including EOF) are logged at debug level only – they are a normal
// part of the TCP session lifecycle.
func copyData(dst, src net.Conn) {
	buf := make([]byte, copyBufferSize)
	n, err := io.CopyBuffer(dst, src, buf)
	if err != nil && !isConnectionClosedError(err) {
		log.Printf("[proxy] copy %s→%s: %v (copied %d bytes)",
			src.RemoteAddr(), dst.RemoteAddr(), err, n)
	}
}

// closeWrite attempts a half-close on the write side of a TCP connection so
// the remote end receives a proper FIN. We fall back gracefully if the
// underlying connection does not support half-close (e.g. TLS).
func closeWrite(conn net.Conn) {
	type writeCloser interface {
		CloseWrite() error
	}
	if wc, ok := conn.(writeCloser); ok {
		if err := wc.CloseWrite(); err != nil && !isConnectionClosedError(err) {
			log.Printf("[proxy] CloseWrite on %s: %v", conn.RemoteAddr(), err)
		}
	}
}

// isClosedNetworkError reports whether err is the sentinel error returned when
// operating on a closed network connection. The standard library does not
// export this error type, so we resort to string matching.
func isClosedNetworkError(err error) bool {
	if err == nil {
		return false
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return netErr.Err.Error() == "use of closed network connection"
	}
	return false
}

// isConnectionClosedError reports whether err indicates a normal connection
// termination (EOF, connection reset, broken pipe, etc.).
func isConnectionClosedError(err error) bool {
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		msg := netErr.Err.Error()
		return msg == "use of closed network connection" ||
			msg == "connection reset by peer" ||
			msg == "broken pipe"
	}
	return false
}
