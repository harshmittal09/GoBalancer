package proxy

import (
	"net"
	"sync"
	"time"
)

const (
	maxIdlePerBackend = 500
	idleConnTL        = 90 * time.Second
	newConnTl         = 5 * time.Second
)

type PooledConn struct {
	conn      net.Conn
	idleSince time.Time
}

type BackendPool struct {
	mu      sync.Mutex
	poolMap map[string]chan PooledConn
}

func New() *BackendPool {
	return &BackendPool{
		poolMap: make(map[string]chan PooledConn),
	}
}

func (p *BackendPool) getCh(backendName string) chan PooledConn {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ch, ok := p.poolMap[backendName]; ok {
		return ch
	}

	ch := make(chan PooledConn, maxIdlePerBackend)

	p.poolMap[backendName] = ch
	return ch
}

func (p *BackendPool) Put(backendName string, conn net.Conn) {
	ch := p.getCh(backendName)

	poolConn := &PooledConn{
		conn:      conn,
		idleSince: time.Now(),
	}

	select {
	case ch <- *poolConn:
		return

	default:
		conn.Close()
	}
}

func (p *BackendPool) Get(backendName string) (net.Conn, error) {

	ch := p.getCh(backendName)

	for {
		select {
		case pooledConn := <-ch:
			if time.Since(pooledConn.idleSince) > idleConnTL {
				pooledConn.conn.Close()
				continue
			}

			if !isAlive(pooledConn.conn) {
				pooledConn.conn.Close()
				continue
			}

			return pooledConn.conn, nil

		default:
			return net.DialTimeout("tcp", backendName, newConnTl)
		}
	}
}

func isAlive(conn net.Conn) bool {
	conn.SetDeadline(time.Now().Add(50 * time.Millisecond))
	defer conn.SetDeadline(time.Time{})

	var buf [1]byte

	_, err := conn.Read(buf[:])

	if err == nil {
		return false
	}

	if netErr, ok := err.(net.Error); ok {
		if netErr.Timeout() == true {
			return true
		}
	}

	return false

}

func (p *BackendPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for add, ch := range p.poolMap {
		close(ch)
		for pooledConn := range ch {
			pooledConn.conn.Close()
		}
		delete(p.poolMap, add)
	}

}
