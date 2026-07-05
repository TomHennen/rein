package srt

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
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

// Status is a preflight verdict, matching doctor's three-value framing.
type Status int

const (
	StatusOK Status = iota
	StatusWarn
	StatusFail
)

// Check is one preflight result: a stable name, a verdict, and a message that
// on failure names the exact fix (the loud-degrade requirement).
type Check struct {
	Name    string
	Status  Status
	Message string
}

// Hard reports whether this check is a hard gate for launching a sandboxed run.
// A StatusFail on a hard check must fail the launch closed; StatusWarn never
// blocks. srt/bwrap/seccomp are hard; clock skew and version-mismatch severity
// depend on the check's own verdict.
func (c Check) Hard() bool { return c.Status == StatusFail }

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
			Check{"srt version", StatusFail, "skipped: srt binary not found"},
			Check{"seccomp", StatusFail, "skipped: srt binary not found"},
		)
	} else {
		checks = append(checks, checkSrtVersion(env, srtPath), checkSeccomp(env, srtPath))
	}
	checks = append(checks, checkBwrapUserns(env))
	return checks
}

func checkSrtPresent(env Env) (string, Check) {
	p, err := env.LookPath("srt")
	if err != nil {
		return "", Check{"srt present", StatusFail,
			"srt not on PATH; install the pinned sandbox runtime: npm i -g @anthropic-ai/sandbox-runtime@" + PinnedVersion}
	}
	return p, Check{"srt present", StatusOK, p}
}

func checkSrtVersion(env Env, srtPath string) Check {
	v, err := env.PackageVersion(srtPath)
	if err != nil {
		return Check{"srt version", StatusFail,
			fmt.Sprintf("cannot read srt package version (%v); reinstall: npm i -g @anthropic-ai/sandbox-runtime@%s", err, PinnedVersion)}
	}
	if v != PinnedVersion {
		return Check{"srt version", StatusFail,
			fmt.Sprintf("srt package version is %s, pinned is %s; the config shape/fail-open behavior is only verified for %s. Reinstall: npm i -g @anthropic-ai/sandbox-runtime@%s",
				v, PinnedVersion, PinnedVersion, PinnedVersion)}
	}
	return Check{"srt version", StatusOK, v + " (package.json; note `srt --version` misreports 1.0.0)"}
}

func checkSeccomp(env Env, srtPath string) Check {
	ok, err := env.SeccompPresent(srtPath)
	if err != nil {
		return Check{"seccomp", StatusFail,
			fmt.Sprintf("cannot verify srt's vendored seccomp filter (%v). Without it srt runs WITHOUT the unix-socket block — the agent could reach keyring/ssh-agent sockets. Refusing to launch.", err)}
	}
	if !ok {
		return Check{"seccomp", StatusFail,
			fmt.Sprintf("srt's vendored apply-seccomp for %s is missing. srt would WARN and run WITHOUT the unix-socket block (keyring/ssh-agent reachable). Reinstall srt; never launch a sandbox without seccomp.", seccompArch())}
	}
	return Check{"seccomp", StatusOK, "apply-seccomp present (unix-socket block available)"}
}

func checkBwrapUserns(env Env) Check {
	if err := env.BwrapUserns(); err != nil {
		return Check{"bwrap userns", StatusFail,
			fmt.Sprintf("bwrap could not create a user namespace (%v). On Ubuntu 23.10+ the AppArmor restriction blocks unprivileged userns; fix with:\n"+
				"    sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0\n"+
				"  (persist in /etc/sysctl.d/), or install a bwrap AppArmor profile. srt cannot sandbox without this.", err)}
	}
	return Check{"bwrap userns", StatusOK, "unprivileged user namespace works"}
}

// Clock skew (#22) is NOT a dedicated srt-preflight check: rein's App-JWT mint
// path already fails closed on a skewed clock (GitHub rejects a JWT whose
// iat/exp fall outside its ±60s tolerance). That check runs in BOTH launch
// paths that matter — doctor's checkAppMint and `rein run`'s eager install-id
// App-JWT GET — so a separate NTP-style probe here would be redundant plumbing
// (and would need its own trusted network time reference). Reuse, per the plan.

// packageVersionOf reads the version from the package.json at the root of srt's
// npm package. It resolves the srt symlink (npm global bin is a symlink into
// lib/node_modules/.../dist) and walks up to the nearest package.json whose
// name is the sandbox-runtime package. This is ground truth — unlike
// `srt --version`, which returns a hardcoded "1.0.0" on 0.0.63.
func packageVersionOf(srtPath string) (string, error) {
	resolved, err := filepath.EvalSymlinks(srtPath)
	if err != nil {
		resolved = srtPath
	}
	dir := filepath.Dir(resolved)
	for i := 0; i < 8; i++ {
		pj := filepath.Join(dir, "package.json")
		if data, err := os.ReadFile(pj); err == nil {
			var meta struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			}
			if json.Unmarshal(data, &meta) == nil && meta.Name == "@anthropic-ai/sandbox-runtime" {
				return meta.Version, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no @anthropic-ai/sandbox-runtime package.json found near %s", srtPath)
}

// packageRootOf returns the srt package root dir (the dir containing the
// sandbox-runtime package.json), used to locate the vendored seccomp filter.
func packageRootOf(srtPath string) (string, error) {
	resolved, err := filepath.EvalSymlinks(srtPath)
	if err != nil {
		resolved = srtPath
	}
	dir := filepath.Dir(resolved)
	for i := 0; i < 8; i++ {
		pj := filepath.Join(dir, "package.json")
		if data, err := os.ReadFile(pj); err == nil {
			var meta struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(data, &meta) == nil && meta.Name == "@anthropic-ai/sandbox-runtime" {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no @anthropic-ai/sandbox-runtime package root found near %s", srtPath)
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
	root, err := packageRootOf(srtPath)
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
