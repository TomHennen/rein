// nono launch preflight / doctor health checks (design-nono-pivot.md §2.3).
//
// Preflight returns a stable-ordered []sandboxutil.Check; the caller decides
// policy (`rein run` fails closed on any Hard() check, `rein doctor` prints them
// all read-only). Env injects the environment-touching operations so verdict
// logic is unit-testable with fakes.
//
// Any nono invocation is session-isolated (setsid + /dev/null stdin, never a
// tty) per the git-push spike discipline — a stray tty-attached nono disrupts
// the user's tmux session (docs/nono-git-push-spike-findings.md).
package nono

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/keystore"
	"github.com/TomHennen/rein/internal/proxy"
	"github.com/TomHennen/rein/internal/sandboxutil"
)

// Re-export the neutral check types so callers read nono.Check / nono.StatusOK
// without importing sandboxutil directly (mirrors srt's aliasing).
type (
	Status = sandboxutil.Status
	Check  = sandboxutil.Check
)

const (
	StatusOK   = sandboxutil.StatusOK
	StatusWarn = sandboxutil.StatusWarn
	StatusFail = sandboxutil.StatusFail
)

// afUnixProbeProfile is a minimal profile whose ONLY content is the approval-
// channel isolation control. This probes SCHEMA ACCEPTANCE, not runtime
// enforcement: nono's schema is strict (it rejects unknown fields AND unknown
// enum variants — verified for 0.68.0, 2026-07-18), so a build that renamed or
// dropped the field would fail to validate this — a schema-drop tripwire. Actual
// enforcement for the pinned version is guaranteed by the SHA-256 digest pin in
// checkNonoPresent; when bumping PinnedVersion, re-verify enforcement out-of-band
// (the containment prober), not by this row alone.
const afUnixProbeProfile = `{"linux":{"af_unix_mediation":"pathname"}}`

// skipNonoUnavailable is the neutral skip message for the binary-dependent rows:
// it covers BOTH nono-absent and nono-present-but-unverified (checkNonoPresent
// returns nonoPath="" for either — we must never exec an unverified binary).
const skipNonoUnavailable = "skipped: nono unavailable (see `nono present`)"

// Env injects the environment-touching operations so the pass/fail decisions are
// unit-testable with stubbed inputs. Production builds it with DefaultEnv.
type Env struct {
	// NonoPath resolves the rein-MANAGED nono binary (ManagedNonoPath in prod).
	// Empty path + error ⇒ nono not installed. nono is ALWAYS invoked by this
	// absolute path, never exec.LookPath, so the agent's $PATH cannot shadow it.
	NonoPath func() (string, error)
	// Stat stats a path (os.Stat in prod).
	Stat func(string) (os.FileInfo, error)
	// Platform maps GOOS/GOARCH to the nono target triple (DetectPlatform in prod).
	Platform func() (string, error)
	// VerifyInstalled re-hashes the on-disk binary against the vendored pin.
	VerifyInstalled func(path, version, platform string) error
	// RunNono runs the nono binary session-isolated (setsid, /dev/null stdin) and
	// returns combined output plus the process exit error (non-nil ⇒ non-zero exit).
	RunNono func(nonoPath string, args ...string) ([]byte, error)
	// BuildProfileJSON returns a representative rein profile as JSON to validate.
	BuildProfileJSON func() ([]byte, error)
	// CAFile returns the path rein stores its CA at and whether it is a present,
	// non-empty regular file. err is non-nil only for an unexpected stat failure
	// (a permission problem on a file that IS there) — a plain absent CA is
	// (path, false, nil).
	CAFile func() (path string, presentNonEmpty bool, err error)
	// BindLoopback attempts to bind a 127.0.0.1 TCP port and closes it again.
	// nil ⇒ rein can host its loopback proxy listener.
	BindLoopback func() error
}

// DefaultEnv wires the production implementations.
func DefaultEnv() Env {
	return Env{
		NonoPath:         ManagedNonoPath,
		Stat:             os.Stat,
		Platform:         DetectPlatform,
		VerifyInstalled:  VerifyInstalled,
		RunNono:          runNonoIsolated,
		BuildProfileJSON: defaultProfileJSON,
		CAFile:           defaultCAFile,
		BindLoopback:     bindLoopbackProbe,
	}
}

