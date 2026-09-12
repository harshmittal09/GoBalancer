package monitor

import (
	"net"
	"time"

	"github.com/tcp-proxy/ring"
)

const (
	timeInterval = 4 * time.Second
	newConnTl    = 2 * time.Second
)

type Monitor struct {
	backendAdd []string
	ring       *ring.Ring
	done       chan struct{}
}

func NewMonitor(r *ring.Ring, backendAdd []string) *Monitor {
	return &Monitor{
		backendAdd: backendAdd,
		ring:       r,
		done:       make(chan struct{}),
	}
}

func (m *Monitor) Start() {

	go func() {

		ticker := time.NewTicker(timeInterval)
		defer ticker.Stop()

		for {

			select {
			case <-m.done:
				return

			case <-ticker.C:

				for _, add := range m.backendAdd {
					BackendConn, err := net.DialTimeout("tcp", add, newConnTl)

					if err == nil {
						m.ring.AddNode(add)
						BackendConn.Close()
					} else {
						m.ring.RemoveNode(add)
					}
				}

			}

		}
	}()

}

func (m *Monitor) Stop() {
	close(m.done)
}
