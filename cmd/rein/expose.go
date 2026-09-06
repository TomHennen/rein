// In-sandbox side of the reverse tunnel (#179; protocol in
// internal/proxy/expose.go). `rein sandbox-exec` is what rein actually launches
// inside srt: it starts one `rein expose <port>` helper per exposed port, then
// execs the agent argv in place. Each helper keeps a few idle upgraded streams
// parked at expose.rein.internal (through the in-sandbox proxy bridge, the only
// route out) and, on the broker's go-byte, dials 127.0.0.1:<port> and splices.
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/TomHennen/rein/internal/proxy"
	"github.com/TomHennen/rein/internal/srt"
)

const (
	// exposeWorkers is the number of idle streams a helper keeps parked per
	// port — browsers open several connections at once, and each consumed
	// stream is replaced only after its handshake round-trip.
	exposeWorkers    = 4
	exposeMaxBackoff = 5 * time.Second
)

// runExpose is `rein expose <port>`: loop forever keeping streams parked.
// Exit 2 on a usage/config error the human must fix (unknown port); it is
// otherwise expected to die with the sandbox.
func runExpose(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: rein expose <port>   (started by rein sandbox-exec)")
		return 2
	}
	port, err := strconv.Atoi(args[0])
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintf(os.Stderr, "rein expose: %q is not a TCP port\n", args[0])
		return 2
	}
	sp, err := sandboxProxyFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rein expose: %v\n", err)
		return 2
	}
	fatal := make(chan string, 1)
	for i := 0; i < exposeWorkers; i++ {
		go exposeWorker(sp, port, fatal)
	}
	msg := <-fatal
	fmt.Fprintf(os.Stderr, "rein expose %d: %s\n", port, msg)
	return 2
}

// sandboxProxy is the in-sandbox proxy bridge from the env srt sets
// (http://srt:<secret>@localhost:3128 — the bridge requires that basic auth on
// the CONNECT, which http.ProxyFromEnvironment adds for `rein declare` and we
// must add by hand here).
type sandboxProxy struct {
	addr      string
	authValue string // "Basic <b64>" or ""
}

func sandboxProxyFromEnv() (sandboxProxy, error) {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		v := os.Getenv(name)
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.Host == "" {
			// Not echoed: the value carries srt's bridge secret.
			return sandboxProxy{}, fmt.Errorf("%s is not a proxy URL", name)
		}
		sp := sandboxProxy{addr: u.Host}
		if u.User != nil {
			pw, _ := u.User.Password()
			sp.authValue = "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pw))
		}
		return sp, nil
	}
	return sandboxProxy{}, errors.New("no HTTPS_PROXY in the environment (not inside a rein sandbox?)")
}

// exposeWorker parks one stream at a time, forever, with backoff on failure.
// A definitive refusal from the broker (4xx) is reported on fatal.
func exposeWorker(sp sandboxProxy, port int, fatal chan<- string) {
	backoff := 200 * time.Millisecond
	for {
		err := parkAndServe(sp, port)
		var refused *exposeRefused
		if errors.As(err, &refused) {
			select {
			case fatal <- refused.Error():
			default:
			}
			return
		}
		if err != nil {
			time.Sleep(backoff)
			if backoff < exposeMaxBackoff {
				backoff *= 2
			}
			continue
		}
		backoff = 200 * time.Millisecond
	}
}

type exposeRefused struct {
	status int
	msg    string
}

func (e *exposeRefused) Error() string {
	return fmt.Sprintf("broker refused (%d): %s", e.status, e.msg)
}

// parkAndServe opens one upgraded stream, waits for the go-byte, dials the
// local port, and splices until either side ends.
func parkAndServe(sp sandboxProxy, port int) error {
	raw, err := net.DialTimeout("tcp", sp.addr, 10*time.Second)
	if err != nil {
		return err
	}
	defer func() {
		if raw != nil { // nil once a splice goroutine owns the stream
			raw.Close()
		}
	}()
	_ = raw.SetDeadline(time.Now().Add(15 * time.Second))
	auth := ""
	if sp.authValue != "" {
		auth = "Proxy-Authorization: " + sp.authValue + "\r\n"
	}
	if _, err := fmt.Fprintf(raw, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n%s\r\n", proxy.ExposeHost, proxy.ExposeHost, auth); err != nil {
		return err
	}
	pr := bufio.NewReader(raw)
	resp, err := http.ReadResponse(pr, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("CONNECT %s: %s", proxy.ExposeHost, resp.Status)
	}
	// SSL_CERT_FILE (set by the launch) makes rein's CA a system root here.
	tc := tls.Client(&bufferedConn{r: pr, Conn: raw}, &tls.Config{ServerName: proxy.ExposeHost, NextProtos: []string{"http/1.1"}})
	if err := tc.Handshake(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(tc, "GET /v1/expose?port=%d HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", port, proxy.ExposeHost, proxy.ExposeUpgrade); err != nil {
		return err
	}
	tr := bufio.NewReader(tc)
	uresp, err := http.ReadResponse(tr, &http.Request{Method: http.MethodGet})
	if err != nil {
		return err
	}
	if uresp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(uresp.Body, 4096))
		uresp.Body.Close()
		msg := strings.TrimSpace(string(body))
		if uresp.StatusCode == http.StatusForbidden || uresp.StatusCode == http.StatusBadRequest {
			return &exposeRefused{status: uresp.StatusCode, msg: msg}
		}
		return fmt.Errorf("expose: %s: %s", uresp.Status, msg)
	}
	// Parked: wait for the go-byte, unbounded (the broker owns the pool).
	_ = raw.SetDeadline(time.Time{})
	b, err := tr.ReadByte()
	if err != nil {
		return err
	}
	if b != proxy.ExposeGo {
		return fmt.Errorf("expose: unexpected byte %#x while parked", b)
	}
	local, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		_, _ = tc.Write([]byte{proxy.ExposeDialFailed})
		return nil // not a tunnel failure: nothing is listening yet; park again
	}
	if _, err := tc.Write([]byte{proxy.ExposeDialOK}); err != nil {
		local.Close()
		return err
	}
	// Taken: the splice runs on its own goroutine so THIS worker re-parks at
	// once. Workers are the idle-stream count, not a concurrency cap — a browser
	// holds several keep-alive connections open for minutes, and each one must
	// not starve the next (the HMR websocket was the one that failed).
	raw = nil // ownership moves to the splice goroutine (see the deferred close)
	go func() {
		defer local.Close()
		defer tc.Close()
		spliceConns(local, &bufferedConn{r: tr, Conn: tc})
	}()
	return nil
}

