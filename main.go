// Command tcp-proxy is the entry point for the Layer-4 TCP load balancer.
// It initialises the consistent hash ring, registers the initial backends,
// starts the active health monitor, and opens both the plain TCP proxy
// listener and the TLS-terminating proxy listener. On receipt of SIGINT or
// SIGTERM it performs a graceful shutdown: listeners are closed (new
// connections are rejected) and the process waits for all in-flight sessions
// to drain before exiting.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tcp-proxy/monitor"
	"github.com/tcp-proxy/proxy"
	"github.com/tcp-proxy/ring"
	"github.com/tcp-proxy/stats"
)

// initialBackends is the static list of backend servers the proxy will target.
// In a production deployment these would be read from a config file or service
// discovery system at startup and updated dynamically at runtime.
var initialBackends = []string{
	"127.0.0.1:8081",
}

const (
	// listenAddr is the address on which the load balancer accepts plain TCP.
	listenAddr = ":8080"

	// tlsListenAddr is the address on which the TLS-terminating listener runs.
	tlsListenAddr = ":8443"

	// statsAddr is the address of the HTTP observability server.
	statsAddr = ":9090"

	// drainTimeout is the maximum time the shutdown sequence will wait for
	// in-flight sessions to finish before forcefully exiting.
	drainTimeout = 30 * time.Second

	// TLS certificate and private key paths.
	// Generate self-signed certs for development with: bash gen_certs.sh
	// In production, replace these with paths to your CA-signed certificates
	// (e.g. from Let's Encrypt or your internal PKI).
	tlsCertFile = "./certs/server.crt"
	tlsKeyFile  = "./certs/server.key"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
	log.Println("[main] starting TCP load balancer (plain + TLS)")

	// -------------------------------------------------------------------------
	// Phase 1: Initialise the consistent hash ring and register backends.
	// -------------------------------------------------------------------------
	r := ring.New()
	for _, addr := range initialBackends {
		r.AddNode(addr)
		log.Printf("[main] registered backend: %s", addr)
	}
	log.Printf("[main] hash ring initialised with %d virtual nodes", r.Len())

	// -------------------------------------------------------------------------
	// Phase 2: Start the active health monitor.
	// The monitor probes backends every 5 s and evicts unhealthy ones from the
	// ring. It also reinstates recovered backends automatically.
	// -------------------------------------------------------------------------
	hm := monitor.New(r, initialBackends)
	hm.Run()
	log.Println("[main] health monitor started")

	// -------------------------------------------------------------------------
	// Phase 3: Initialise the shared backend connection pool.
	// Both the plain TCP and TLS listeners share a single pool so idle
	// connections are reused across both ingress paths.
	// -------------------------------------------------------------------------
	pool := proxy.NewBackendPool()

	// -------------------------------------------------------------------------
	// Phase 4: Start the plain TCP proxy listener on :8080.
	// -------------------------------------------------------------------------
	srv, err := proxy.Start(listenAddr, r)
	if err != nil {
		log.Fatalf("[main] failed to start plain TCP proxy: %v", err)
	}
	log.Printf("[main] plain TCP proxy accepting connections on %s", listenAddr)

	// -------------------------------------------------------------------------
	// Phase 5: Start the TLS-terminating proxy listener on :8443.
	// Skips startup (with a warning) if the cert/key files do not exist so
	// the server remains operational in plain-TCP mode during development.
	// -------------------------------------------------------------------------
	var tlsSrv *proxy.TLSServer
	if fileExists(tlsCertFile) && fileExists(tlsKeyFile) {
		tlsSrv, err = proxy.StartTLS(tlsListenAddr, tlsCertFile, tlsKeyFile, r, pool)
		if err != nil {
			log.Fatalf("[main] failed to start TLS proxy: %v", err)
		}
		log.Printf("[main] TLS proxy accepting connections on %s", tlsListenAddr)
	} else {
		log.Printf("[main] WARNING: TLS cert/key not found at %s / %s", tlsCertFile, tlsKeyFile)
		log.Printf("[main] WARNING: TLS listener is DISABLED. Run 'bash gen_certs.sh' to enable it.")
	}

	// -------------------------------------------------------------------------
	// Phase 6: Start the HTTP stats / observability server on :9090.
	// -------------------------------------------------------------------------
	statsSrv := stats.New(statsAddr, srv, r)
	statsSrv.Start()
	log.Printf("[main] stats dashboard: http://localhost%s", statsAddr)

	// -------------------------------------------------------------------------
	// Phase 7: Block until a termination signal is received.
	// -------------------------------------------------------------------------
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	log.Printf("[main] received signal %v – initiating graceful shutdown", sig)

	// -------------------------------------------------------------------------
	// Phase 8: Graceful shutdown sequence.
	//
	// 1. Stop the health monitor so it no longer mutates the ring.
	// 2. Close the plain TCP proxy listener.
	// 3. Close the TLS proxy listener (if running) + drain its backend pool.
	// 4. Close the stats server.
	// 5. Wait for all in-flight sessions with a hard deadline.
	// -------------------------------------------------------------------------
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)

		hm.Stop()
		log.Println("[main] health monitor stopped")

		if err := srv.Close(); err != nil {
			log.Printf("[main] plain proxy close: %v", err)
		}

		if tlsSrv != nil {
			if err := tlsSrv.Close(); err != nil {
				log.Printf("[main] TLS proxy close: %v", err)
			}
		}

		if err := statsSrv.Close(); err != nil {
			log.Printf("[main] stats close: %v", err)
		}

		log.Println("[main] listeners closed – draining in-flight sessions")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	select {
	case <-shutdownDone:
		log.Println("[main] graceful shutdown complete")
	case <-ctx.Done():
		log.Printf("[main] drain timeout (%s) exceeded – forcing exit", drainTimeout)
	}
}

// fileExists returns true if the file at path exists and is readable.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
