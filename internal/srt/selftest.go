package srt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Probe exit codes. The probe (`rein __sandbox-probe`) runs INSIDE the sandbox
// and reports, via its exit code, whether srt's two fail-open protections
// actually took effect. srt propagates the child's exit code, so
// VerifyConfigApplied reads these back from the srt process.
const (
	// ProbeOK: BOTH protections verified — AF_UNIX socket creation was blocked
	// (seccomp on) AND the sentinel read back empty/absent (denyRead applied).
	ProbeOK = 0
	// ProbeSeccompFailOpen: socket(AF_UNIX) SUCCEEDED inside the sandbox — the
	// seccomp unix-socket block is NOT in effect (the agent could reach
	// keyring/ssh-agent sockets). This is gap #4 firing.
	ProbeSeccompFailOpen = 10
	// ProbeDenyReadFailOpen: the sentinel file read back its known marker
	// content inside the sandbox — denyRead did NOT apply, so credential stores
	// would be readable. On 0.0.63's `-s` path srt refuses to run rather than
	// applying an empty-denyRead default (see config.go), so this is a
	// version-drift / denyRead-semantics guard rather than a live gap #3 on this
	// version — but it must still hard-fail the launch if it ever fires.
	ProbeDenyReadFailOpen = 11
	// ProbeError: the probe hit an internal error (couldn't run its checks).
	// Treated as fail-closed by the verifier.
	ProbeError = 12
)

// sentinelMarker is the known content written to the host-side sentinel file.
// If the in-sandbox probe reads these exact bytes back, the denyRead did not
// take effect. Not a secret — it's a canary, chosen to be unmistakable.
const sentinelMarker = "REIN-SANDBOX-SENTINEL-DO-NOT-LEAK-8f3a1c2b"

// VerifyParams are the inputs to VerifyConfigApplied.
type VerifyParams struct {
	// Base is the real per-run Params (working tree, socket, denyRead cred
	// stores, runtime dirs). VerifyConfigApplied augments a COPY of it with the
	// sentinel path, so the config it proves is the real config plus one extra
	// denyRead entry — a strict superset. If the real config would null-fallback
	// (empty denyRead), so does the superset, and the sentinel read-back catches
	// it. A pass therefore proves the real config applies.
	Base Params

	// SrtPath is the srt binary (resolved by preflight). Required.
	SrtPath string

	// ReinBin is the absolute path to the rein binary to invoke as the in-sandbox
	// probe (`rein __sandbox-probe`). Must be executable and readable inside the
	// sandbox (the default ro-bind of / covers it, as long as it is not under a
	// denyRead path). Required.
	ReinBin string

	// Env is the scrubbed exec environment (from BuildEnv). srt needs PATH to
	// find bwrap/socat/rg/bash. Required.
	Env []string

	// Timeout caps the probe spawn. A stuck sandbox must not hang the launch.
	Timeout time.Duration
}

