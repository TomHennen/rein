package srt

import (
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
