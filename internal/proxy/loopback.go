package proxy

import (
	"fmt"
	"net"
)

// ListenLoopback binds a TCP listener on 127.0.0.1:port for the loopback
// HTTP-CONNECT front: nono chains the sandboxed agent's GitHub traffic to it as
// its upstream_proxy, sending a `CONNECT host:443` preamble the shared proxy core
// (handleConn) already speaks. The listener feeds Proxy.Serve like the unix-socket
// front — same TLS-termination, per-host-class injection, and relay core.
//
// Binds IPv4 loopback ONLY (never 0.0.0.0/::1): the listener carries a live inject
// capability, and its only safety is nono's loopback mediation — nono lets the
// sandboxed agent reach no loopback port except nono's own proxy, so this listener
// is reachable only via nono's tunnel. No proxy-auth is required or possible (nono
// 0.68.0's upstream-proxy auth is unimplemented); the prober asserts the mediation
// separately.
//
// port 0 lets the OS pick a free port, returned so the caller can hand it to the
// profile generator. Unlike Listen (the fs-socket front) this needs no placement
// check — a loopback TCP port has no filesystem capability surface.
func ListenLoopback(port int) (net.Listener, int, error) {
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		return nil, 0, fmt.Errorf("proxy: listen on loopback TCP port %d: %w", port, err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}
