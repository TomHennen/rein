package srt

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestVerifyConfigApplied_E2E launches REAL srt with the probe and asserts the
// deny-read + seccomp self-test passes for a well-formed config, AND fails
// closed when denyRead is stripped. Gated behind REIN_SANDBOX_E2E because it
// needs srt + bwrap + seccomp on the box; `go test ./...` stays hermetic.
//
// Run: REIN_SANDBOX_E2E=1 go test ./internal/srt/ -run E2E -v
func TestVerifyConfigApplied_E2E(t *testing.T) {
	if os.Getenv("REIN_SANDBOX_E2E") == "" {
		t.Skip("set REIN_SANDBOX_E2E=1 to run the live srt self-test")
	}
	srtPath, err := exec.LookPath("srt")
	if err != nil {
		t.Fatalf("srt not on PATH: %v", err)
	}

	// Build a rein binary to serve as the in-sandbox probe (`rein __sandbox-probe`).
	reinBin := buildReinForProbe(t)

	workTree := t.TempDir()
	env := BuildEnv(EnvParams{
		Parent:       os.Environ(),
		CABundlePath: filepath.Join(t.TempDir(), "unused-bundle.pem"),
		StubGHToken:  "stub",
	})

	base := Params{
		SocketPath:  filepath.Join(t.TempDir(), "proxy.sock"),
		WorkingTree: workTree,
	}

	// Positive: a correct config must verify clean.
	if err := VerifyConfigApplied(VerifyParams{
		Base: base, SrtPath: srtPath, ReinBin: reinBin, Env: env, Timeout: 30 * time.Second,
	}); err != nil {
		t.Fatalf("VerifyConfigApplied on a correct config failed: %v", err)
	}
	t.Log("positive: correct config verified (deny-read + seccomp both proven applied)")

	// Negative: prove the self-test actually catches a fail-open. We can't easily
	// force srt to drop denyRead, so we assert the mechanism: a config whose
	// denyRead does NOT include the sentinel should let the probe read it back
	// (ProbeDenyReadFailOpen). VerifyConfigApplied always adds the sentinel, so
	// we exercise the probe directly against a hand-built config here.
	assertSentinelDetection(t, srtPath, reinBin, env, workTree)
}

