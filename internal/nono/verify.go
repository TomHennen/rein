package nono

// Host-side containment launch gate. VerifyContainment launches the in-sandbox
// probe (RunContainmentProbe) through a REAL `nono run` under the profile Build
// emits, reads the Observations back, Classifies them, and returns a non-nil
// error when any channel LEAKED — so the caller MUST fail the launch closed.
//
// Safe invocation (docs/nono-git-push-spike-findings.md execution discipline):
// every `nono run` is session-isolated with `setsid -w` (new session; the -w
// WAITS so we get nono's real exit code — bare setsid returns immediately and
// races the child), stdin from /dev/null, a trivial short-lived probe, and no
// tmux server/socket is ever touched. On timeout the whole process group is
// killed by negative pid.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrNonoUnavailable is returned (wrapped) when nono is not installed, so a
// caller/CI can SKIP cleanly. A skip is NOT a pass: the returned Verdict is
// empty and ShouldFailClosed() is false, but callers gating a real launch must
// treat this error as "cannot verify" per their fail-closed policy.
var ErrNonoUnavailable = errors.New("nono binary not found")

// VerifyParams are the inputs to VerifyContainment.
type VerifyParams struct {
	// ReinBin is the absolute path to the rein binary invoked as the in-sandbox
	// probe (`rein __nono-probe`). Required.
	ReinBin string

	// NonoBin is the nono binary. If empty it is resolved from PATH; if it cannot
	// be found, VerifyContainment returns ErrNonoUnavailable (skip).
	NonoBin string

	// CACertPath is a readable CA PEM for the profile (Build requires it so the
	// sandbox's TLS trust is wired). For a pure containment probe its content is
	// irrelevant; if empty, a throwaway PEM is staged. If set, must be absolute.
	CACertPath string

	// ListenAddr is the upstream_proxy the profile points at (rein's real
	// listener in production). If empty, a throwaway host-side upstream listener
	// is started and used, so the gate is self-contained. Bare host:port.
	ListenAddr string

	// CredPaths are real credential files to assert unreadable (App key, gh
	// token, ssh keys). Non-existent entries are dropped (and reduce certainty).
	// If empty, a sensible default set under $HOME is used.
	CredPaths []string

	// ExtraDomains is passed through to Build (operator opt-in egress).
	ExtraDomains []string

	// Policy tunes classification (e.g. FailOnUDP).
	Policy Policy

	// Timeout caps the probe launch. Default 60s.
	Timeout time.Duration
}

