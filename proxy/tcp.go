package proxy

import (
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/tcp-proxy/ring"
)

const (
	PortAddress = ":9099"
)

type Proxy struct {
	listner net.Listener
	ring    *ring.Ring
	pool    *BackendPool
	wg      sync.WaitGroup
}

func NewProxy(r *ring.Ring, p *BackendPool) *Proxy {
	return &Proxy{
		ring: r,
		pool: p,
	}
}

func (p *Proxy) Start() {
	listner, err := net.Listen("tcp", PortAddress)
	if err != nil {
		log.Printf("Error Starting Proxy")
		return
	}

	p.listner = listner

	p.wg.Add(1)

	go func() {
		defer p.wg.Done()
		for {
			conn, err := listner.Accept()
			if err != nil {
				break
			}
			p.wg.Add(1)
			go p.manageClient(conn)
		}
	}()

}

func (p *Proxy) manageClient(clientConn net.Conn) {

	defer p.wg.Done()

	clientAddress := clientConn.RemoteAddr().String()
	backendAdd := p.ring.GetNode(clientAddress)
	if backendAdd == "" {
		clientConn.Close()
		return
	}

	backendConn, err := p.pool.Get(backendAdd)
	if err != nil {
		clientConn.Close()
		return
	}

	done := make(chan struct{}, 2)

	go func() {
		io.Copy(backendConn, clientConn)
		backendConn.SetDeadline(time.Now())
		done <- struct{}{}

	}()

	go func() {
		io.Copy(clientConn, backendConn)
		clientConn.SetDeadline(time.Now())
		done <- struct{}{}
	}()

	<-done
	<-done

	clientConn.Close()
	backendConn.SetDeadline(time.Time{})
	p.pool.Put(backendAdd, backendConn)

}

func (p *Proxy) Close() {
	if p.listner != nil {
		p.listner.Close()
	}

	p.wg.Wait()
	p.pool.Close()
}