// assertSentinelDetection builds a config that does NOT deny-read a sentinel,
// launches srt+probe against it, and asserts the probe reports the deny-read
// fail-open code — proving the content-comparison catches a config that didn't
// hide the credential.
func assertSentinelDetection(t *testing.T, srtPath, reinBin string, env []string, workTree string) {
	t.Helper()
	sentDir := t.TempDir()
	sentinel := filepath.Join(sentDir, "cred")
	if err := os.WriteFile(sentinel, []byte(sentinelMarker), 0o600); err != nil {
		t.Fatal(err)
	}
	// Config with the sentinel NOT in denyRead.
	cfg, err := Build(Params{SocketPath: filepath.Join(sentDir, "p.sock"), WorkingTree: workTree})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := cfg.MarshalIndent()
	settings := filepath.Join(sentDir, "settings.json")
	if err := os.WriteFile(settings, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(srtPath, "-s", settings, "--", reinBin, "__sandbox-probe", sentinel, sentinelMarker)
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	code := exitCode(runErr)
	if code != ProbeDenyReadFailOpen {
		t.Errorf("probe against a config NOT denying the sentinel returned %d, want ProbeDenyReadFailOpen(%d); the content-sentinel is not detecting a readable credential. output: %s",
			code, ProbeDenyReadFailOpen, strings.TrimSpace(string(out)))
	} else {
		t.Log("negative: sentinel readable => ProbeDenyReadFailOpen (content-comparison works)")
	}
}

// TestDenyHomeApplied_E2E live-probes the #59 wholesale $HOME deny against
// REAL srt: a canary file in the actual home directory must be unreadable
// in-sandbox under DenyReadHome, readable again when its dir is allowed back
// (allowRead-within-deny), and readable when the deny is absent (negative
// control proving the canary itself works). Reuses the __sandbox-probe
// machinery: marker read back => ProbeDenyReadFailOpen, hidden => ProbeOK.
//
// Run: REIN_SANDBOX_E2E=1 go test ./internal/srt/ -run E2E -v
func TestDenyHomeApplied_E2E(t *testing.T) {
	if os.Getenv("REIN_SANDBOX_E2E") == "" {
		t.Skip("set REIN_SANDBOX_E2E=1 to run the live srt home-deny test")
	}
	srtPath, err := exec.LookPath("srt")
	if err != nil {
		t.Fatalf("srt not on PATH: %v", err)
	}
	reinBin := buildReinForProbe(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatalf("resolve home: %v", err)
	}

	// Canary INSIDE the real home dir (that is the point of the test); its
	// own temp dir so cleanup is exact and an allow-back can target it.
	canaryDir, err := os.MkdirTemp(home, ".rein-e2e-canary-*")
	if err != nil {
		t.Fatalf("create canary dir in home: %v", err)
	}
	defer os.RemoveAll(canaryDir)
	canary := filepath.Join(canaryDir, "cred")
	if err := os.WriteFile(canary, []byte(sentinelMarker), 0o600); err != nil {
		t.Fatal(err)
	}

	env := BuildEnv(EnvParams{
		Parent:       os.Environ(),
		CABundlePath: filepath.Join(t.TempDir(), "unused-bundle.pem"),
		StubGHToken:  "stub",
	})
	workTree := t.TempDir()

	probe := func(name string, p Params, wantCode int) {
		t.Helper()
		dir := t.TempDir()
		cfg, err := Build(p)
		if err != nil {
			t.Fatalf("%s: Build: %v", name, err)
		}
		data, _ := cfg.MarshalIndent()
		settings := filepath.Join(dir, "settings.json")
		if err := os.WriteFile(settings, data, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(srtPath, "-s", settings, "--", reinBin, "__sandbox-probe", canary, sentinelMarker)
		cmd.Env = env
		out, runErr := cmd.CombinedOutput()
		if code := exitCode(runErr); code != wantCode {
			t.Errorf("%s: probe exit = %d, want %d. output: %s", name, code, wantCode, strings.TrimSpace(string(out)))
		} else {
			t.Logf("%s: ok (exit %d)", name, code)
		}
	}

	// srt's OWN install chain must be allowed back whenever $HOME is denied:
	// srt executes its vendored apply-seccomp helper INSIDE the bwrap
	// namespace, and on an npm-under-$HOME install (this box:
	// ~/.npm-global/...) the home tmpfs hides it — the sandbox bootstrap dies
	// with exit 127 before the child starts. First observed LIVE by this very
	// test; production mirrors this via sandboxAllowReadPaths (cmd/rein).
	srtChain := srtInstallAllowReads(t, srtPath)
	t.Logf("srt install chain allow-backs: %v", srtChain)

	base := Params{
		SocketPath:  filepath.Join(t.TempDir(), "proxy.sock"),
		WorkingTree: workTree,
	}

	// (1) Home denied wholesale: canary must be GONE (ProbeOK = marker not read).
	p := base
	p.DenyReadHome = home
	p.AllowRead = srtChain
	probe("deny-home hides canary", p, ProbeOK)

	// (2) Allow the canary's dir back: readable again — allowRead-within-deny
	// works end to end (probe reads the marker => ProbeDenyReadFailOpen, which
	// HERE means the allow-back functioned).
	p = base
	p.DenyReadHome = home
	p.AllowRead = append([]string{canaryDir}, srtChain...)
	probe("allow-back re-exposes canary", p, ProbeDenyReadFailOpen)

	// (3) Negative control, no home deny: canary readable (proves the canary
	// detects readability at all — same rationale as assertSentinelDetection).
	probe("no deny leaves canary readable", base, ProbeDenyReadFailOpen)

	// (4) Belt-and-suspenders composition: the canary FILE explicitly denied
	// while its dir is allowed back under the home deny — the file deny must
	// beat the dir allow (srt exact-match rule), so the marker stays hidden.
	p = base
	p.DenyReadHome = home
	p.AllowRead = append([]string{canaryDir}, srtChain...)
	p.DenyReadCredStores = []string{canary}
	probe("file deny beats dir allow-back", p, ProbeOK)
}

// srtInstallAllowReads mirrors cmd/rein's install-chain derivation for the
// srt binary itself, scoped to what this test needs: the PATH entry's dir and
// the resolved target's node_modules root (or containing dir). Entries not
// under a denied path are harmless no-ops for srt.
func srtInstallAllowReads(t *testing.T, srtPath string) []string {
	t.Helper()
	abs, err := filepath.Abs(srtPath)
	if err != nil {
		t.Fatalf("abs srt path: %v", err)
	}
	out := []string{filepath.Dir(abs)}
	if target, err := filepath.EvalSymlinks(abs); err == nil {
		if i := strings.LastIndex(target, "/node_modules/"); i >= 0 {
			out = append(out, target[:i+len("/node_modules")])
		} else {
			out = append(out, filepath.Dir(target))
		}
	}
	return out
}

func buildReinForProbe(t *testing.T) string {
	t.Helper()
	// Find the module root (dir with go.mod) walking up from CWD.
	dir, _ := os.Getwd()
	root := dir
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("could not find go.mod")
		}
		root = parent
	}
	bin := filepath.Join(t.TempDir(), "rein")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/rein")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build rein for probe: %v\n%s", err, out)
	}
	return bin
}

