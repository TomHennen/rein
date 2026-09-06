package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// freePort grabs an ephemeral loopback port for the host listener under test.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// park plays the in-sandbox helper's first half: open an upgraded stream for
// port and return it after the 101 (or the non-101 response).
func park(t *testing.T, h *harness, port int, hostHdr string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	tc := h.rawTLS(t, ExposeHost)
	if hostHdr == "" {
		hostHdr = ExposeHost
	}
	fmt.Fprintf(tc, "GET /v1/expose?port=%d HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", port, hostHdr, ExposeUpgrade)
	br := bufio.NewReader(tc)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	return tc, br, resp
}

// TestExpose_BridgesHostConnectionToParkedStream is the whole tunnel in one
// process: a fake helper parks a stream, a "browser" connects to the host
// loopback listener, the go/status handshake runs, bytes flow both ways.
func TestExpose_BridgesHostConnectionToParkedStream(t *testing.T) {
	port := freePort(t)
	h := newHarness(t, harnessOpts{exposePorts: []int{port}})

	tc, br, resp := park(t, h, port, "")
	defer tc.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want 101", resp.StatusCode)
	}
	if got := resp.Header.Get("Upgrade"); got != ExposeUpgrade {
		t.Errorf("Upgrade header = %q", got)
	}

	// Helper side: wait for the go-byte, answer DialOK, then echo.
	echoed := make(chan string, 1)
	go func() {
		b, err := br.ReadByte()
		if err != nil || b != ExposeGo {
			echoed <- fmt.Sprintf("bad go-byte %#x err=%v", b, err)
			return
		}
		tc.Write([]byte{ExposeDialOK})
		line, err := br.ReadString('\n')
		if err != nil {
			echoed <- "read: " + err.Error()
			return
		}
		tc.Write([]byte("echo:" + line))
		echoed <- line
	}()

	browser, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 5*time.Second)
	if err != nil {
		t.Fatalf("host loopback listener not reachable: %v", err)
	}
	defer browser.Close()
	_ = browser.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := browser.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(browser).ReadString('\n')
	if err != nil {
		t.Fatalf("read from bridge: %v", err)
	}
	if reply != "echo:hello\n" {
		t.Errorf("bridge reply = %q", reply)
	}
	if got := <-echoed; got != "hello\n" {
		t.Errorf("helper saw %q", got)
	}
}

// TestExpose_DialFailedClosesBrowserConnection: the helper reports nothing is
// listening in the sandbox; the browser side is closed rather than hung.
func TestExpose_DialFailedClosesBrowserConnection(t *testing.T) {
	port := freePort(t)
	h := newHarness(t, harnessOpts{exposePorts: []int{port}})
	tc, br, resp := park(t, h, port, "")
	defer tc.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	go func() {
		if b, _ := br.ReadByte(); b == ExposeGo {
			tc.Write([]byte{ExposeDialFailed})
		}
	}()
	browser, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	_ = browser.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(browser); err != nil {
		t.Errorf("expected a clean close, got %v", err)
	}
}

// TestExpose_Refusals pins the fail-closed answers: an undeclared port, a
// missing Upgrade, a wrong path, and a Host/SNI mismatch are all refused
// (never parked), and a proxy with NO exposed ports refuses everything.
func TestExpose_Refusals(t *testing.T) {
	port := freePort(t)
	h := newHarness(t, harnessOpts{exposePorts: []int{port}})

	_, _, resp := park(t, h, port+1, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("undeclared port: status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "expose_ports") {
		t.Errorf("refusal must name the operator knob: %s", body)
	}

	_, _, resp = park(t, h, port, "github.com")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("host mismatch: status = %d, want 400", resp.StatusCode)
	}

	tc := h.rawTLS(t, ExposeHost)
	fmt.Fprintf(tc, "GET /v1/expose?port=%d HTTP/1.1\r\nHost: %s\r\n\r\n", port, ExposeHost)
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no Upgrade: status = %d, want 400", resp.StatusCode)
	}

	none := newHarness(t, harnessOpts{})
	_, _, resp = park(t, none, port, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("no exposed ports: status = %d, want 403", resp.StatusCode)
	}
}

// TestExpose_ParkedPoolIsBounded: a helper (or a hostile agent) cannot pin
// unbounded broker goroutines by parking streams forever.
func TestExpose_ParkedPoolIsBounded(t *testing.T) {
	port := freePort(t)
	h := newHarness(t, harnessOpts{exposePorts: []int{port}})
	var keep []net.Conn
	defer func() {
		for _, c := range keep {
			c.Close()
		}
	}()
	for i := 0; i < maxParkedPerPort; i++ {
		tc, _, resp := park(t, h, port, "")
		keep = append(keep, tc)
		if resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("park %d: status = %d", i, resp.StatusCode)
		}
	}
	_, _, resp := park(t, h, port, "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("park beyond the bound: status = %d, want 429", resp.StatusCode)
	}
}

