// expose.rein.internal: the reverse tunnel that lets the HUMAN's browser reach
// a server the agent started INSIDE the sandbox (issue #179).
//
// srt runs the agent under `bwrap --unshare-net`, so a port bound in-sandbox is
// invisible to the host, and its seccomp filter blocks socket(AF_UNIX) for the
// agent's process tree, so a naive in-sandbox socat bridge is impossible. What
// the agent CAN do is TCP to the in-sandbox proxy bridge — the path every
// virtual host already rides. So the tunnel is agent-initiated, `ssh -R`
// style: an in-sandbox helper (`rein expose <port>`) keeps idle streams parked
// here via an HTTP/1.1 Upgrade on this virtual host; when a browser connects
// to the host-side loopback listener rein takes one parked stream, sends a
// go-byte, the helper dials 127.0.0.1:<port> inside the sandbox, answers with
// a status byte, and both ends are spliced. One parked stream per browser
// connection; no multiplexing protocol.
//
// Security shape: ports are OPERATOR-declared (session expose_ports), never
// agent-chosen; the host listener binds loopback only; nothing on this path
// ever carries a GitHub token (a local class, never injected, never relayed);
// the parked-stream pool is bounded so a hostile agent cannot pin unbounded
// goroutines/fds in the broker.
package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ExposeHost is the local-only virtual host the in-sandbox `rein expose`
// helper rides. Listed in LocalHosts so srt routes it to the run socket.
const ExposeHost = "expose.rein.internal"

// ExposeUpgrade is the Upgrade token both sides must present.
const ExposeUpgrade = "rein-expose/1"

// Tunnel protocol bytes (after the 101). Broker → helper: goByte when a
// browser connection is waiting. Helper → broker: one status byte.
const (
	ExposeGo         byte = 0x01
	ExposeDialOK     byte = 0x01
	ExposeDialFailed byte = 0x02
)

const (
	// maxParkedPerPort bounds the idle streams a helper may park per port —
	// a bound on broker goroutines/fds held open by in-sandbox code.
	maxParkedPerPort = 8
	// takeTimeout is how long a browser connection waits for a parked stream
	// before rein gives up (nothing running `rein expose` in the sandbox).
	takeTimeout = 10 * time.Second
	// statusTimeout bounds the helper's dial + status-byte reply.
	statusTimeout = 10 * time.Second
)

var exposePortParam = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)

// parked is one upgraded stream from the helper. Lifecycle: enqueued, then
// ready (the 101 is on the wire — only then may a bridge write the go-byte),
// then taken by exactly one bridge, then finished. finish is idempotent so the
// parking goroutine (shutdown) and the bridge can both call it; a bridge that
// pulls an already-finished entry skips it.
type parked struct {
	conn  net.Conn
	first chan readResult // the single background read: liveness while parked, the status byte once taken
	ready chan struct{}
	done  chan struct{}
	once  sync.Once
}

func (pk *parked) finish() { pk.once.Do(func() { close(pk.done) }) }

func (pk *parked) finished() bool {
	select {
	case <-pk.done:
		return true
	default:
		return false
	}
}

type readResult struct {
	b   byte
	err error
}

// exposer is the per-run tunnel registry: a bounded pool of parked streams
// per operator-declared port.
type exposer struct {
	ports  map[int]chan *parked
	closed chan struct{}
	once   sync.Once
}

func newExposer(ports []int) *exposer {
	e := &exposer{ports: map[int]chan *parked{}, closed: make(chan struct{})}
	for _, p := range ports {
		e.ports[p] = make(chan *parked, maxParkedPerPort)
	}
	return e
}

func (e *exposer) close() { e.once.Do(func() { close(e.closed) }) }

// ExposedPorts reports the operator-declared ports, ascending.
func (p *Proxy) ExposedPorts() []int {
	if p.expose == nil {
		return nil
	}
	out := make([]int, 0, len(p.expose.ports))
	for port := range p.expose.ports {
		out = append(out, port)
	}
	sortInts(out)
	return out
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// serveExpose answers the expose.rein.internal virtual host: validates the
// helper's Upgrade request, parks the stream, and BLOCKS until it has been
// consumed by a browser connection (or the run ends), so the caller's deferred
// close runs only afterwards. Never relays, never injects.
func (p *Proxy) serveExpose(conn net.Conn, br io.Reader, req *http.Request) bool {
	if req.Body != nil {
		_, _ = io.CopyN(io.Discard, req.Body, 4096)
		req.Body.Close()
	}
	record := func(decision, path string) {
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: ExposeHost, Method: req.Method, Path: path, Decision: decision})
	}
	if p.expose == nil {
		record("refused-expose-unavailable", req.URL.Path)
		p.writeLocalJSON(conn, http.StatusForbidden, "rein: no ports are exposed in this run (session expose_ports)")
		return false
	}
	portStr := req.URL.Query().Get("port")
	if req.Method != http.MethodGet || req.URL.Path != "/v1/expose" || !exposePortParam.MatchString(portStr) ||
		!strings.EqualFold(req.Header.Get("Upgrade"), ExposeUpgrade) || !headerHasToken(req.Header.Get("Connection"), "upgrade") {
		record("refused-expose-invalid", req.URL.Path)
		p.writeLocalJSON(conn, http.StatusBadRequest, "rein: want GET /v1/expose?port=<n> with Upgrade: "+ExposeUpgrade)
		return false
	}
	port, _ := strconv.Atoi(portStr)
	pool, ok := p.expose.ports[port]
	if !ok {
		record("refused-expose-port", "port="+portStr)
		p.writeLocalJSON(conn, http.StatusForbidden, fmt.Sprintf("rein: port %d is not exposed; the human declares ports in the session file (expose_ports)", port))
		return false
	}

	pk := &parked{conn: &prefixConn{r: br, Conn: conn}, first: make(chan readResult, 1), ready: make(chan struct{}), done: make(chan struct{})}
	select {
	case pool <- pk:
	default:
		record("refused-expose-full", "port="+portStr)
		p.writeLocalJSON(conn, http.StatusTooManyRequests, fmt.Sprintf("rein: port %d already has %d parked streams", port, maxParkedPerPort))
		return false
	}

	// The 101 must be on the wire before any bridge writes the go-byte, or the
	// helper reads the two interleaved. A failed write finishes the entry; a
	// bridge that pulls it skips it.
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: "+ExposeUpgrade+"\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		pk.finish()
		return false
	}
	_ = conn.SetWriteDeadline(time.Time{})
	close(pk.ready)

	// One background read: liveness while parked, the status byte once taken.
	go func() {
		var b [1]byte
		_, err := io.ReadFull(pk.conn, b[:])
		pk.first <- readResult{b: b[0], err: err}
	}()

	select {
	case <-pk.done:
	case <-p.expose.closed:
		// Run over: finish the entry (a bridge mid-splice sees its conn close
		// when we return) and let the deferred close tear the stream down.
		pk.finish()
	}
	return false
}

