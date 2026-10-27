// Command backend is a small TCP server used as a load-balancing target in
// benchmarks. For every line it receives it replies with
//
//	<listen address> conn#<n>: <line>
//
// so a client can tell which backend served it. A line of the form
// "GET <bytes>" makes it stream that many bytes and close, for throughput tests.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
)

var chunk = make([]byte, 256*1024)

func main() {
	addr := flag.String("addr", "127.0.0.1:9001", "address to listen on")
	flag.Parse()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	var connCount int64
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		id := atomic.AddInt64(&connCount, 1)
		go serve(conn, *addr, id)
	}
}

func serve(conn net.Conn, addr string, id int64) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "GET ") {
			remaining, err := strconv.ParseInt(line[4:], 10, 64)
			if err != nil {
				return
			}
			for remaining > 0 {
				n := int64(len(chunk))
				if remaining < n {
					n = remaining
				}
				if _, err := conn.Write(chunk[:n]); err != nil {
					return
				}
				remaining -= n
			}
			return
		}
		if _, err := fmt.Fprintf(conn, "%s conn#%d: %s\n", addr, id, line); err != nil {
			return
		}
	}
}