// VerifyConfigApplied launches srt with the real per-run filesystem/seccomp
// config (plus a sentinel denyRead entry) running the in-sandbox probe, and
// returns nil ONLY if the probe confirms both protections took effect. Any
// other outcome — a fail-open exit code, a non-zero probe error, or srt failing
// to run at all — returns a non-nil error, and the caller MUST fail the launch
// closed. This is the real guarantee that srt did not silently disarm rein's
// protections; the typed config + Validate are only the first line.
func VerifyConfigApplied(vp VerifyParams) error {
	if vp.SrtPath == "" || vp.ReinBin == "" {
		return fmt.Errorf("srt verify: SrtPath and ReinBin are required")
	}
	if vp.Timeout <= 0 {
		vp.Timeout = 30 * time.Second
	}

	// Host-side sentinel: a real, readable file with known content, placed in
	// its own temp dir so we can denyRead the FILE precisely. Outside the
	// working tree (which is allowWrite) so nothing re-binds it back.
	sentDir, err := os.MkdirTemp("", "rein-sentinel-*")
	if err != nil {
		return fmt.Errorf("srt verify: create sentinel dir: %w", err)
	}
	defer os.RemoveAll(sentDir)
	sentinel := filepath.Join(sentDir, "credential-sentinel")
	if err := os.WriteFile(sentinel, []byte(sentinelMarker), 0o600); err != nil {
		return fmt.Errorf("srt verify: write sentinel: %w", err)
	}

	// Build the config to prove: the real Params + the sentinel in denyRead.
	p := vp.Base
	p.SentinelPath = sentinel
	cfg, err := Build(p)
	if err != nil {
		return fmt.Errorf("srt verify: build config: %w", err)
	}
	data, err := cfg.MarshalIndent()
	if err != nil {
		return fmt.Errorf("srt verify: marshal config: %w", err)
	}
	settings := filepath.Join(sentDir, "settings.json")
	if err := os.WriteFile(settings, data, 0o600); err != nil {
		return fmt.Errorf("srt verify: write settings: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), vp.Timeout)
	defer cancel()

	// srt -s <settings> -- <rein> __sandbox-probe <sentinel> <marker>
	cmd := exec.CommandContext(ctx, vp.SrtPath,
		"-s", settings, "--",
		vp.ReinBin, "__sandbox-probe", sentinel, sentinelMarker)
	cmd.Env = vp.Env
	// Capture output for diagnostics; the verdict is the exit code.
	out, runErr := cmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("srt verify: probe timed out after %s (sandbox may be wedged); failing closed. output: %s", vp.Timeout, trim(out))
	}

	code := exitCode(runErr)
	switch code {
	case ProbeOK:
		return nil
	case ProbeSeccompFailOpen:
		return fmt.Errorf("srt verify: SECCOMP FAIL-OPEN — AF_UNIX socket creation was NOT blocked inside the sandbox; the agent could reach keyring/ssh-agent sockets. Refusing to launch (gap #4). output: %s", trim(out))
	case ProbeDenyReadFailOpen:
		return fmt.Errorf("srt verify: CONFIG FAIL-OPEN — the credential sentinel was READABLE inside the sandbox; srt did not apply rein's denyRead (likely null-fallback to the default config with empty denyRead). Refusing to launch (gap #3). output: %s", trim(out))
	case ProbeError:
		return fmt.Errorf("srt verify: probe reported an internal error; failing closed. output: %s", trim(out))
	default:
		// srt itself failed to start, crashed, or returned an unexpected code.
		return fmt.Errorf("srt verify: probe returned unexpected exit code %d (srt may have failed to launch); failing closed. output: %s", code, trim(out))
	}
}

// RunProbe is the body of the hidden `rein __sandbox-probe` subcommand. It runs
// INSIDE the sandbox and returns the exit code the parent VerifyConfigApplied
// interprets. args are the trailing argv after "__sandbox-probe":
// [sentinelPath, expectedMarker].
//
//   - socket(AF_UNIX) must FAIL. If it succeeds, seccomp is not applied.
//   - reading sentinelPath must NOT yield expectedMarker. denyRead of a FILE
//     ro-binds /dev/null (reads empty); denyRead of a DIR tmpfs's it (file
//     absent). Either way the marker must be gone. If the marker comes back,
//     denyRead did not apply.
func RunProbe(args []string) int {
	if len(args) < 2 {
		return ProbeError
	}
	sentinelPath, marker := args[0], args[1]

	if socketAFUnixSucceeds() {
		return ProbeSeccompFailOpen
	}

	data, err := os.ReadFile(sentinelPath)
	if err == nil && string(data) == marker {
		return ProbeDenyReadFailOpen
	}
	// err != nil (absent/tmpfs) or empty (/dev/null bind) => denyRead applied.
	return ProbeOK
}

func trim(b []byte) string {
	const max = 400
	s := string(b)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// exitCode extracts a process exit code from an *exec.ExitError, returning -1
// for a nil error's success is 0 and for a non-exit error (couldn't start).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}
