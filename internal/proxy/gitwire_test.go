package proxy

// gitwire_test drives the REAL git binary against the proxy's synthesized
// receive-pack responses (issue #35 §5.3's live-gate item, made hermetic):
// git connects through an HTTP CONNECT proxy (the same preamble srt sends)
// to a local TCP listener served by Proxy.Serve, trusting the rein CA.
// No network, no credentials, no GitHub — what's under test is that
// git/remote-curl ACCEPTS rein's synthesized wire bytes:
//
//   - the pre-declaration ERR advertisement ⇒ `fatal: remote error: …`,
//     clean exit, nothing uploaded;
//   - the post-approval report-status `ng` ⇒ `! [remote rejected] …`.
//
// Skipped when git isn't installed.

import (
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newGitWireHarness builds a proxy like newHarness but ALSO serving on TCP,
// and returns (harness, proxyURL, caFile).
func newGitWireHarness(t *testing.T, opts harnessOpts) (*harness, string, string) {
	t.Helper()
	h := newHarness(t, opts)
	// Rebuild a proxy sharing the same CA/core wiring is heavy; instead
	// bridge: accept TCP and pipe to the unix socket the harness serves.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("unix", h.socket)
			if err != nil {
				c.Close()
				continue
			}
			go func() { defer u.Close(); defer c.Close(); buf := make([]byte, 32<<10); copyConn(c, u, buf) }()
			go func() { buf := make([]byte, 32<<10); copyConn(u, c, buf) }()
		}
	}()

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, h.ca.CertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	return h, "http://" + ln.Addr().String(), caFile
}

func copyConn(dst, src net.Conn, buf []byte) {
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// gitPush inits a repo with one commit and pushes refspec through the proxy.
func gitPush(t *testing.T, proxyURL, caFile, refspec string) (stderr string, exitCode int) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	env := append(os.Environ(),
		"HOME="+dir, // no user gitconfig
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_TERMINAL_PROMPT=0",
	)
	run := func(args ...string) *exec.Cmd {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		return cmd
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "work"},
		{"commit", "-q", "--allow-empty", "-m", "probe"},
	} {
		if out, err := run(args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	cmd := run("-c", "http.proxy="+proxyURL,
		"-c", "http.sslCAInfo="+caFile,
		// Static credentials so git never prompts; the proxy overwrites
		// Authorization on inject anyway.
		"-c", "http.extraHeader=Authorization: Basic eDp5", // x:y
		"push", "https://github.com/o/r.git", refspec)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}
	return string(out), code
}

func TestGitWire_PreDeclarationPushSeesFatalRemoteError(t *testing.T) {
	g := &gateState{} // empty confirmed set
	h, proxyURL, caFile := newGitWireHarness(t, harnessOpts{decl: g.hooks()})

	out, code := gitPush(t, proxyURL, caFile, "HEAD:refs/heads/agent/73/x")
	if code == 0 {
		t.Fatalf("pre-declaration push must fail, output:\n%s", out)
	}
	// The §5.3 acceptance check: real git parses the synthesized ERR pkt
	// and prints it as a remote error — no hang, no retry loop, no upload.
	if !strings.Contains(out, "remote error") || !strings.Contains(out, "writes are locked until you declare your issue") {
		t.Errorf("git did not surface the ERR pkt as a remote error:\n%s", out)
	}
	if !strings.Contains(out, "rein declare") {
		t.Errorf("the instructive next step must reach the agent verbatim:\n%s", out)
	}
	if h.gh.count() != 0 {
		t.Error("GitHub (fake upstream) must never be contacted")
	}
}

// fakeReceivePackAdvertisement serves a minimal, valid v0 receive-pack
// advertisement for an empty repo (what a post-approval GET relays back
// to git so it proceeds to the POST). No side-band advertised: the
// synthesized report is then plain, which real git also accepts.
func fakeReceivePackAdvertisement(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs") {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		body := pkt("# service=git-receive-pack\n") + "0000" +
			pkt(zeroOID+" capabilities^{}\x00report-status delete-refs ofs-delta agent=fake\n") + "0000"
		io.WriteString(w, body)
		return
	}
	// The denied POST must never get here; answer garbage so a leak is loud.
	http.Error(w, "unexpected upstream POST", http.StatusTeapot)
}

func TestGitWire_NonConventionRefSeesRemoteRejected(t *testing.T) {
	g := approvedGate(73)
	h, proxyURL, caFile := newGitWireHarness(t, harnessOpts{decl: g.hooks(), respond: fakeReceivePackAdvertisement})

	out, code := gitPush(t, proxyURL, caFile, "HEAD:refs/heads/main")
	if code == 0 {
		t.Fatalf("non-convention push must fail, output:\n%s", out)
	}
	// The §5.4 acceptance check: real git parses the synthesized
	// report-status and renders the per-ref rejection.
	if !strings.Contains(out, "[remote rejected]") {
		t.Errorf("git did not render the ng report as a remote rejection:\n%s", out)
	}
	if !strings.Contains(out, "agent/<issue>/<nonce>") {
		t.Errorf("the rejection reason must teach the convention:\n%s", out)
	}
	// Upstream saw ONLY the advertisement GET — never the denied POST.
	if h.gh.count() != 1 || h.gh.last().Method != http.MethodGet {
		t.Errorf("denied push must never reach the upstream (count=%d last=%+v)", h.gh.count(), h.gh.last())
	}
}

func TestGitWire_UnconfirmedIssueSeesRemoteRejected(t *testing.T) {
	g := approvedGate(73)
	h, proxyURL, caFile := newGitWireHarness(t, harnessOpts{decl: g.hooks(), respond: fakeReceivePackAdvertisement})

	out, code := gitPush(t, proxyURL, caFile, "HEAD:refs/heads/agent/74/x")
	if code == 0 {
		t.Fatalf("unconfirmed-issue push must fail, output:\n%s", out)
	}
	if !strings.Contains(out, "[remote rejected]") || !strings.Contains(out, "rein declare 74") {
		t.Errorf("rejection must name the expansion step:\n%s", out)
	}
	if h.gh.count() != 1 || h.gh.last().Method != http.MethodGet {
		t.Errorf("denied push must never reach the upstream (count=%d last=%+v)", h.gh.count(), h.gh.last())
	}
}
