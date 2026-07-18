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

// VerifyContainment is the function rein's run path calls to gate a launch on
// containment. It returns the classified Verdict and a non-nil error when the
// launch must be refused (a leak, a failed positive control, or the probe could
// not run). When nono is absent it returns ErrNonoUnavailable so CI can skip.
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

	cfg := probeConfig{
		Creds:           credTargets,
		ExternalTarget:  "1.1.1.1:443", // literal, no DNS — blocked at connect, robust offline
		GitHubTarget:    resolveGitHub(),
		PlantedLoopback: planted,
		UDPTarget:       "8.8.8.8:53",
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

	stderr, runErr := launchProbe(nonoBin, vp.ReinBin, profPath, work, outPath, cfgPath, vp.Timeout)

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

	verdict := Classify(obs, vp.Policy)
	if verdict.ShouldFailClosed() {
		return verdict, fmt.Errorf("nono verify: CONTAINMENT FAILURE — refusing to launch:\n%s", verdict.String())
	}
	return verdict, nil
}

// launchProbe runs the probe through nono under session isolation and returns
// nono's stderr and any run error.
func launchProbe(nonoBin, reinBin, profPath, work, outPath, cfgPath string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	nonoArgs := []string{
		"run", "-p", profPath,
		"--allow", work, // config + observations output
		"--read-file", reinBin, // the probe binary must be readable to exec
		"--", reinBin, "__nono-probe", outPath, cfgPath,
	}

	// Prefer `setsid -w` for session isolation + waiting; fall back to a direct
	// nono invocation if setsid is unavailable (our context has no tty anyway).
	var cmd *exec.Cmd
	if sid, err := exec.LookPath("setsid"); err == nil {
		cmd = exec.CommandContext(ctx, sid, append([]string{"-w", nonoBin}, nonoArgs...)...)
	} else {
		cmd = exec.CommandContext(ctx, nonoBin, nonoArgs...)
	}
	// New process group so a timeout kill reaches the whole tree (setsid already
	// makes a new session; this is belt-and-suspenders for the fallback path).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Never inherit HTTP(S)_PROXY etc. from the parent — nono sets the sandbox's
	// own; a stray parent value must not leak in. Start from a minimal env.
	cmd.Env = minimalEnv()
	cmd.Stdin = nil // os/exec wires /dev/null
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = nil

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // kill the group
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