// VerifyContainment is the launch-gate seam rein's `run --nono` path will call
// to gate a launch on containment (P1e wiring; not yet invoked from cmd/rein —
// the prober lands first per design §3e). It returns the classified Verdict and
// a non-nil error when the launch must be refused (a leak, a failed positive
// control, or the probe could not run). When nono is absent it returns
// ErrNonoUnavailable so CI can skip.
func VerifyContainment(vp VerifyParams) (Verdict, error) {
	if vp.ReinBin == "" {
		return Verdict{}, fmt.Errorf("nono verify: ReinBin is required")
	}
	nonoBin := vp.NonoBin
	if nonoBin == "" {
		p, err := exec.LookPath("nono")
		if err != nil {
			return Verdict{}, fmt.Errorf("%w: %v", ErrNonoUnavailable, err)
		}
		nonoBin = p
	} else if _, err := os.Stat(nonoBin); err != nil {
		return Verdict{}, fmt.Errorf("%w: %v", ErrNonoUnavailable, err)
	}
	if vp.Timeout <= 0 {
		vp.Timeout = 60 * time.Second
	}

	// Scratch dir: config, observations output, staged CA + sentinel all live
	// here and it is the ONLY dir granted (r+w) to the sandbox.
	work, err := os.MkdirTemp("", "rein-nono-probe-*")
	if err != nil {
		return Verdict{}, fmt.Errorf("nono verify: mkdir: %w", err)
	}
	defer os.RemoveAll(work)

	caPath := vp.CACertPath
	if caPath == "" {
		caPath = filepath.Join(work, "ca.pem")
		if err := os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----\nrein-nono-probe-throwaway\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
			return Verdict{}, fmt.Errorf("nono verify: write throwaway CA: %w", err)
		}
	}

	// Planted ARBITRARY loopback listener — THE control. Not referenced anywhere
	// in the profile, so a sandbox connect to it must be denied. It must be LIVE
	// host-side, else the probe's connect would fail with ECONNREFUSED and (under
	// the errno discipline) read as unknown, not a false pass.
	planted, plantedClose, err := startLoopbackListener()
	if err != nil {
		return Verdict{}, fmt.Errorf("nono verify: start planted listener: %w", err)
	}
	defer plantedClose()

	// Upstream listener for the profile's upstream_proxy, unless the caller
	// supplied a real one (rein's live proxy).
	listenAddr := vp.ListenAddr
	if listenAddr == "" {
		up, upClose, uerr := startLoopbackListener()
		if uerr != nil {
			return Verdict{}, fmt.Errorf("nono verify: start upstream listener: %w", uerr)
		}
		defer upClose()
		listenAddr = up
	}

	// Credential targets + host-side existence truth. Plant a sentinel canary in
	// a deny_credentials dir if one is writable, for a deterministic read-back.
	credTargets, credExist, sentinelCleanup := stageCredTargets(vp.CredPaths)
	defer sentinelCleanup()

	// Approval-channel fixture (§3e): a DEDICATED throwaway tmux server on a socket
	// in a NON-granted dir. Non-granted is load-bearing — §3e proved fs-deny does
	// NOT block AF_UNIX connect, so a denied connect here is the runtime proof that
	// af_unix_mediation is enforced, not just declared. Absent tmux ⇒ empty ⇒ the
	// socket/send-keys checks are skipped (never fail the gate). Cleaned in defer.
	tmuxFx := stageTmuxFixture()
	defer tmuxFx.cleanup()

	cfg := probeConfig{
		Creds:           credTargets,
		ExternalTarget:  "1.1.1.1:443", // literal, no DNS — blocked at connect, robust offline
		GitHubTarget:    resolveGitHub(),
		PlantedLoopback: planted,
		UDPTarget:       "8.8.8.8:53",
		TmuxSocket:      tmuxFx.probeSocket(),
		TmuxBin:         tmuxFx.probeBin(),
		TmuxTarget:      tmuxFx.probeSession(),
		DialTimeoutMS:   3000,
	}
	cfgPath := filepath.Join(work, "probe-config.json")
	cfgBytes, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(cfgPath, cfgBytes, 0o600); err != nil {
		return Verdict{}, fmt.Errorf("nono verify: write probe config: %w", err)
	}
	outPath := filepath.Join(work, "observations.json")

	// Build the rein-representative profile and write it into the scratch dir.
	prof, err := Build(Params{
		ListenAddr:   listenAddr,
		CACertPath:   caPath,
		ExtraDomains: vp.ExtraDomains,
		Name:         "rein-containment-probe",
		Description:  "rein containment prober profile (generated).",
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("nono verify: build profile: %w", err)
	}
	profBytes, err := prof.MarshalIndent()
	if err != nil {
		return Verdict{}, fmt.Errorf("nono verify: marshal profile: %w", err)
	}
	profPath := filepath.Join(work, "profile.json")
	if err := os.WriteFile(profPath, profBytes, 0o600); err != nil {
		return Verdict{}, fmt.Errorf("nono verify: write profile: %w", err)
	}

	stderr, runErr := launchProbe(nonoBin, vp.ReinBin, profPath, work, outPath, cfgPath, tmuxFx.readDirs(), vp.Timeout)

	// Read observations regardless of nono's exit — a leak still produces output.
	obs, oerr := readObservations(outPath)
	if oerr != nil {
		return Verdict{}, fmt.Errorf("nono verify: probe produced no readable observations (failing closed): %w; nono/probe error: %v; stderr: %s",
			oerr, runErr, trimOutput(stderr))
	}
	// Overlay host-side existence truth (the sandbox may not even stat a
	// deny_credentials path, so we trust the host for Existed).
	for i := range obs.Creds {
		obs.Creds[i].Existed = credExist[obs.Creds[i].Path]
	}
	// Overlay host truth: if tmux is present but the fixture failed to stage, the
	// approval-socket/send-keys proof was skipped despite a real approval surface —
	// don't let that pass silently (fail closed under RequireControls below).
	obs.ApprovalFixtureErr = tmuxFx.stageError()

	// The gate guarantees the planted loopback listener is live and that direct
	// TCP is seccomp-denied pre-network, so an Unknown on those controls is an
	// errno drift that must fail closed, not pass.
	pol := vp.Policy
	pol.RequireControls = true
	verdict := Classify(obs, pol)
	if verdict.ShouldFailClosed() {
		return verdict, fmt.Errorf("nono verify: CONTAINMENT FAILURE — refusing to launch:\n%s", verdict.String())
	}
	return verdict, nil
}

