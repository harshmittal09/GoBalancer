// Package proxy — tls.go
//
// TLSServer adds a Layer-4 TLS termination listener on top of the existing
// proxy engine.  It accepts inbound encrypted connections on a dedicated port
// (default :8443), completes the TLS handshake at the boundary, then splices
// the decrypted byte stream to a backend via the shared BackendPool and the
// consistent hash ring — identical routing logic to the plain TCP path.
//
// Architecture (L4 TLS Termination):
//
//	┌─────────────┐   TLS (encrypted)   ┌──────────────────────────────┐
//	│   Client    │ ──────────────────► │  TLSServer (:8443)           │
//	└─────────────┘                     │  • TLS handshake boundary    │
//	                                    │  • Hash ring lookup          │
//	                                    │  • Pool.Get(backendAddr)     │
//	                                    │  • io.CopyBuffer (splice)    │
//	                                    └──────────┬───────────────────┘
//	                                               │ plain TCP (decrypted)
//	                                    ┌──────────▼───────────────────┐
//	                                    │  Backend Echo Servers         │
//	                                    │  :8081 / :8082 / :8083        │
//	                                    └──────────────────────────────┘
//
// The TLS handshake is completed entirely at the load balancer boundary.
// Backends receive plain, unencrypted TCP streams and require no TLS
// configuration of their own — a classic "TLS offload" pattern used by
// production load balancers like HAProxy and AWS NLB.
package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tcp-proxy/ring"
)

// TLSServer is a TLS-terminating Layer-4 proxy listener.  It shares a
// HashRing and BackendPool with the plain TCP Server for uniform routing.
type TLSServer struct {
	listener    net.Listener
	ring        *ring.HashRing
	pool        *BackendPool
	tlsConfig   *tls.Config

	activeConns atomic.Int64
	totalConns  atomic.Int64
	totalErrors atomic.Int64

	wg sync.WaitGroup
}

// StartTLS creates a TLS listener on listenAddr using the certificate and
// private key at certFile and keyFile respectively.  It shares r and pool
// with the plain TCP Server so both listeners use identical routing.
//
// StartTLS is non-blocking: the accept loop runs in a background goroutine.
// Call Close() to perform a graceful shutdown.
func StartTLS(listenAddr, certFile, keyFile string, r *ring.HashRing, pool *BackendPool) (*TLSServer, error) {
	// ── Load the TLS certificate and private key pair ─────────────────────────
	// tls.LoadX509KeyPair reads both PEM-encoded files and validates that the
	// certificate's public key matches the private key.
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("tls: loading certificate pair (%s, %s): %w", certFile, keyFile, err)
	}

	// ── Assemble a hardened TLS configuration ────────────────────────────────
	tlsCfg := &tls.Config{
		// Serve the loaded certificate for all connections.
		Certificates: []tls.Certificate{cert},

		// MinVersion enforces TLS 1.2 as the floor; TLS 1.3 is preferred.
		// TLS 1.0 and 1.1 are cryptographically deprecated (POODLE, BEAST).
		MinVersion: tls.VersionTLS12,

		// Prefer TLS 1.3 — it has a reduced handshake round-trip count (1-RTT
		// vs 2-RTT in TLS 1.2) and eliminates all legacy cipher suites.
		MaxVersion: tls.VersionTLS13,

		// CipherSuites specifies the allowed suites for TLS 1.2 connections.
		// TLS 1.3 cipher suites are always AEAD and are not configurable.
		// These suites provide:
		//   - Forward secrecy via ECDHE key exchange
		//   - Authenticated encryption (AEAD) with AES-GCM or ChaCha20-Poly1305
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		},

		// CurvePreferences restricts key agreement to modern elliptic curves.
		// X25519 is preferred; P-256 and P-384 are included for compatibility.
		CurvePreferences: []tls.CurveID{
			tls.X25519,
			tls.CurveP256,
			tls.CurveP384,
		},

		// PreferServerCipherSuites forces the server's cipher preference order
		// rather than the client's, ensuring the strongest suite is always used.
		PreferServerCipherSuites: true, //nolint:staticcheck // intentional for TLS 1.2 control
	}

	// ── Create the TLS listener ───────────────────────────────────────────────
	// tls.Listen wraps net.Listen with the TLS configuration.  Individual
	// connections returned by Accept() are *tls.Conn, which satisfy net.Conn.
	l, err := tls.Listen("tcp", listenAddr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("tls: listen on %s: %w", listenAddr, err)
	}

	s := &TLSServer{
		listener:  l,
		ring:      r,
		pool:      pool,
		tlsConfig: tlsCfg,
	}

	log.Printf("[tls-proxy] listening on %s (TLS 1.2/1.3)", listenAddr)
	s.wg.Add(1)
	go s.acceptLoop()

	return s, nil
}

