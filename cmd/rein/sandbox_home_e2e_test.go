package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/srt"
)

// TestWorkTreeUnderAllowBack_E2E is the live regression guard for the #63
// launch-blocking bug: a working tree that lives UNDER a read-only allow-back.
//
// srt re-binds allow-backs READ-ONLY on top of the $HOME deny tmpfs, and emits
// those ro-binds AFTER the writable binds. It skips an allowRead only when a
// write path COVERS it — never when the allowRead is an ANCESTOR of one. So an
// allow-back containing the working tree ro-bound straight over it: the tree
// went read-only in-sandbox, bwrap could not create the .gitconfig bind target
// inside it, and the launch ABORTED with
//
//	bwrap: Can't create file at <worktree>/.gitconfig: Read-only file system
//
// This is a common layout, not an exotic one — `~/go` is an allow-back (the Go
// module cache) and `~/go/src/<pkg>` is the classic GOPATH checkout.
//
// The bug was invisible to the entire existing suite because every test in this
// repo runs from a work tree OUTSIDE $HOME, where no allow-back can possibly be
// an ancestor. So this test does the one thing that catches it: it puts the work
// tree inside the allow-back, in the REAL home directory, and launches REAL srt.
//
// It drives the OPERATOR path (REIN_SANDBOX_ALLOW_READ), which both exercises
// resolveAllowBacks end-to-end and answers the same question for operator-
// supplied entries: an operator who allow-reads an ancestor of their own work
// tree must not brick their run either.
//
// Run: REIN_SANDBOX_E2E=1 go test ./cmd/rein/ -run E2E -v
func TestWorkTreeUnderAllowBack_E2E(t *testing.T) {
	if os.Getenv("REIN_SANDBOX_E2E") == "" {
		t.Skip("set REIN_SANDBOX_E2E=1 to run the live srt work-tree-under-allow-back test")
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

	// The ~/go-shaped allow-back, in the REAL home dir (that is the point).
	//
	//	<ancestor>/            <- the allow-back (like ~/go)
	//	  data/readme          <- read-back that must SURVIVE the punch-out (like ~/go/pkg/mod)
	//	  work/                <- the WORKING TREE, must be WRITABLE (like ~/go/src/demo)
	ancestor, err := os.MkdirTemp(home, ".rein-e2e-allowback-*")
	if err != nil {
		t.Fatalf("create allow-back dir in home: %v", err)
	}
	defer os.RemoveAll(ancestor)
	data := filepath.Join(ancestor, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "readme"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	workTree := filepath.Join(ancestor, "work")
	if err := os.MkdirAll(workTree, 0o755); err != nil {
		t.Fatal(err)
	}

	// The real derivation, through the operator allow-read entry.
	homeDeny, allowReads, showHome, err := deriveHomeDenial("", ancestor, home, srtPath, nil, []string{workTree})
	if err != nil {
		t.Fatalf("deriveHomeDenial: %v", err)
	}
	if showHome || homeDeny == "" {
		t.Fatalf("expected the home deny to be active; showHome=%v homeDeny=%q", showHome, homeDeny)
	}
	// The regression itself: the ancestor must never be emitted whole.
	for _, p := range allowReads {
		if p == ancestor {
			t.Fatalf("the allow-back %q was emitted whole — it contains the working tree, so srt "+
				"will ro-bind over it and the launch will abort (the #63 bug)", ancestor)
		}
		if pathAtOrUnder(workTree, p) {
			t.Fatalf("allow-back %q is an ancestor of the working tree %q — same bug", p, workTree)
		}
	}

	launch := func(name string, allow []string, wantWritable bool) {
		t.Helper()
		cfg, err := srt.Build(srt.Params{
			SocketPath:   filepath.Join(t.TempDir(), "proxy.sock"),
			WorkingTree:  workTree,
			DenyReadHome: homeDeny,
			AllowRead:    allow,
		})
		if err != nil {
			t.Fatalf("%s: srt.Build: %v", name, err)
		}
		blob, err := cfg.MarshalIndent()
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		settings := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(settings, blob, 0o600); err != nil {
			t.Fatal(err)
		}
		// Write into the work tree, and read the sibling the punch-out preserved.
		script := "echo hi > " + filepath.Join(workTree, "probe") + " || exit 30; " +
			"[ \"$(cat " + filepath.Join(data, "readme") + ")\" = cached ] || exit 31; exit 0"
		cmd := exec.Command(srtPath, "-s", settings, "--", "/bin/sh", "-c", script)
		cmd.Env = srt.BuildEnv(srt.EnvParams{
			Parent:       os.Environ(),
			CABundlePath: filepath.Join(t.TempDir(), "unused.pem"),
			StubGHToken:  "stub",
		})
		out, runErr := cmd.CombinedOutput()
		_ = os.Remove(filepath.Join(workTree, "probe"))

		if wantWritable {
			if runErr != nil {
				t.Errorf("%s: the sandbox could not write to the working tree (or launch at all): %v\noutput: %s\n"+
					"This is the #63 bug: an allow-back ro-bound over the working tree.", name, runErr, strings.TrimSpace(string(out)))
			} else {
				t.Logf("%s: work tree WRITABLE in-sandbox, and the punched-out sibling is still READABLE", name)
			}
			return
		}
		// Negative control: prove this test can actually SEE the bug. Emitting the
		// ancestor whole must break the run — if it does not, srt's semantics
		// changed and the punch-out may no longer be load-bearing.
		if runErr == nil {
			t.Errorf("%s: emitting the ancestor allow-back WHOLE was expected to break the run "+
				"(read-only work tree / failed launch), but it succeeded. srt's bind ordering may have "+
				"changed — re-verify whether resolveAllowBacks is still needed.", name)
			return
		}
		// It must break for the RIGHT reason — a read-only work tree — not for some
		// unrelated reason that would make this control vacuous.
		ev := evidenceLine(string(out))
		if !strings.Contains(ev, "Read-only file system") {
			t.Errorf("%s: the run broke, but not with the expected read-only work tree. This control is "+
				"only meaningful if it reproduces the #63 failure. err=%v output: %s", name, runErr, strings.TrimSpace(string(out)))
			return
		}
		t.Logf("%s: (negative control) reproduces the #63 failure exactly: %s", name, ev)
	}

	launch("punched-out (the fix)", allowReads, true)

	// Negative control = the SAME allow-back set, plus the ancestor re-added WHOLE
	// (i.e. what the derivation produced before the fix). It must keep srt's own
	// install-chain allow-backs: dropping those hides srt's vendored apply-seccomp
	// under the home tmpfs and the sandbox dies with exit 127 before the child ever
	// runs — a DIFFERENT failure that would make this control vacuous.
	unpunched := append(append([]string{}, allowReads...), ancestor)
	launch("ancestor emitted whole (the bug)", unpunched, false)
}

// evidenceLine picks the line that actually explains the failure (bwrap's, or a
// read-only complaint), not the first line — srt prints benign warnings first.
func evidenceLine(out string) string {
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "Read-only file system") || strings.Contains(ln, "bwrap:") {
			return strings.TrimSpace(ln)
		}
	}
	return strings.TrimSpace(out)
}
