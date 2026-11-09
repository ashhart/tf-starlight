//go:build !windows

package e2e_test

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type postgresEndpoint struct {
	mu          sync.Mutex
	ln          net.Listener
	backend     string
	closed      bool
	connections map[net.Conn]bool
	wg          sync.WaitGroup
}

func newPostgresEndpoint(t *testing.T, backend string) *postgresEndpoint {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &postgresEndpoint{ln: ln, backend: backend, connections: map[net.Conn]bool{}}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			p.wg.Add(1)
			go p.forward(client)
		}
	}()
	return p
}

func (p *postgresEndpoint) forward(client net.Conn) {
	defer p.wg.Done()
	defer client.Close()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	backend := p.backend
	p.connections[client] = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.connections, client); p.mu.Unlock() }()
	server, err := net.DialTimeout("tcp", backend, time.Second)
	if err != nil {
		return
	}
	defer server.Close()
	p.mu.Lock()
	if p.closed || backend != p.backend {
		p.mu.Unlock()
		return
	}
	p.connections[server] = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.connections, server); p.mu.Unlock() }()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(server, client); server.Close(); close(done) }()
	_, _ = io.Copy(client, server)
	client.Close()
	<-done
}

// retarget severs existing sessions as a failover endpoint does; new connections use the promoted server.
func (p *postgresEndpoint) retarget(backend string) {
	p.mu.Lock()
	p.backend = backend
	for conn := range p.connections {
		_ = conn.Close()
	}
	p.mu.Unlock()
}

func (p *postgresEndpoint) close() {
	p.mu.Lock()
	p.closed = true
	_ = p.ln.Close()
	for conn := range p.connections {
		_ = conn.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}
