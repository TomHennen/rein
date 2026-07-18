package proxy

import (
	"fmt"
	"net"
)

// ListenLoopback binds a TCP listener on 127.0.0.1:port for the loopback
// HTTP-CONNECT front (the nono pivot): nono chains the sandboxed agent's GitHub
// traffic to this listener as its upstream_proxy, sending a `CONNECT host:443`
// preamble that the shared proxy core (handleConn) already speaks. The returned
// listener is fed to Proxy.Serve exactly like the srt unix-socket front — same
// TLS-termination, per-host-class injection, and relay core.
//
// It binds IPv4 loopback ONLY (never 0.0.0.0, never ::1): the listener carries a
// live inject capability, and its ONLY safety rests on nono's loopback mediation
// — nono blocks the sandboxed agent from reaching any loopback port except
// nono's own proxy, so this listener is reachable only via nono's tunnel. No
// proxy-auth is required or possible (nono 0.68.0's upstream-proxy auth is
// unimplemented); the prober asserts the loopback-mediation property separately.
//
// port 0 lets the OS choose a free port; the actual bound port is returned so
// the caller can hand it to nono's profile generator (upstream_proxy host:port).
// Unlike Listen (the fs-socket front), this needs no placement check or
// permission tightening — a loopback TCP port has no filesystem capability
// surface.
func ListenLoopback(port int) (net.Listener, int, error) {
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		return nil, 0, fmt.Errorf("proxy: listen on loopback TCP port %d: %w", port, err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}