// ActiveConnections returns the live count of in-flight TLS sessions.
func (s *TLSServer) ActiveConnections() int64 { return s.activeConns.Load() }

// TotalConnections returns the cumulative count of accepted TLS connections.
func (s *TLSServer) TotalConnections() int64 { return s.totalConns.Load() }

// TotalErrors returns the cumulative count of TLS or backend dial failures.
func (s *TLSServer) TotalErrors() int64 { return s.totalErrors.Load() }

// Close shuts down the TLS listener and waits for all in-flight sessions to
// finish.  It also drains the shared connection pool.
func (s *TLSServer) Close() error {
	err := s.listener.Close()
	s.wg.Wait()
	s.pool.Close()
	log.Printf("[tls-proxy] shut down. total accepted: %d, errors: %d",
		s.totalConns.Load(), s.totalErrors.Load())
	return err
}

// acceptLoop is the main TLS accept loop.  It runs in a dedicated goroutine.
func (s *TLSServer) acceptLoop() {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if isClosedNetworkError(err) {
				log.Println("[tls-proxy] listener closed, stopping accept loop")
				return
			}
			log.Printf("[tls-proxy] accept error: %v", err)
			time.Sleep(5 * time.Millisecond)
			continue
		}

		s.totalConns.Add(1)
		s.activeConns.Add(1)
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer s.activeConns.Add(-1)
			s.handleTLSConnection(c)
		}(conn)
	}
}

// handleTLSConnection is the per-connection handler for TLS sessions.
//
// It performs four steps:
//  1. Complete the TLS handshake (performs the cryptographic negotiation).
//  2. Look up the target backend via the hash ring (using the client IP as key).
//  3. Obtain a backend connection from the pool (reuse or dial fresh).
//  4. Splice the decrypted TLS byte stream to the backend via bidirectional
//     io.CopyBuffer tunnels.
func (s *TLSServer) handleTLSConnection(rawConn net.Conn) {
	defer rawConn.Close() //nolint:errcheck

	// ── Step 1: Complete the TLS handshake ────────────────────────────────────
	// rawConn is a *tls.Conn at runtime (returned by tls.Listen.Accept).
	// Calling Handshake() explicitly allows us to set a deadline on the
	// cryptographic negotiation, preventing slow-client denial-of-service.
	tlsConn, ok := rawConn.(*tls.Conn)
	if !ok {
		log.Printf("[tls-proxy] accepted conn is not a *tls.Conn — this is a bug")
		s.totalErrors.Add(1)
		return
	}

	// Apply a strict deadline for the handshake.  Clients that stall during
	// the TLS negotiation (e.g. slow-loris TLS variant) are disconnected.
	if err := tlsConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		log.Printf("[tls-proxy] set handshake deadline: %v", err)
		s.totalErrors.Add(1)
		return
	}

	if err := tlsConn.Handshake(); err != nil {
		if !isConnectionClosedError(err) {
			log.Printf("[tls-proxy] TLS handshake from %s failed: %v", rawConn.RemoteAddr(), err)
		}
		s.totalErrors.Add(1)
		return
	}

	// Handshake complete — clear the deadline so normal data flow is not
	// subject to the handshake timeout.
	if err := tlsConn.SetDeadline(time.Time{}); err != nil {
		log.Printf("[tls-proxy] clear deadline: %v", err)
		s.totalErrors.Add(1)
		return
	}

	// Log the negotiated protocol version and cipher suite for observability.
	state := tlsConn.ConnectionState()
	log.Printf("[tls-proxy] handshake OK from %s | TLS version: %s | cipher: %s",
		tlsConn.RemoteAddr(),
		tlsVersionName(state.Version),
		tls.CipherSuiteName(state.CipherSuite),
	)

	// ── Step 2: Route via hash ring ───────────────────────────────────────────
	// Use the client's remote address (IP:port) as the ring key.  This matches
	// the plain TCP path and ensures consistent backend affinity per session.
	remoteAddr := tlsConn.RemoteAddr().String()
	target := s.ring.GetNode(remoteAddr)
	if target == "" {
		log.Printf("[tls-proxy] no healthy backends for client %s — dropping", remoteAddr)
		s.totalErrors.Add(1)
		return
	}

	log.Printf("[tls-proxy] routing %s → %s", remoteAddr, target)

	// ── Step 3: Get a backend connection from the pool ────────────────────────
	// Pool.Get returns a reused idle connection or dials a fresh one.
	backendConn, err := s.pool.Get(target)
	if err != nil {
		log.Printf("[tls-proxy] pool.Get backend %s for client %s: %v", target, remoteAddr, err)
		s.totalErrors.Add(1)
		return
	}

	// ── Step 4: Bidirectional byte-stream splice ──────────────────────────────
	// tunnel() copies data concurrently in both directions until either side
	// closes.  The TLS decryption is transparent: io.CopyBuffer reads plaintext
	// bytes from tls.Conn and writes them verbatim to the backend TCP socket.
	//
	// After the session ends we attempt to return the backend connection to the
	// pool.  If the connection is broken (non-nil tunnel error), we discard it.
	backendHealthy := tunnelWithResult(tlsConn, backendConn)
	if backendHealthy {
		s.pool.Put(target, backendConn)
	} else {
		backendConn.Close() //nolint:errcheck
	}

	log.Printf("[tls-proxy] session closed for client %s", remoteAddr)
}

