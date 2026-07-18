package srt

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/TomHennen/rein/internal/sandboxutil"
)

// PinnedVersion is the srt package version CP3 is verified against. The schema
// spec and the injection model were confirmed on disk for exactly this version;
// a mismatch means the config shape or fail-open behavior may differ, so the
// preflight refuses (run) / warns (doctor) rather than trusting an unverified
// build.
//
// NOTE (schema-spec discrepancy, verified on this VM): `srt --version` reports
// "1.0.0" while the npm package is genuinely 0.0.63 (package.json + `npm ls`).
// The CLI flag is a hardcoded stub and MUST NOT be used for the pin check —
// PackageVersion reads package.json instead.
const PinnedVersion = "0.0.63"

// Status and Check moved to internal/sandboxutil (substrate-neutral: doctor's
// future nono rows need the same types without importing srt — see
// docs/design-nono-pivot.md §5/§7 "Standalone" caveat). Aliased here, not
// copied, so existing srt callers (doctor.go, run_sandboxed.go) are unaffected.
type Status = sandboxutil.Status

const (
	StatusOK   = sandboxutil.StatusOK
	StatusWarn = sandboxutil.StatusWarn
	StatusFail = sandboxutil.StatusFail
)

// Check is one preflight result: a stable name, a verdict, and a message that
// on failure names the exact fix (the loud-degrade requirement). Hard()
// (StatusFail == hard gate) is defined on sandboxutil.Check.
type Check = sandboxutil.Check

// Env injects the environment-touching operations so the pass/fail decisions
// are unit-testable with stubbed inputs. Production builds it with DefaultEnv.
type Env struct {
	// LookPath resolves a binary on PATH (exec.LookPath in production).
	LookPath func(name string) (string, error)
	// PackageVersion returns the srt npm package version given the srt binary
	// path (reads package.json in production).
	PackageVersion func(srtPath string) (string, error)
	// BwrapUserns runs the userns/AppArmor health probe and returns nil on a
	// healthy sandbox (bwrap --unshare-user --uid 0 --bind / / -- true).
	BwrapUserns func() error
	// SeccompPresent reports whether srt's vendored apply-seccomp for this arch
	// exists (the unix-socket block that stops keyring/ssh-agent reach).
	SeccompPresent func(srtPath string) (bool, error)
	// SystemCA resolves the system CA bundle path and validates it holds at
	// least one PEM certificate (systemCAProbe in production). BuildCABundle
	// enforces the same gate at launch; this surfaces it in preflight/doctor.
	SystemCA func() (string, error)
	// Now is the local wall clock (time.Now in production).
	Now func() time.Time
}

// DefaultEnv wires the production implementations.
func DefaultEnv() Env {
	return Env{
		LookPath:       exec.LookPath,
		PackageVersion: packageVersionOf,
		BwrapUserns:    bwrapUsernsProbe,
		SeccompPresent: seccompPresentFor,
		SystemCA:       systemCAProbe,
		Now:            time.Now,
	}
}

// Preflight runs every launch gate and returns the results in a stable order.
// The caller decides policy: `rein run --sandbox` fails closed on any hard
// StatusFail; `rein doctor` prints them all. srtPath (resolved once) is threaded
// so the version + seccomp checks share it.
func Preflight(env Env) []Check {
	srtPath, srtCheck := checkSrtPresent(env)
	checks := []Check{srtCheck}
	// Version + seccomp both need the srt path; skip them (as fail) if srt is
	// absent, since there's nothing to inspect.
	if srtPath == "" {
		checks = append(checks,
			Check{Name: "srt version", Status: StatusFail, Message: "skipped: srt binary not found"},
			Check{Name: "seccomp", Status: StatusFail, Message: "skipped: srt binary not found"},
		)
	} else {
		checks = append(checks, checkSrtVersion(env, srtPath), checkSeccomp(env, srtPath))
	}
	checks = append(checks, checkBwrapUserns(env), checkSystemCA(env))
	return checks
}

func checkSrtPresent(env Env) (string, Check) {
	p, err := env.LookPath("srt")
	if err != nil {
		return "", Check{Name: "srt present", Status: StatusFail, Message: "srt not on PATH; install the pinned sandbox runtime: npm i -g @anthropic-ai/sandbox-runtime@" + PinnedVersion}
	}
	return p, Check{Name: "srt present", Status: StatusOK, Message: p}
}

func checkSrtVersion(env Env, srtPath string) Check {
	v, err := env.PackageVersion(srtPath)
	if err != nil {
		return Check{Name: "srt version", Status: StatusFail,
			Message: fmt.Sprintf("cannot read srt package version (%v); reinstall: npm i -g @anthropic-ai/sandbox-runtime@%s", err, PinnedVersion)}
	}
	if v != PinnedVersion {
		return Check{Name: "srt version", Status: StatusFail,
			Message: fmt.Sprintf("srt package version is %s, pinned is %s; the config shape/fail-open behavior is only verified for %s. Reinstall: npm i -g @anthropic-ai/sandbox-runtime@%s",
				v, PinnedVersion, PinnedVersion, PinnedVersion)}
	}
	return Check{Name: "srt version", Status: StatusOK, Message: v + " (package.json; note `srt --version` misreports 1.0.0)"}
}