// ServeExpose binds a loopback listener per exposed port and bridges each
// accepted host connection over a parked stream. Fails closed (an error, no
// listeners) if any port cannot be bound. Returns once all listeners are up;
// they run until ctx is done.
func (p *Proxy) ServeExpose(ctx context.Context) error {
	if p.expose == nil {
		return nil
	}
	var lns []net.Listener
	for _, port := range p.ExposedPorts() {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return fmt.Errorf("expose port %d: %w (something on the host already listens there?)", port, err)
		}
		lns = append(lns, ln)
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: ExposeHost, Method: "LISTEN", Path: "127.0.0.1:" + strconv.Itoa(port), Decision: "exposed"})
		go p.acceptExposed(ctx, ln, port)
	}
	go func() {
		<-ctx.Done()
		p.expose.close()
		for _, l := range lns {
			l.Close()
		}
	}()
	return nil
}

func (p *Proxy) acceptExposed(ctx context.Context, ln net.Listener, port int) {
	pool := p.expose.ports[port]
	for {
		hc, err := ln.Accept()
		if err != nil {
			return
		}
		go p.bridgeExposed(ctx, hc, pool, port)
	}
}

// take pulls the next usable parked stream for port: ready, not finished, and
// with the helper still on the other end. Returns nil on timeout/shutdown.
func (p *Proxy) take(ctx context.Context, pool chan *parked, port int) *parked {
	timer := time.NewTimer(takeTimeout)
	defer timer.Stop()
	for {
		var pk *parked
		select {
		case pk = <-pool:
		case <-timer.C:
			p.logger.Printf("expose: port %d: no parked stream within %s (is `rein expose %d` running in the sandbox?)", port, takeTimeout, port)
			return nil
		case <-ctx.Done():
			return nil
		}
		select {
		case <-pk.ready:
		case <-pk.done:
			continue // the 101 never made it
		case <-ctx.Done():
			pk.finish()
			return nil
		}
		if pk.finished() {
			continue
		}
		// A helper that went away while parked has already reported on first.
		select {
		case <-pk.first:
			pk.finish()
			continue
		default:
		}
		return pk
	}
}

// bridgeExposed takes a parked stream for port, runs the go/status handshake,
// and splices hc with it. Any failure closes hc (the browser sees a reset).
func (p *Proxy) bridgeExposed(ctx context.Context, hc net.Conn, pool chan *parked, port int) {
	defer hc.Close()
	pk := p.take(ctx, pool, port)
	if pk == nil {
		return
	}
	defer pk.finish()
	_ = pk.conn.SetWriteDeadline(time.Now().Add(statusTimeout))
	if _, err := pk.conn.Write([]byte{ExposeGo}); err != nil {
		return
	}
	_ = pk.conn.SetWriteDeadline(time.Time{})
	select {
	case r := <-pk.first:
		if r.err != nil || r.b != ExposeDialOK {
			p.logger.Printf("expose: port %d: nothing listening on 127.0.0.1:%d inside the sandbox", port, port)
			p.audit.Record(AuditEntry{Session: p.sessionID, Host: ExposeHost, Method: "BRIDGE", Path: "127.0.0.1:" + strconv.Itoa(port), Decision: "refused-expose-nolistener"})
			return
		}
	case <-time.After(statusTimeout):
		return
	case <-ctx.Done():
		return
	}
	p.audit.Record(AuditEntry{Session: p.sessionID, Host: ExposeHost, Method: "BRIDGE", Path: "127.0.0.1:" + strconv.Itoa(port), Decision: "exposed-bridged"})
	splice(hc, pk.conn)
}

// splice copies both directions until both are done, half-closing the write
// side of each peer when the other direction ends.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		closeWrite(dst)
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// closeWrite half-closes dst's write side, unwrapping the read-prefix wrapper
// (whose embedded interface hides the TLS/TCP CloseWrite). Falls back to
// unblocking the peer's reader when no half-close exists.
func closeWrite(dst net.Conn) {
	inner := dst
	if pc, ok := dst.(*prefixConn); ok {
		inner = pc.Conn
	}
	if cw, ok := inner.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = dst.SetReadDeadline(time.Now())
}

// headerHasToken reports whether a comma-separated header lists token
// (case-insensitive).
func headerHasToken(h, token string) bool {
	for _, t := range strings.Split(h, ",") {
		if strings.EqualFold(strings.TrimSpace(t), token) {
			return true
		}
	}
	return false
}
