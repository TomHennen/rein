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

// parked is one idle upgraded stream from the helper. first carries the
// result of the single background read the parking goroutine keeps on the
// stream: EOF/error before use means the helper went away (drop it); after
// the go-byte it is the helper's status byte.
type parked struct {
	conn  net.Conn
	first chan readResult
	done  chan struct{} // closed when the stream has been consumed or dropped
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

// ExposedPorts reports the operator-declared ports (sorted by config order).
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
// consumed by a browser connection (or dropped), so the caller's deferred
// close runs only afterwards. Never relays, never injects.
func (p *Proxy) serveExpose(conn net.Conn, br io.Reader, req *http.Request) bool {
	if req.Body != nil {
		_, _ = io.CopyN(io.Discard, req.Body, 4096)
		req.Body.Close()
	}
	record := func(decision string, port int) {
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: ExposeHost, Method: req.Method, Path: req.URL.Path, Decision: decision, Issue: port})
	}
	if p.expose == nil {
		record("refused-expose-unavailable", 0)
		p.writeLocalJSON(conn, http.StatusForbidden, "rein: no ports are exposed in this run (session expose_ports)")
		return false
	}
	portStr := req.URL.Query().Get("port")
	if req.Method != http.MethodGet || req.URL.Path != "/v1/expose" || !exposePortParam.MatchString(portStr) ||
		!strings.EqualFold(req.Header.Get("Upgrade"), ExposeUpgrade) || !headerHasToken(req.Header.Get("Connection"), "upgrade") {
		record("refused-expose-invalid", 0)
		p.writeLocalJSON(conn, http.StatusBadRequest, "rein: want GET /v1/expose?port=<n> with Upgrade: "+ExposeUpgrade)
		return false
	}
	port, _ := strconv.Atoi(portStr)
	pool, ok := p.expose.ports[port]
	if !ok {
		record("refused-expose-port", port)
		p.writeLocalJSON(conn, http.StatusForbidden, fmt.Sprintf("rein: port %d is not exposed; the human declares ports in the session file (expose_ports)", port))
		return false
	}

	pk := &parked{conn: &prefixConn{r: br, Conn: conn}, first: make(chan readResult, 1), done: make(chan struct{})}
	select {
	case pool <- pk:
	default:
		record("refused-expose-full", port)
		p.writeLocalJSON(conn, http.StatusTooManyRequests, fmt.Sprintf("rein: port %d already has %d parked streams", port, maxParkedPerPort))
		return false
	}

	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: "+ExposeUpgrade+"\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		p.dropParked(pool, pk)
		return false
	}
	_ = conn.SetWriteDeadline(time.Time{})

	// One background read: liveness while parked, the status byte once taken.
	go func() {
		var b [1]byte
		_, err := io.ReadFull(pk.conn, b[:])
		pk.first <- readResult{b: b[0], err: err}
	}()

	select {
	case <-pk.done:
	case <-p.expose.closed:
		p.dropParked(pool, pk)
	}
	return false
}

// dropParked removes pk from pool if it is still parked and releases it.
func (p *Proxy) dropParked(pool chan *parked, pk *parked) {
	// Drain-and-requeue: the pool is small and this is rare.
	n := len(pool)
	for i := 0; i < n; i++ {
		select {
		case x := <-pool:
			if x != pk {
				pool <- x
			}
		default:
		}
	}
	select {
	case <-pk.done:
	default:
		close(pk.done)
	}
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
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: ExposeHost, Method: "LISTEN", Path: "127.0.0.1:" + strconv.Itoa(port), Decision: "exposed", Issue: port})
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

// bridgeExposed takes a parked stream for port, runs the go/status handshake,
// and splices hc with it. Any failure closes hc (the browser sees a reset).
func (p *Proxy) bridgeExposed(ctx context.Context, hc net.Conn, pool chan *parked, port int) {
	defer hc.Close()
	timer := time.NewTimer(takeTimeout)
	defer timer.Stop()
	for {
		var pk *parked
		select {
		case pk = <-pool:
		case <-timer.C:
			p.logger.Printf("expose: port %d: no parked stream within %s (is `rein expose %d` running in the sandbox?)", port, takeTimeout, port)
			return
		case <-ctx.Done():
			return
		}
		// A helper that went away while parked has already reported on first.
		select {
		case r := <-pk.first:
			_ = r
			close(pk.done)
			continue // dead stream; take the next one
		default:
		}
		_ = pk.conn.SetWriteDeadline(time.Now().Add(statusTimeout))
		if _, err := pk.conn.Write([]byte{ExposeGo}); err != nil {
			close(pk.done)
			continue
		}
		_ = pk.conn.SetWriteDeadline(time.Time{})
		select {
		case r := <-pk.first:
			if r.err != nil || r.b != ExposeDialOK {
				p.logger.Printf("expose: port %d: nothing listening on 127.0.0.1:%d inside the sandbox", port, port)
				close(pk.done)
				return
			}
		case <-time.After(statusTimeout):
			close(pk.done)
			return
		case <-ctx.Done():
			close(pk.done)
			return
		}
		splice(hc, pk.conn)
		close(pk.done)
		return
	}
}

// splice copies both directions until both are done, half-closing the write
// side of each peer when the other direction ends.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.SetReadDeadline(time.Now())
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
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