func checkSeccomp(env Env, srtPath string) Check {
	ok, err := env.SeccompPresent(srtPath)
	if err != nil {
		return Check{Name: "seccomp", Status: StatusFail,
			Message: fmt.Sprintf("cannot verify srt's vendored seccomp filter (%v). Without it srt runs WITHOUT the unix-socket block — the agent could reach keyring/ssh-agent sockets. Refusing to launch.", err)}
	}
	if !ok {
		return Check{Name: "seccomp", Status: StatusFail,
			Message: fmt.Sprintf("srt's vendored apply-seccomp for %s is missing. srt would WARN and run WITHOUT the unix-socket block (keyring/ssh-agent reachable). Reinstall srt; never launch a sandbox without seccomp.", seccompArch())}
	}
	return Check{Name: "seccomp", Status: StatusOK, Message: "apply-seccomp present (unix-socket block available)"}
}

func checkBwrapUserns(env Env) Check {
	if err := env.BwrapUserns(); err != nil {
		return Check{Name: "bwrap userns", Status: StatusFail,
			Message: fmt.Sprintf("bwrap could not create a user namespace (%v). On Ubuntu 23.10+ the AppArmor restriction blocks unprivileged userns; fix with:\n"+
				"    sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0\n"+
				"  (persist in /etc/sysctl.d/), or install a bwrap AppArmor profile. srt cannot sandbox without this.", err)}
	}
	return Check{Name: "bwrap userns", Status: StatusOK, Message: "unprivileged user namespace works"}
}

func checkSystemCA(env Env) Check {
	path, err := env.SystemCA()
	if err != nil {
		return Check{Name: "system CA bundle", Status: StatusFail, Message: err.Error()}
	}
	return Check{Name: "system CA bundle", Status: StatusOK, Message: path}
}

// Clock skew (#22) is NOT a dedicated srt-preflight check: rein's App-JWT mint
// path already fails closed on a skewed clock (GitHub rejects a JWT whose
// iat/exp fall outside its ±60s tolerance). That check runs in BOTH launch
// paths that matter — doctor's checkAppMint and `rein run`'s eager install-id
// App-JWT GET — so a separate NTP-style probe here would be redundant plumbing
// (and would need its own trusted network time reference). Reuse, per the plan.

// pkgMeta is the subset of srt's package.json rein reads.
type pkgMeta struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// findSrtPackage resolves the srt symlink (npm global bin is a symlink into
// lib/node_modules/.../dist) and walks up to the nearest package.json whose name
// is the sandbox-runtime package, returning that dir (the package ROOT) and its
// parsed metadata. This is ground truth for the version — unlike
// `srt --version`, which returns a hardcoded "1.0.0" on 0.0.63.
func findSrtPackage(srtPath string) (root string, meta pkgMeta, err error) {
	resolved, e := filepath.EvalSymlinks(srtPath)
	if e != nil {
		resolved = srtPath
	}
	dir := filepath.Dir(resolved)
	for i := 0; i < 8; i++ {
		if data, e := os.ReadFile(filepath.Join(dir, "package.json")); e == nil {
			var m pkgMeta
			if json.Unmarshal(data, &m) == nil && m.Name == "@anthropic-ai/sandbox-runtime" {
				return dir, m, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", pkgMeta{}, fmt.Errorf("no @anthropic-ai/sandbox-runtime package.json found near %s", srtPath)
}

// packageVersionOf returns srt's npm package version (used by the pin check).
func packageVersionOf(srtPath string) (string, error) {
	_, meta, err := findSrtPackage(srtPath)
	if err != nil {
		return "", err
	}
	return meta.Version, nil
}

// seccompArch maps GOARCH to srt's vendored seccomp dir name.
func seccompArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	default:
		return runtime.GOARCH
	}
}

// seccompPresentFor reports whether srt's vendored apply-seccomp binary for this
// arch exists under the package root.
func seccompPresentFor(srtPath string) (bool, error) {
	root, _, err := findSrtPackage(srtPath)
	if err != nil {
		return false, err
	}
	p := filepath.Join(root, "vendor", "seccomp", seccompArch(), "apply-seccomp")
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// bwrapUsernsProbe runs the Ubuntu-24.04 userns gate: a trivial bwrap that needs
// an unprivileged user namespace. Exit 0 => healthy. The AppArmor restriction
// on 23.10+ makes this fail with a distinctive namespace error.
func bwrapUsernsProbe() error {
	cmd := exec.Command("bwrap", "--unshare-user", "--uid", "0", "--bind", "/", "/", "--", "true")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, string(out))
	}
	return nil
}