// bufferedConn lets bytes already pulled into a bufio.Reader be read first.
type bufferedConn struct {
	r *bufio.Reader
	net.Conn
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func spliceConns(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else if bc, ok := dst.(*bufferedConn); ok {
			if cw, ok := bc.Conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// runSandboxExec is `rein sandbox-exec [--expose <port>]... -- <agent argv>`:
// the in-sandbox launch wrapper for EVERY sandboxed run. It folds the per-run
// proxy secret into the child's proxy URLs (#185; srt mints none for an
// external proxy), starts the expose helpers (#179), and execs the agent.
func runSandboxExec(args []string) int {
	ports, argv, err := parseSandboxExecArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rein sandbox-exec: %v\n", err)
		return 2
	}
	if secret := os.Getenv(srt.EnvProxyAuth); secret != "" {
		for _, kv := range rewriteProxyEnv(os.Environ(), secret) {
			k, v, _ := strings.Cut(kv, "=")
			_ = os.Setenv(k, v)
		}
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rein sandbox-exec: %v\n", err)
		return 2
	}
	for _, port := range ports {
		h := exec.Command(self, "expose", strconv.Itoa(port))
		h.Stdin = nil
		h.Stdout = nil
		h.Stderr = os.Stderr // only a definitive refusal is ever printed
		if err := h.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "rein sandbox-exec: start expose %d: %v\n", port, err)
			return 2
		}
		// Reparented to the sandbox's PID-1 reaper at exec; dies with the
		// sandbox (its only route out is the run socket).
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "rein sandbox-exec: %v\n", err)
		return 127
	}
	if err := syscall.Exec(path, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "rein sandbox-exec: exec %s: %v\n", path, err)
		return 126
	}
	return 0 // unreachable
}

// parseSandboxExecArgs accepts `[--expose N]... -- argv...`.
func parseSandboxExecArgs(args []string) (ports []int, argv []string, err error) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			argv = args[i+1:]
			if len(argv) == 0 {
				return nil, nil, errors.New("no agent command after --")
			}
			return ports, argv, nil
		case a == "--expose":
			if i+1 >= len(args) {
				return nil, nil, errors.New("--expose needs a port")
			}
			i++
			p, perr := strconv.Atoi(args[i])
			if perr != nil || p < 1 || p > 65535 {
				return nil, nil, fmt.Errorf("--expose %q is not a TCP port", args[i])
			}
			ports = append(ports, p)
		default:
			return nil, nil, fmt.Errorf("unexpected argument %q (want [--expose <port>]... -- <cmd>)", a)
		}
	}
	return nil, nil, errors.New("missing -- before the agent command")
}

// proxyEnvNames are the variables srt sets to its in-sandbox proxy URL, in
// the exact spelling generateProxyEnvVars uses.
var proxyEnvNames = []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"}

// rewriteProxyEnv returns the KEY=VALUE pairs to (re)set so every proxy URL
// in env carries the secret as userinfo (srt:<secret>@), plus git's
// pre-emptive basic proxy auth (srt sets the same when it owns the token).
// Pure: input env, output the changed pairs only.
func rewriteProxyEnv(env []string, secret string) []string {
	var out []string
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			continue
		}
		for _, name := range proxyEnvNames {
			if k != name {
				continue
			}
			u, err := url.Parse(v)
			if err != nil || u.Host == "" {
				continue
			}
			u.User = url.UserPassword(proxy.ProxyUser, secret)
			out = append(out, k+"="+u.String())
		}
	}
	if len(out) > 0 {
		gcp := "http.proxyAuthMethod=basic"
		for _, kv := range env {
			if strings.HasPrefix(kv, "GIT_CONFIG_PARAMETERS=") && !strings.Contains(kv, gcp) {
				gcp = strings.TrimPrefix(kv, "GIT_CONFIG_PARAMETERS=") + " " + "'" + gcp + "'"
				out = append(out, "GIT_CONFIG_PARAMETERS="+gcp)
				return out
			}
		}
		out = append(out, "GIT_CONFIG_PARAMETERS='"+gcp+"'")
	}
	return out
}

// sandboxExecArgv wraps the agent argv for launch through the staged
// binary's sandbox-exec (always, since #185: the proxy secret rewrite).
func sandboxExecArgv(reinBin string, ports []int, agentArgv []string) []string {
	out := []string{reinBin, "sandbox-exec"}
	for _, p := range ports {
		out = append(out, "--expose", strconv.Itoa(p))
	}
	out = append(out, "--")
	return append(out, agentArgv...)
}

// exposePortsCSV renders ports for REIN_IN_SANDBOX_EXPOSE_PORTS.
func exposePortsCSV(ports []int) string {
	s := make([]string, 0, len(ports))
	for _, p := range ports {
		s = append(s, strconv.Itoa(p))
	}
	return strings.Join(s, ",")
}
