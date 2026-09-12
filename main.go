package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/tcp-proxy/monitor"
	"github.com/tcp-proxy/proxy"
	"github.com/tcp-proxy/ring"
)

func main() {

	backendAddresses := []string{"127.0.0.1:9001", "127.0.0.1:9002", "127.0.0.1:9003"}

	r := ring.New()

	for _, add := range backendAddresses {
		r.AddNode(add)
	}

	backendPool := proxy.New()

	monitor := monitor.NewMonitor(r, backendAddresses)

	monitor.Start()

	p := proxy.NewProxy(r, backendPool)

	p.Start()

	closeChan := make(chan os.Signal, 1)

	signal.Notify(closeChan, syscall.SIGINT, syscall.SIGTERM)

	<-closeChan

	monitor.Stop()
	p.Close()
}