// helperEcho plays the helper after the 101: on the go-byte answer DialOK and
// echo lines back, until the stream ends.
func helperEcho(tc net.Conn, br *bufio.Reader) {
	if b, err := br.ReadByte(); err != nil || b != ExposeGo {
		return
	}
	tc.Write([]byte{ExposeDialOK})
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		tc.Write([]byte("echo:" + line))
	}
}

func dialBrowser(t *testing.T, port int) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 5*time.Second)
	if err != nil {
		t.Fatalf("host loopback listener not reachable: %v", err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	return c
}

func roundTrip(t *testing.T, browser net.Conn, msg string) string {
	t.Helper()
	if _, err := browser.Write([]byte(msg + "\n")); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(browser).ReadString('\n')
	if err != nil {
		t.Fatalf("read from bridge: %v", err)
	}
	return reply
}

// TestExpose_ShutdownMidSpliceDoesNotPanic pins the double-close: the run ends
// (ctx cancelled) while a browser connection is actively bridged. The parking
// goroutine and the bridge both finish the same entry; that must be idempotent
// and both ends must simply close.
func TestExpose_ShutdownMidSpliceDoesNotPanic(t *testing.T) {
	port := freePort(t)
	h := newHarness(t, harnessOpts{exposePorts: []int{port}})
	tc, br, resp := park(t, h, port, "")
	defer tc.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	go helperEcho(tc, br)
	browser := dialBrowser(t, port)
	defer browser.Close()
	if got := roundTrip(t, browser, "ping"); got != "echo:ping\n" {
		t.Fatalf("bridge reply = %q", got)
	}
	h.cancel() // run over while the splice is live
	// Both ends see the stream end; the broker must not panic (a panic would
	// abort the test binary, not just this test).
	_ = browser.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(browser); err != nil {
		t.Errorf("browser side after shutdown: %v", err)
	}
}

// TestExpose_BrowserWaitingBeforePark pins the go-byte ordering: a browser
// already blocked on an EMPTY pool must not get its go-byte written before the
// helper has read the 101 (the two interleaved corrupt the helper's response).
func TestExpose_BrowserWaitingBeforePark(t *testing.T) {
	port := freePort(t)
	h := newHarness(t, harnessOpts{exposePorts: []int{port}})
	for i := 0; i < 20; i++ {
		browser := dialBrowser(t, port)
		tc, br, resp := park(t, h, port, "") // the browser is already waiting
		if resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("iteration %d: status = %d (a go-byte raced the 101?)", i, resp.StatusCode)
		}
		go helperEcho(tc, br)
		if got := roundTrip(t, browser, "hi"); got != "echo:hi\n" {
			t.Fatalf("iteration %d: bridge reply = %q", i, got)
		}
		browser.Close()
		tc.Close()
	}
}

// TestExpose_DeadParkedStreamIsSkipped: a helper that parked and then went
// away must not consume a browser connection; the next live stream is used
// and the pool slot is re-admitted.
func TestExpose_DeadParkedStreamIsSkipped(t *testing.T) {
	port := freePort(t)
	h := newHarness(t, harnessOpts{exposePorts: []int{port}})
	dead, _, resp := park(t, h, port, "")
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	dead.Close() // helper vanished while parked
	live, br, resp := park(t, h, port, "")
	defer live.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	go helperEcho(live, br)
	time.Sleep(50 * time.Millisecond) // let the dead stream's EOF land on first
	browser := dialBrowser(t, port)
	defer browser.Close()
	if got := roundTrip(t, browser, "alive"); got != "echo:alive\n" {
		t.Errorf("bridge reply = %q (the dead stream was used?)", got)
	}
	// Both slots are released: the pool admits maxParkedPerPort fresh streams.
	var keep []net.Conn
	defer func() {
		for _, c := range keep {
			c.Close()
		}
	}()
	for i := 0; i < maxParkedPerPort; i++ {
		c, _, r := park(t, h, port, "")
		keep = append(keep, c)
		if r.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("re-admission %d: status = %d", i, r.StatusCode)
		}
	}
}

// TestExpose_BusyPortFailsClosed: a host port already in use aborts
// construction of the listeners (the launch), instead of silently not
// forwarding.
func TestExpose_BusyPortFailsClosed(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	p := &Proxy{expose: newExposer([]int{port}), logger: testLogger(t), audit: NewAuditLog(io.Discard)}
	if err := p.ServeExpose(t.Context()); err == nil {
		t.Fatal("ServeExpose bound a busy port without error")
	}
}