// launchProbe runs the probe through nono under session isolation and returns
// nono's stderr and any run error.
//
// Session isolation is done with SysProcAttr{Setsid:true} on nono ITSELF, not an
// external `setsid` wrapper: that makes nono a new session (no controlling tty,
// detached from the caller's — and the user's tmux — session) AND makes
// cmd.Process.Pid the session/group leader, so a timeout kill(-pid) reaches
// nono's whole supervised tree. A `setsid -w` wrapper would put nono in a pgid
// != cmd.Process.Pid, so the group kill would miss it (nono orphaned to init).
// cmd.Run waits for nono to exit, so there is no race with the child (a BARE
// `setsid` returns immediately and races it).
func launchProbe(nonoBin, reinBin, profPath, work, outPath, cfgPath string, extraReadDirs []string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := []string{
		"run", "-p", profPath,
		"--allow", work, // config + observations output
		"--read-file", reinBin, // the probe binary must be readable to exec
	}
	// Read-only grants so the in-sandbox send-keys probe can exec the dynamically
	// linked tmux (binary + libs). These do NOT weaken the other channels (they
	// test network + $HOME creds, not /usr /lib) and never grant the fixture dir.
	for _, d := range extraReadDirs {
		args = append(args, "--read", d)
	}
	args = append(args, "--", reinBin, "__nono-probe", outPath, cfgPath)
	cmd := exec.CommandContext(ctx, nonoBin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// On timeout, kill the whole session group (nono + its supervised children),
	// not just the direct child. cmd.Process.Pid is the session leader, so -pid
	// targets the group.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// Never inherit HTTP(S)_PROXY etc. from the parent — nono sets the sandbox's
	// own; a stray parent value must not leak in. Start from a minimal env.
	cmd.Env = minimalEnv()
	cmd.Stdin = nil // os/exec wires /dev/null
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = nil

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		// Belt-and-suspenders: ensure the group is gone even if Cancel raced.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return stderr.Bytes(), fmt.Errorf("probe timed out after %s (sandbox may be wedged)", timeout)
	}
	return stderr.Bytes(), err
}

func readObservations(path string) (Observations, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Observations{}, err
	}
	var obs Observations
	if err := json.Unmarshal(raw, &obs); err != nil {
		return Observations{}, fmt.Errorf("parse observations: %w", err)
	}
	return obs, nil
}

// startLoopbackListener opens a TCP listener on 127.0.0.1:0 that accepts and
// answers one byte per connection (so the probe's round-trip proves a real
// reach). Returns its addr and a closer.
func startLoopbackListener() (addr string, closer func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 64)
				_, _ = conn.Read(buf)
				_, _ = conn.Write([]byte("ok\n"))
			}(c)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }, nil
}

// denyCredDirs are deny_credentials-covered directories good for a sentinel
// canary (a deterministic, non-secret marker file we plant then remove).
func denyCredDirs() []string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".config", "rein-credentials"),
		filepath.Join(home, ".config", "gh"),
		filepath.Join(home, ".aws"),
	}
}

const sentinelMarker = "REIN-NONO-CONTAINMENT-SENTINEL-DO-NOT-LEAK-3f9c1a"

// stageCredTargets builds the credential target list (real paths + a planted
// sentinel), records host-side existence, and returns a cleanup for the
// sentinel. A sentinel readable-back inside the sandbox is a definitive
// deny_credentials leak even when no real cred files are present.
func stageCredTargets(paths []string) (targets []CredTarget, existed map[string]bool, cleanup func()) {
	existed = map[string]bool{}
	cleanup = func() {}

	if len(paths) == 0 {
		home, _ := os.UserHomeDir()
		if home != "" {
			paths = []string{
				filepath.Join(home, ".config", "rein-credentials", "app.pem"),
				filepath.Join(home, ".config", "gh", "hosts.yml"),
				filepath.Join(home, ".ssh", "id_ed25519"),
				filepath.Join(home, ".ssh", "id_rsa"),
			}
		}
	}
	for _, p := range paths {
		ex := fileExists(p)
		existed[p] = ex
		targets = append(targets, CredTarget{Path: p, Sentinel: false})
	}

	// Plant a sentinel in the first writable deny_credentials dir.
	for _, dir := range denyCredDirs() {
		if !dirExists(dir) {
			continue
		}
		sp := filepath.Join(dir, "rein-probe-sentinel-"+strconv.FormatInt(time.Now().UnixNano(), 36))
		if err := os.WriteFile(sp, []byte(sentinelMarker), 0o600); err != nil {
			continue
		}
		existed[sp] = true
		targets = append(targets, CredTarget{Path: sp, Sentinel: true, Marker: sentinelMarker})
		cleanup = func() { _ = os.Remove(sp) }
		break
	}
	return targets, existed, cleanup
}

