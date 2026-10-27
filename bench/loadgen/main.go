// Command loadgen drives traffic through GoBalancer (or directly at a backend)
// and reports latency percentiles, throughput and errors.
//
// Modes:
//
//	sessions    open a new connection per request (connect, 1 request, close)
//	persistent  keep c connections open and send m requests on each
//	hold        open n connections, keep them idle, then check they still work
//	bulk        download a large payload over p parallel connections
//	failover    steady traffic, reported per 500ms window and per backend
//
// Every reply is checked against its request, so a reply delivered to the
// wrong client is counted as an error rather than a success.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var addr = flag.String("addr", "127.0.0.1:9099", "address to send traffic to")

func main() {
	mode := flag.String("mode", "sessions", "sessions | persistent | hold | bulk | failover")
	n := flag.Int("n", 18000, "sessions (sessions mode) or connections (hold mode)")
	c := flag.Int("c", 100, "concurrent clients")
	m := flag.Int("m", 1000, "requests per connection (persistent mode)")
	p := flag.Int("p", 1, "parallel streams (bulk mode)")
	size := flag.Int64("bytes", 1<<30, "bytes per stream (bulk mode)")
	d := flag.Duration("d", 15*time.Second, "duration (failover mode) or hold time (hold mode)")
	ready := flag.String("ready", "", "file to create once all connections are open (hold mode)")
	flag.Parse()

	switch *mode {
	case "sessions":
		sessions(*n, *c)
	case "persistent":
		persistent(*c, *m)
	case "hold":
		hold(*n, *d, *ready)
	case "bulk":
		bulk(*p, *size)
	case "failover":
		failover(*c, *d)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", *mode)
		os.Exit(2)
	}
}

// roundTrip sends one line and checks the reply echoes it back.
// It returns the backend address that served the request.
func roundTrip(conn net.Conn, r *bufio.Reader, msg string) (string, error) {
	if _, err := fmt.Fprintf(conn, "%s\n", msg); err != nil {
		return "", err
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if !strings.HasSuffix(line, ": "+msg) {
		return "", fmt.Errorf("reply %q does not match request %q", line, msg)
	}
	return strings.Fields(line)[0], nil
}

func percentiles(d []time.Duration) string {
	if len(d) == 0 {
		return "no samples"
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	at := func(q float64) string {
		v := d[int(q*float64(len(d)-1))]
		return fmt.Sprintf("%.2fms", float64(v.Microseconds())/1000)
	}
	return fmt.Sprintf("p50=%s p90=%s p99=%s p99.9=%s max=%s", at(.5), at(.9), at(.99), at(.999), at(1))
}

func sessions(total, workers int) {
	lat := make([]time.Duration, 0, total)
	var mu sync.Mutex
	var next, failed int64 = -1, 0
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := atomic.AddInt64(&next, 1)
				if i >= int64(total) {
					return
				}
				t := time.Now()
				conn, err := net.DialTimeout("tcp", *addr, 5*time.Second)
				if err != nil {
					atomic.AddInt64(&failed, 1)
					continue
				}
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				_, err = roundTrip(conn, bufio.NewReader(conn), fmt.Sprintf("session-%d", i))
				conn.Close()
				if err != nil {
					atomic.AddInt64(&failed, 1)
					continue
				}
				el := time.Since(t)
				mu.Lock()
				lat = append(lat, el)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	secs := time.Since(start).Seconds()
	fmt.Printf("sessions=%d concurrency=%d ok=%d failed=%d elapsed=%.1fs rate=%.0f/s\n", total, workers, len(lat), failed, secs, float64(len(lat))/secs)
	fmt.Printf("  session latency (dial to reply): %s\n", percentiles(lat))
}

func persistent(conns, msgs int) {
	var mu sync.Mutex
	var rtts []time.Duration
	var failed int64
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", *addr, 10*time.Second)
			if err != nil {
				atomic.AddInt64(&failed, 1)
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(2 * time.Minute))
			r := bufio.NewReader(conn)
			local := make([]time.Duration, 0, msgs)
			for j := 0; j < msgs; j++ {
				t := time.Now()
				if _, err := roundTrip(conn, r, fmt.Sprintf("c%d-m%d", i, j)); err != nil {
					atomic.AddInt64(&failed, 1)
					return
				}
				local = append(local, time.Since(t))
			}
			mu.Lock()
			rtts = append(rtts, local...)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	secs := time.Since(start).Seconds()
	fmt.Printf("connections=%d requests_each=%d requests=%d failed_connections=%d elapsed=%.1fs rate=%.0f req/s\n", conns, msgs, len(rtts), failed, secs, float64(len(rtts))/secs)
	fmt.Printf("  request latency: %s\n", percentiles(rtts))
}

func hold(total int, holdFor time.Duration, readyFile string) {
	conns := make([]net.Conn, 0, total)
	readers := make([]*bufio.Reader, 0, total)
	failed := 0
	start := time.Now()
	for i := 0; i < total; i++ {
		conn, err := net.DialTimeout("tcp", *addr, 10*time.Second)
		if err != nil {
			failed++
			continue
		}
		conn.SetDeadline(time.Now().Add(30 * time.Second))
		r := bufio.NewReader(conn)
		if _, err := roundTrip(conn, r, fmt.Sprintf("open-%d", i)); err != nil {
			failed++
			conn.Close()
			continue
		}
		conns = append(conns, conn)
		readers = append(readers, r)
	}
	fmt.Printf("hold: opened=%d failed=%d in %.1fs\n", len(conns), failed, time.Since(start).Seconds())
	if readyFile != "" {
		os.WriteFile(readyFile, []byte("ready\n"), 0o644)
	}
	time.Sleep(holdFor)
	working := 0
	for i, conn := range conns {
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := roundTrip(conn, readers[i], fmt.Sprintf("check-%d", i)); err == nil {
			working++
		}
		conn.Close()
	}
	fmt.Printf("hold: still working after %v idle: %d/%d\n", holdFor, working, len(conns))
}

func bulk(streams int, size int64) {
	var received int64
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", *addr)
			if err != nil {
				return
			}
			defer conn.Close()
			fmt.Fprintf(conn, "GET %d\n", size)
			n, _ := io.Copy(io.Discard, conn)
			atomic.AddInt64(&received, n)
		}()
	}
	wg.Wait()
	secs := time.Since(start).Seconds()
	fmt.Printf("bulk: streams=%d received=%.2fGiB complete=%v elapsed=%.2fs throughput=%.2f Gbit/s\n",
		streams, float64(received)/(1<<30), received == int64(streams)*size, secs, float64(received)*8/secs/1e9)
}