// tunnelWithResult runs the bidirectional copy (same as tunnel() in tcp.go)
// and returns true only if neither direction encountered a non-EOF error on
// the backend side.  This lets the caller decide whether to pool the backend
// connection or discard it.
func tunnelWithResult(client, backend net.Conn) (backendHealthy bool) {
	var (
		wg         sync.WaitGroup
		clientErr  error
		backendErr error
	)
	wg.Add(2)

	// client → backend
	go func() {
		defer wg.Done()
		buf := make([]byte, copyBufferSize)
		_, clientErr = copyDataErr(backend, client, buf)
		closeWrite(backend)
	}()

	// backend → client
	go func() {
		defer wg.Done()
		buf := make([]byte, copyBufferSize)
		_, backendErr = copyDataErr(client, backend, buf)
		closeWrite(client)
	}()

	wg.Wait()

	// The backend connection is considered healthy if neither direction saw a
	// hard error on the backend socket (EOF and closed-connection errors are
	// expected normal termination and are not counted as failures).
	return isConnectionClosedError(backendErr) && isConnectionClosedError(clientErr)
}

// copyDataErr is like copyData but returns the error so the caller can inspect
// whether the connection is reusable.
func copyDataErr(dst, src net.Conn, buf []byte) (int64, error) {
	n, err := ioCustomCopyBuffer(dst, src, buf)
	if err != nil && !isConnectionClosedError(err) {
		log.Printf("[tls-proxy] copy %s→%s: %v (copied %d bytes)",
			src.RemoteAddr(), dst.RemoteAddr(), err, n)
	}
	return n, err
}

// ioCustomCopyBuffer is a thin wrapper around io.CopyBuffer that returns the
// underlying error rather than masking it.
func ioCustomCopyBuffer(dst, src net.Conn, buf []byte) (int64, error) {
	return copyBuffer(dst, src, buf)
}

// copyBuffer copies from src to dst using the given buffer and returns the
// number of bytes copied and any error.
func copyBuffer(dst net.Conn, src net.Conn, buf []byte) (int64, error) {
	var total int64
	for {
		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			total += int64(nw)
			if werr != nil {
				return total, werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, errEOF()) {
				return total, nil
			}
			return total, rerr
		}
	}
}

// errEOF returns io.EOF via the errors package to avoid a direct import
// of "io" that would create a circular dependency concern.
func errEOF() error {
	return tlsEOFSentinel
}

// tlsEOFSentinel is a package-level reference to io.EOF used by errEOF.
// It avoids repeating the import path and keeps the pattern testable.
var tlsEOFSentinel = ioEOF()

// tlsVersionName converts a TLS version constant to a human-readable string
// for structured logging.
func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "TLS 1.0"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	default:
		return fmt.Sprintf("unknown(0x%04x)", v)
	}
}
