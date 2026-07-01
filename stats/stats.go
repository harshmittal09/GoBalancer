// Package stats exposes a lightweight HTTP endpoint at /stats that reports
// real-time operational metrics for the load balancer. It uses only the
// standard library (net/http, encoding/json) and reads its data directly from
// the proxy.Server and ring.HashRing without additional locking overhead.
package stats

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"time"

	"github.com/tcp-proxy/proxy"
	"github.com/tcp-proxy/ring"
)

// startTime records when the process started for uptime calculation.
var startTime = time.Now()

// Snapshot is the JSON-serialisable metrics payload returned by /stats.
type Snapshot struct {
	// Uptime is a human-readable duration since process start.
	Uptime string `json:"uptime"`

	// ActiveConnections is the number of client sessions currently tunnelled.
	ActiveConnections int64 `json:"active_connections"`

	// TotalConnections is the cumulative count of all accepted connections.
	TotalConnections int64 `json:"total_connections"`

	// TotalErrors is the cumulative count of backend dial failures.
	TotalErrors int64 `json:"total_errors"`

	// VirtualNodes is the current number of virtual nodes on the hash ring.
	VirtualNodes int `json:"virtual_nodes"`

	// Backends is the list of physical backend addresses currently on the ring.
	Backends []string `json:"backends"`

	// BackendCount is len(Backends) for convenience.
	BackendCount int `json:"backend_count"`

	// GoRoutines is the current number of goroutines in the process.
	GoRoutines int `json:"goroutines"`

	// MemAllocMB is the current heap allocation in megabytes.
	MemAllocMB float64 `json:"mem_alloc_mb"`
}

// Server is the HTTP observability server.
type Server struct {
	httpSrv *http.Server
	proxy   *proxy.Server
	ring    *ring.HashRing
}

// New creates a stats Server that listens on listenAddr (e.g. ":9090").
func New(listenAddr string, p *proxy.Server, r *ring.HashRing) *Server {
	s := &Server{proxy: p, ring: r}

	mux := http.NewServeMux()
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/", s.handleRoot)

	s.httpSrv = &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	return s
}

// Start begins serving HTTP requests in a background goroutine. It returns
// immediately. The server runs until Close is called.
func (s *Server) Start() {
	go func() {
		log.Printf("[stats] HTTP stats server listening on %s", s.httpSrv.Addr)
		if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[stats] ListenAndServe error: %v", err)
		}
	}()
}

// Close shuts down the HTTP server gracefully.
func (s *Server) Close() error {
	return s.httpSrv.Close()
}

// handleStats writes the current metrics snapshot as JSON.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	snap := Snapshot{
		Uptime:            fmt.Sprintf("%.0fs", time.Since(startTime).Seconds()),
		ActiveConnections: s.proxy.ActiveConnections(),
		TotalConnections:  s.proxy.TotalConnections(),
		TotalErrors:       s.proxy.TotalErrors(),
		VirtualNodes:      s.ring.Len(),
		Backends:          s.ring.Nodes(),
		BackendCount:      len(s.ring.Nodes()),
		GoRoutines:        runtime.NumGoroutine(),
		MemAllocMB:        float64(mem.Alloc) / (1024 * 1024),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		log.Printf("[stats] JSON encode error: %v", err)
	}
}

// handleHealth returns 200 OK with "healthy" if at least one backend is
// registered, or 503 Service Unavailable if the ring is empty.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.ring.Len() == 0 {
		http.Error(w, `{"status":"degraded","reason":"no healthy backends"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintln(w, `{"status":"healthy"}`)
}

// handleRoot returns a simple HTML dashboard with auto-refreshing JSON stats.
func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>TCP Load Balancer – Stats</title>
  <style>
    body { font-family: 'Segoe UI', sans-serif; background: #0f172a; color: #e2e8f0; margin: 0; padding: 2rem; }
    h1   { color: #38bdf8; margin-bottom: 0.25rem; }
    sub  { color: #64748b; }
    pre  { background: #1e293b; border: 1px solid #334155; border-radius: 8px;
           padding: 1.5rem; font-size: 0.9rem; line-height: 1.6; overflow: auto; }
    .badge { display: inline-block; background: #0ea5e9; color: #fff;
             padding: 2px 10px; border-radius: 99px; font-size: 0.75rem; margin-left: 8px; }
  </style>
</head>
<body>
  <h1>⚡ TCP Load Balancer <span class="badge">live</span></h1>
  <sub>Auto-refreshes every 2 seconds</sub>
  <pre id="stats">Loading…</pre>
  <script>
    async function refresh() {
      try {
        const r = await fetch('/stats');
        const j = await r.json();
        document.getElementById('stats').textContent = JSON.stringify(j, null, 2);
      } catch(e) {
        document.getElementById('stats').textContent = 'Error: ' + e;
      }
    }
    refresh();
    setInterval(refresh, 2000);
  </script>
</body>
</html>`)
}
