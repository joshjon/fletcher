package daemon

import (
	"context"
	"net"
	"net/http"
	"sync"
)

// requestConnections tracks raw HTTP/1, HTTP/2 and hijacked connections.
// http.Server.Shutdown does not own CONNECT or WebSocket hijacks. Closing them releases
// backpressured request writers before session operations are drained.
type requestConnections struct {
	mu     sync.Mutex
	closed bool
	live   map[*requestConn]struct{}
}

func newRequestConnections() *requestConnections {
	return &requestConnections{live: make(map[*requestConn]struct{})}
}

func (p *requestConnections) listener(ln net.Listener) net.Listener {
	if p == nil {
		return ln
	}
	return &requestListener{Listener: ln, owner: p}
}

func (p *requestConnections) closeAll() {
	p.mu.Lock()
	p.closed = true
	connections := make([]*requestConn, 0, len(p.live))
	for c := range p.live {
		connections = append(connections, c)
	}
	p.mu.Unlock()
	for _, c := range connections {
		_ = c.Close()
	}
}

type requestListener struct {
	net.Listener
	owner *requestConnections
}

func (l *requestListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	c := &requestConn{Conn: conn, owner: l.owner}
	l.owner.mu.Lock()
	closed := l.owner.closed
	if !closed {
		l.owner.live[c] = struct{}{}
	}
	l.owner.mu.Unlock()
	if closed {
		_ = c.Close()
		return nil, net.ErrClosed
	}
	return c, nil
}

type requestConn struct {
	net.Conn
	owner    *requestConnections
	once     sync.Once
	closeErr error
}

func (c *requestConn) Close() error {
	c.once.Do(func() {
		c.closeErr = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.live, c)
		c.owner.mu.Unlock()
	})
	return c.closeErr
}

func (s *services) httpServers() []*http.Server {
	servers := []*http.Server{s.connectSrv, s.gatewaySrv, s.mcpSrv, s.proxySrv, s.proxyOpenSrv, s.remoteSrv, s.vpnAPISrv, s.pairingSrv}
	if s.publicWeb != nil {
		servers = append(servers, s.publicWeb.httpsSrv, s.publicWeb.httpSrv)
	}
	return servers
}

func (s *services) trackRequests(ctx context.Context) *requestConnections {
	connections := newRequestConnections()
	for _, srv := range s.httpServers() {
		if srv != nil {
			srv.BaseContext = func(net.Listener) context.Context { return ctx }
		}
	}
	for _, ln := range []*net.Listener{&s.connectLn, &s.gatewayLn, &s.mcpLn, &s.gatewayUnixLn, &s.mcpUnixLn, &s.proxyUnixLn, &s.proxyOpenUnixLn, &s.remoteLn, &s.pairingLn} {
		if *ln != nil {
			*ln = connections.listener(*ln)
		}
	}
	if s.publicWeb != nil {
		s.publicWeb.httpsLn = connections.listener(s.publicWeb.httpsLn)
		s.publicWeb.httpLn = connections.listener(s.publicWeb.httpLn)
	}
	return connections
}

func (s *services) closeRequests(connections *requestConnections) {
	connections.closeAll()
	for _, srv := range s.httpServers() {
		if srv != nil {
			_ = srv.Close()
		}
	}
}