// Preflight runs every nono launch gate and returns the results in a stable
// order. nonoPath (resolved once) is threaded so the binary-dependent checks
// share it and degrade uniformly when nono is absent.
func Preflight(env Env) []Check {
	nonoPath, presentCheck := checkNonoPresent(env)
	return []Check{
		presentCheck,
		checkProfileValidate(env, nonoPath),
		checkCAFile(env),
		checkLoopbackPort(env),
		checkAfUnixMediation(env, nonoPath),
	}
}

// checkNonoPresent verifies the managed nono binary exists AND its on-disk
// SHA-256 matches the vendored pin for this (version, platform). HARD by design
// (fail closed): a `rein run --nono` cannot proceed without a pinned, verified
// runtime. Returns the resolved path ("" when absent/unverified) for the
// binary-dependent checks below.
func checkNonoPresent(env Env) (string, Check) {
	path, err := env.NonoPath()
	if err != nil {
		return "", Check{Name: "nono present", Status: StatusFail, Message: fmt.Sprintf("cannot resolve managed nono path: %v", err)}
	}
	if _, err := env.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", Check{Name: "nono present", Status: StatusFail,
				Message: fmt.Sprintf("nono not installed at %s; install the pinned runtime (nono %s)", path, PinnedVersion)}
		}
		return "", Check{Name: "nono present", Status: StatusFail, Message: fmt.Sprintf("stat %s: %v", path, err)}
	}
	platform, err := env.Platform()
	if err != nil {
		return "", Check{Name: "nono present", Status: StatusFail, Message: err.Error()}
	}
	if err := env.VerifyInstalled(path, PinnedVersion, platform); err != nil {
		// Digest mismatch ⇒ the on-disk binary is not the verified pinned build.
		return "", Check{Name: "nono present", Status: StatusFail,
			Message: fmt.Sprintf("nono at %s failed pin verification: %v", path, err)}
	}
	return path, Check{Name: "nono present", Status: StatusOK, Message: fmt.Sprintf("%s (pinned %s, digest ok)", path, PinnedVersion)}
}

// checkProfileValidate builds a representative rein profile and runs it through
// `nono profile validate`. HARD when nono reports the profile invalid — the
// launch would emit a profile nono refuses. WARN (skip, not Fail) when nono is
// absent so `rein doctor` still completes on a box without nono installed.
func checkProfileValidate(env Env, nonoPath string) Check {
	const name = "nono profile validate"
	if nonoPath == "" {
		return Check{Name: name, Status: StatusWarn, Message: skipNonoUnavailable}
	}
	profile, err := env.BuildProfileJSON()
	if err != nil {
		// A profile rein cannot even build is a hard problem regardless of nono.
		return Check{Name: name, Status: StatusFail, Message: fmt.Sprintf("build rein profile: %v", err)}
	}
	if verr := validateProfile(env, nonoPath, profile); verr != nil {
		return Check{Name: name, Status: StatusFail, Message: fmt.Sprintf("nono rejected the generated rein profile: %s", verr)}
	}
	return Check{Name: name, Status: StatusOK, Message: "generated rein profile passes `nono profile validate`"}
}

// checkAfUnixMediation confirms the pinned nono's SCHEMA accepts
// af_unix_mediation:"pathname" — the field the approval-channel isolation rests
// on. HARD: a build that renamed/dropped the field fails this probe. This
// verifies schema acceptance, not runtime enforcement (see afUnixProbeProfile);
// enforcement for the pinned version comes from the digest pin in
// checkNonoPresent. WARN (skip) when nono is unavailable.
func checkAfUnixMediation(env Env, nonoPath string) Check {
	const name = "nono af_unix_mediation"
	if nonoPath == "" {
		return Check{Name: name, Status: StatusWarn, Message: skipNonoUnavailable}
	}
	if verr := validateProfile(env, nonoPath, []byte(afUnixProbeProfile)); verr != nil {
		return Check{Name: name, Status: StatusFail,
			Message: fmt.Sprintf("nono %s rejects af_unix_mediation:\"pathname\" (approval-channel isolation unavailable): %s", PinnedVersion, verr)}
	}
	return Check{Name: name, Status: StatusOK, Message: "af_unix_mediation:\"pathname\" accepted (approval-channel isolation available)"}
}

