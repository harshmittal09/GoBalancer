// Command tcp-proxy is the entry point for the Layer-4 TCP load balancer.
// It initialises the consistent hash ring, registers the initial backends,
// starts the active health monitor, and opens the proxy listener. On receipt
// of SIGINT or SIGTERM it performs a graceful shutdown: the listener is closed
// (new connections are rejected) and the process waits for all in-flight
// sessions to drain before exiting.
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

// listenAddr is the address on which the load balancer will accept client TCP
// connections.
const listenAddr = ":8080"

// statsAddr is the address of the HTTP observability server.
const statsAddr = ":9090"

// drainTimeout is the maximum time the shutdown sequence will wait for
// in-flight sessions to finish before forcefully exiting.
const drainTimeout = 30 * time.Second

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
	log.Println("[main] starting TCP load balancer")

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
	// Phase 3: Start the TCP proxy listener.
	// -------------------------------------------------------------------------
	srv, err := proxy.Start(listenAddr, r)
	if err != nil {
		log.Fatalf("[main] failed to start proxy: %v", err)
	}
	log.Printf("[main] proxy is accepting connections on %s", listenAddr)

	// -------------------------------------------------------------------------
	// Phase 4: Start the HTTP stats / observability server on :9090.
	// -------------------------------------------------------------------------
	statsSrv := stats.New(statsAddr, srv, r)
	statsSrv.Start()
	log.Printf("[main] stats dashboard: http://localhost%s", statsAddr)

	// -------------------------------------------------------------------------
	// Phase 4: Block until a termination signal is received.
	// -------------------------------------------------------------------------
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	log.Printf("[main] received signal %v – initiating graceful shutdown", sig)

	// -------------------------------------------------------------------------
	// Phase 5: Graceful shutdown sequence.
	//
	// 1. Stop the health monitor so it no longer mutates the ring.
	// 2. Close the proxy listener so no new connections are accepted.
	// 3. Close the stats server.
	// 4. Wait for in-flight sessions with a deadline.
	// -------------------------------------------------------------------------
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)

		// Stop issuing new health probes.
		hm.Stop()
		log.Println("[main] health monitor stopped")

		// Close the listener; existing sessions keep running until they finish.
		if err := srv.Close(); err != nil {
			log.Printf("[main] proxy close returned: %v", err)
		}
		if err := statsSrv.Close(); err != nil {
			log.Printf("[main] stats close returned: %v", err)
		}
		log.Println("[main] listener closed – draining in-flight sessions")
	}()

	// Apply a maximum drain timeout so we always exit eventually.
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	select {
	case <-shutdownDone:
		log.Println("[main] graceful shutdown complete")
	case <-ctx.Done():
		log.Printf("[main] drain timeout (%s) exceeded – forcing exit", drainTimeout)
	}
}