// tmuxFixture is a DEDICATED throwaway tmux server used to test approval-channel
// isolation (§3e). It is NEVER the operator's server: it runs on its own socket
// in an own temp dir, started with $TMUX/$TMUX_PANE stripped, and killed in a
// defer. usable is set only when the server started AND passed a liveness check,
// so a "denied" verdict is never vacuous (a dead fixture is a skip, not a leak).
type tmuxFixture struct {
	bin     string
	dir     string
	socket  string
	session string
	present bool // tmux is installed on the host (a real approval surface exists)
	server  bool // a server was started (must be killed)
	usable  bool // started AND live — the probe may target it
}

// stageError is non-empty when tmux IS present but the fixture could not be
// staged, so the socket/send-keys enforcement proof was skipped even though a
// real approval surface exists. Overlaid onto Observations so Classify fails
// closed (under RequireControls) instead of silently passing. Empty when tmux is
// simply absent (a legitimate clean skip).
func (f tmuxFixture) stageError() string {
	if f.present && !f.usable {
		return "tmux is installed but the approval-isolation fixture could not be staged (server start/liveness failed); the socket/send-keys enforcement proof was skipped"
	}
	return ""
}

func (f tmuxFixture) probeSocket() string {
	if f.usable {
		return f.socket
	}
	return ""
}
func (f tmuxFixture) probeBin() string {
	if f.usable {
		return f.bin
	}
	return ""
}
func (f tmuxFixture) probeSession() string {
	if f.usable {
		return f.session
	}
	return ""
}

// readDirs are the read-only grants the send-keys probe needs to exec the
// dynamically linked tmux inside the sandbox. Only when the fixture is usable.
func (f tmuxFixture) readDirs() []string {
	if f.usable {
		return []string{"/usr", "/lib"}
	}
	return nil
}

// cleanup kills the dedicated server (only ever OUR socket) and removes its dir.
func (f tmuxFixture) cleanup() {
	if f.server && f.bin != "" && f.socket != "" {
		c := exec.Command(f.bin, "-S", f.socket, "kill-server")
		c.Env = withoutTmux(os.Environ())
		_ = c.Run()
	}
	if f.dir != "" {
		_ = os.RemoveAll(f.dir)
	}
}

// stageTmuxFixture starts a dedicated tmux server on a socket in a NON-granted
// temp dir. On any failure (tmux absent, mkdir, start, or a failed liveness
// check) it returns a non-usable fixture so the approval-socket/send-keys checks
// are SKIPPED — a machine without tmux must not fail the launch gate. The
// non-granted dir is deliberate: §3e showed fs-deny does not block AF_UNIX
// connect, so a denied connect proves af_unix_mediation is enforced at runtime.
func stageTmuxFixture() tmuxFixture {
	var f tmuxFixture
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return f // tmux absent — legitimate clean skip (no approval surface)
	}
	f.present = true // tmux exists: a real approval surface. From here a failure
	// is a fail-open risk (stageError), not a clean skip.
	dir, err := os.MkdirTemp("", "rein-approvalfix-*")
	if err != nil {
		return f
	}
	f.bin, f.dir, f.session = bin, dir, "probe"
	f.socket = filepath.Join(dir, "tmux.sock")
	env := withoutTmux(os.Environ())

	start := exec.Command(bin, "-S", f.socket, "new-session", "-d", "-s", f.session)
	start.Env = env
	start.Stdin = nil
	if err := start.Run(); err != nil {
		return f // could not start — skip (dir still cleaned)
	}
	f.server = true

	// Liveness positive control: a "denied" connect is only meaningful against a
	// server that is actually up.
	ls := exec.Command(bin, "-S", f.socket, "list-sessions")
	ls.Env = env
	if err := ls.Run(); err != nil {
		return f // started but not live — skip (cleanup still kills it)
	}
	f.usable = true
	return f
}

// withoutTmux drops $TMUX/$TMUX_PANE so a tmux command starting the fixture can
// never attach to or nest inside the operator's session.
func withoutTmux(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// resolveGitHub resolves github.com host-side to an IP:443 for the direct-connect
// (must-be-blocked) check. Returns "" if unresolvable, which classifies the
// channel unknown rather than falsely ok.
func resolveGitHub() string {
	ips, err := net.LookupIP("github.com")
	if err != nil || len(ips) == 0 {
		return ""
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return net.JoinHostPort(v4.String(), "443")
		}
	}
	return net.JoinHostPort(ips[0].String(), "443")
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// minimalEnv returns an environment with PATH/HOME but stripped of any inherited
// proxy vars, so the probe launch is not skewed by the parent's env.
func minimalEnv() []string {
	keep := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "TERM": true, "LANG": true}
	var out []string
	for _, kv := range os.Environ() {
		if i := indexByte(kv, '='); i > 0 && keep[kv[:i]] {
			out = append(out, kv)
		}
	}
	return out
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func trimOutput(b []byte) string {
	const max = 600
	s := string(b)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