// checkCAFile verifies rein's CA file the profile grants is present and non-empty
// (stat-only, not a content read — see defaultCAFile for why). WARN (not Fail)
// when absent: the CA is minted lazily on the first `rein run`, so a fresh
// install legitimately has none yet — doctor is read-only and must not create it.
// Fail only when a CA that IS on disk cannot be stat'd.
func checkCAFile(env Env) Check {
	const name = "rein CA"
	path, ok, err := env.CAFile()
	if err != nil {
		return Check{Name: name, Status: StatusFail, Message: fmt.Sprintf("CA at %s cannot be stat'd: %v", path, err)}
	}
	if !ok {
		return Check{Name: name, Status: StatusWarn,
			Message: fmt.Sprintf("CA not yet created (or empty) at %s; first `rein run` mints it", path)}
	}
	return Check{Name: name, Status: StatusOK, Message: path}
}

// checkLoopbackPort verifies rein can bind a 127.0.0.1 TCP port for its proxy
// listener. WARN (not Fail): a transient bind failure shouldn't hard-gate, and
// the launch path binds :0 (kernel-assigned) so a single busy port is not fatal.
func checkLoopbackPort(env Env) Check {
	const name = "loopback proxy port"
	if err := env.BindLoopback(); err != nil {
		return Check{Name: name, Status: StatusWarn, Message: fmt.Sprintf("cannot bind a 127.0.0.1 TCP port: %v", err)}
	}
	return Check{Name: name, Status: StatusOK, Message: "127.0.0.1 TCP bindable"}
}

// validateProfile writes profileJSON to a temp file and runs `nono profile
// validate` on it session-isolated. Returns an error naming the failure when
// nono reports the profile invalid.
//
// nono's exit code is authoritative ONLY when the process is waited on (a
// detached setsid returns before nono writes its verdict); RunNono uses
// CombinedOutput, which waits. We additionally treat a "Result: invalid" line in
// the output as failure — belt-and-suspenders against a future exit-code change.
func validateProfile(env Env, nonoPath string, profileJSON []byte) error {
	tmp, err := os.CreateTemp("", "rein-nono-profile-*.json")
	if err != nil {
		return fmt.Errorf("write temp profile: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(profileJSON); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp profile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write temp profile: %w", err)
	}
	out, runErr := env.RunNono(nonoPath, "profile", "validate", tmpName)
	if runErr != nil {
		return fmt.Errorf("%s: %s", runErr, strings.TrimSpace(collapse(out)))
	}
	if strings.Contains(string(out), "Result: invalid") {
		return fmt.Errorf("%s", strings.TrimSpace(collapse(out)))
	}
	return nil
}

// collapse flattens whitespace so a multi-line nono error renders on one doctor
// row.
func collapse(b []byte) string { return strings.Join(strings.Fields(string(b)), " ") }

// runNonoIsolated runs nono session-isolated: setsid detaches it from any
// controlling terminal (never touches the user's tmux), stdin is /dev/null.
// CombinedOutput waits, so the returned error carries nono's real exit code.
func runNonoIsolated(nonoPath string, args ...string) ([]byte, error) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer devnull.Close()
	cmd := exec.Command(nonoPath, args...)
	cmd.Stdin = devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.CombinedOutput()
}

// defaultProfileJSON builds the real rein profile (from the proxy host lists) so
// validate exercises the exact shape the launch path emits. The listen addr and
// CA path are placeholders — validate is a schema/JSON check, not a liveness one.
func defaultProfileJSON() ([]byte, error) {
	pr, err := Build(Params{
		ListenAddr: "127.0.0.1:47821",
		CACertPath: filepath.Join(os.TempDir(), "rein-ca.pem"),
	})
	if err != nil {
		return nil, err
	}
	return pr.MarshalIndent()
}

// defaultCAFile stat-checks rein's keystore-managed CA file WITHOUT reading it
// (a Phase 1/2 keystore backend could make Get trigger a biometric prompt;
// stat-only, like doctor's app-key check, avoids that). The file holds the
// combined cert+key PEM the profile's public CA is derived from.
func defaultCAFile() (string, bool, error) {
	configDir, err := config.ConfigDir()
	if err != nil {
		return "", false, err
	}
	ks := keystore.NewFileKeystore(configDir)
	path := ks.PathOf(proxy.CAEntryName)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, false, nil
		}
		return path, false, err
	}
	return path, info.Size() > 0, nil
}

// bindLoopbackProbe binds 127.0.0.1:0 (kernel-assigned port) and closes it,
// proving rein can host its loopback proxy listener. Explicitly 127.0.0.1,
// never 0.0.0.0, so nothing off-host is ever reachable.
func bindLoopbackProbe() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	return ln.Close()
}