// TestHomeWriteSemantics_E2E pins the THREE distinct $HOME behaviors the launch
// banner promises (cmd/rein/run_sandboxed.go, printSandboxBanner) to what REAL
// srt 0.0.63 + bwrap actually do. The banner is a security-UX contract: if srt
// ever changes these semantics, the banner silently becomes a lie, and the
// operator's mental model of where their work persists goes with it.
//
// The three, each asserted below:
//
//	(1) a hidden $HOME path READS as absent               — ENOENT, fails loudly;
//	(2) a write elsewhere under hidden $HOME SUCCEEDS     — into the deny tmpfs...
//	    ...and does NOT reach the host (checked after the run) — evaporating;
//	(3) a write to an ALLOWED-BACK path FAILS with EROFS  — allowRead is a
//	    read-only bind, so these writes ERROR rather than evaporate.
//
// (2) is why rein does NOT make hidden-$HOME writes error (issue #63): the deny
// tmpfs is what keeps ordinary tooling alive on a cold cache. A read-only $HOME
// hard-fails `go build` at build-cache init ("failed to initialize build cache
// ... read-only file system") — measured on this box — whereas under the tmpfs
// it simply rebuilds. srt cannot express it anyway: a denyWrite entry for a dir
// already in denyRead is SKIPPED ("Skipping denyWrite bind already hidden by
// denyRead tmpfs", linux-sandbox-utils.js), so denyWrite does not stack.
//
// Run: REIN_SANDBOX_E2E=1 go test ./internal/srt/ -run E2E -v
func TestHomeWriteSemantics_E2E(t *testing.T) {
	if os.Getenv("REIN_SANDBOX_E2E") == "" {
		t.Skip("set REIN_SANDBOX_E2E=1 to run the live srt home-write-semantics test")
	}
	srtPath, err := exec.LookPath("srt")
	if err != nil {
		t.Fatalf("srt not on PATH: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	if home, err = filepath.EvalSymlinks(home); err != nil {
		t.Fatalf("resolve home: %v", err)
	}

	// An allowed-back dir (read-only bind) with a file in it, inside the real
	// home — this is the shape of ~/.claude, ~/.cargo, ~/go in production.
	allowDir, err := os.MkdirTemp(home, ".rein-e2e-rw-*")
	if err != nil {
		t.Fatalf("create allow-back dir in home: %v", err)
	}
	defer os.RemoveAll(allowDir)
	if err := os.WriteFile(filepath.Join(allowDir, "readable"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A HIDDEN file: sits directly in $HOME, never allowed back.
	hidden := filepath.Join(home, ".rein-e2e-hidden-probe")
	if err := os.WriteFile(hidden, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(hidden)
	// The path the sandbox will write into the tmpfs. It must NOT exist on the
	// host afterwards — that is the "evaporates" half of the contract.
	ephemeral := filepath.Join(home, ".rein-e2e-ephemeral-write")
	if err := os.Remove(ephemeral); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	defer os.Remove(ephemeral)

	workTree := t.TempDir()
	cfg, err := Build(Params{
		SocketPath:   filepath.Join(t.TempDir(), "proxy.sock"),
		WorkingTree:  workTree,
		DenyReadHome: home,
		AllowRead:    append(srtInstallAllowReads(t, srtPath), allowDir),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	data, _ := cfg.MarshalIndent()
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settings, data, 0o600); err != nil {
		t.Fatal(err)
	}

	// Distinct exit codes so a failure names the broken clause, not just "exit 1".
	script := fmt.Sprintf(`
		# (1) hidden path must read as ABSENT.
		[ -e %[1]q ] && exit 21
		# (2) write elsewhere under hidden $HOME must SUCCEED (deny tmpfs is rw).
		echo scratch > %[2]q || exit 22
		[ "$(cat %[2]q)" = scratch ] || exit 23
		# (3) the allowed-back dir must be READABLE but its writes must EROFS.
		[ "$(cat %[3]q)" = hi ] || exit 24
		echo nope > %[4]q 2>/dev/null && exit 25
		# (4) the agent-visible facts must actually SURVIVE srt's env pass.
		[ "$REIN_IN_SANDBOX" = 1 ] || exit 26
		[ "$REIN_IN_SANDBOX_WORKTREE" = %[5]q ] || exit 27
		[ "$REIN_IN_SANDBOX_HOME" = ephemeral ] || exit 28
		exit 0
	`, hidden, ephemeral, filepath.Join(allowDir, "readable"), filepath.Join(allowDir, "written"), workTree)

	cmd := exec.Command(srtPath, "-s", settings, "--", "/bin/sh", "-c", script)
	cmd.Env = BuildEnv(EnvParams{
		Parent:       os.Environ(),
		CABundlePath: filepath.Join(t.TempDir(), "unused-bundle.pem"),
		StubGHToken:  "stub",
		// The #63 agent-visible channel, asserted end-to-end below: rein sets
		// these, but srt re-materializes the child's env through bwrap
		// --setenv/--unsetenv, so "BuildEnv set it" does NOT imply "the agent
		// sees it". If srt ever starts filtering unknown names, this channel
		// becomes dead code and the fix must be found elsewhere — fail here.
		WorkTree:      workTree,
		HomeEphemeral: true,
	})
	out, runErr := cmd.CombinedOutput()
	switch code := exitCode(runErr); code {
	case 0:
		t.Log("in-sandbox: hidden read = ENOENT; hidden-$HOME write = SUCCEEDS (tmpfs); allow-back write = EROFS")
	case 21:
		t.Errorf("hidden $HOME file was VISIBLE in-sandbox — the deny-read tmpfs did not apply")
	case 22, 23:
		t.Errorf("write under hidden $HOME FAILED (exit %d) — srt no longer gives a writable deny tmpfs. "+
			"The banner promises these writes succeed-then-evaporate, and rein relies on it to keep tool "+
			"caches (go-build, ~/.npm) alive. Re-check the #63 ruling. output: %s", code, strings.TrimSpace(string(out)))
	case 24:
		t.Errorf("the allowed-back dir was NOT readable in-sandbox — allowRead within denyRead broke")
	case 25:
		t.Errorf("a write to the ALLOWED-BACK dir SUCCEEDED — allowRead is supposed to be a READ-ONLY bind. "+
			"If it is writable now, the banner's 'writes to allowed-back paths ERROR' clause is wrong, and "+
			"an agent could mutate the developer's real ~/.claude, ~/.cargo, ~/go. output: %s", strings.TrimSpace(string(out)))
	case 26, 27, 28:
		t.Errorf("an agent-visible fact did NOT reach the sandboxed child (exit %d: %s). srt is filtering the "+
			"REIN_IN_SANDBOX_* env vars out, so rein's ONLY channel for telling the AGENT that $HOME is "+
			"ephemeral is dead — the launch banner reaches the human only. See #63.",
			code, map[int]string{26: "REIN_IN_SANDBOX", 27: "REIN_IN_SANDBOX_WORKTREE", 28: "REIN_IN_SANDBOX_HOME"}[code])
	default:
		t.Fatalf("probe exit = %d, want 0. output: %s", code, strings.TrimSpace(string(out)))
	}

	// The evaporation half: the tmpfs write must never have reached the host.
	if _, err := os.Stat(ephemeral); err == nil {
		t.Errorf("the in-sandbox write to %s REACHED THE HOST — the deny tmpfs is not ephemeral, "+
			"and the sandbox is writing into the developer's real home directory", ephemeral)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", ephemeral, err)
	} else {
		t.Log("on host after run: the in-sandbox $HOME write is GONE (evaporated with the tmpfs)")
	}
}