func failover(workers int, dur time.Duration) {
	type result struct {
		at      time.Duration
		backend string
		ok      bool
	}
	var mu sync.Mutex
	var results []result
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; time.Since(start) < dur; i++ {
				res := result{}
				if conn, err := net.DialTimeout("tcp", *addr, 3*time.Second); err == nil {
					conn.SetDeadline(time.Now().Add(3 * time.Second))
					res.backend, err = roundTrip(conn, bufio.NewReader(conn), fmt.Sprintf("w%d-%d", w, i))
					res.ok = err == nil
					conn.Close()
				}
				res.at = time.Since(start)
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
				time.Sleep(20 * time.Millisecond)
			}
		}(w)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool { return results[i].at < results[j].at })
	var backends []string
	seen := map[string]bool{}
	for _, r := range results {
		if r.ok && !seen[r.backend] {
			seen[r.backend] = true
			backends = append(backends, r.backend)
		}
	}
	sort.Strings(backends)

	fmt.Printf("  %6s %6s %6s", "t(s)", "ok", "failed")
	for _, b := range backends {
		fmt.Printf(" %16s", b)
	}
	fmt.Println()
	const window = 500 * time.Millisecond
	for from := time.Duration(0); from < dur; from += window {
		ok, bad := 0, 0
		per := map[string]int{}
		for _, r := range results {
			if r.at >= from && r.at < from+window {
				if r.ok {
					ok++
					per[r.backend]++
				} else {
					bad++
				}
			}
		}
		fmt.Printf("  %6.1f %6d %6d", from.Seconds(), ok, bad)
		for _, b := range backends {
			fmt.Printf(" %16d", per[b])
		}
		fmt.Println()
	}

	failures := 0
	var first, last time.Duration = -1, -1
	for _, r := range results {
		if !r.ok {
			failures++
			if first < 0 {
				first = r.at
			}
			last = r.at
		}
	}
	if failures == 0 {
		fmt.Printf("failover: requests=%d failed=0\n", len(results))
		return
	}
	fmt.Printf("failover: requests=%d failed=%d (%.1f%%) failures between %.2fs and %.2fs (window %.2fs)\n",
		len(results), failures, 100*float64(failures)/float64(len(results)), first.Seconds(), last.Seconds(), (last - first).Seconds())
}
