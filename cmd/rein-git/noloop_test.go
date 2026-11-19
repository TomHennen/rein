package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// buildShim compiles this package once per test into dir/name.
func buildShim(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name)
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", out, ".")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

func script(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func runShim(t *testing.T, shim string, env []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shim, "--version")
	cmd.Args[0] = "git" // as a PATH lookup would invoke it
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("shim did not terminate (exec loop?): %s", out)
	}
	if err != nil {
		t.Fatalf("shim: %v\n%s", err, out)
	}
	return string(out)
}

// A per-run shim (rein run --direct) with the persistent shim dir ALSO on PATH
// must go straight to the real git: no hop through the persistent copy (which
// may be a stale older rein-git) and no run<->persistent exec loop.
func TestShimSkipsPersistentShimDir(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the shim")
	}
	root := t.TempDir()
	state := filepath.Join(root, "state")
	persistent := filepath.Join(state, "rein", "shim")
	runDir := filepath.Join(root, "run")
	realDir := filepath.Join(root, "real")
	marker := filepath.Join(root, "persistent-hop")

	shim := buildShim(t, runDir, "git")
	script(t, filepath.Join(persistent, "git"), "touch "+marker+"\nexit 99\n")
	script(t, filepath.Join(realDir, "git"), "echo REAL-GIT \"$@\"\n")

	sep := string(os.PathListSeparator)
	env := []string{
		"XDG_STATE_HOME=" + state,
		"HOME=" + root,
		"PATH=" + runDir + sep + persistent + sep + realDir + sep + "/usr/bin:/bin",
	}
	out := runShim(t, shim, env)
	if !strings.Contains(out, "REAL-GIT --version") {
		t.Fatalf("output = %q, want the real git", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("per-run shim hopped through the persistent shim dir")
	}
}

// Even with the persistent shim (current code) FIRST on PATH, ahead of the
// per-run dir, the chain terminates at the real git.
func TestShimPersistentFirstTerminates(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the shim")
	}
	root := t.TempDir()
	state := filepath.Join(root, "state")
	persistent := filepath.Join(state, "rein", "shim")
	runDir := filepath.Join(root, "run")
	realDir := filepath.Join(root, "real")

	shim := buildShim(t, persistent, "git")
	buildShim(t, runDir, "git")
	script(t, filepath.Join(realDir, "git"), "echo REAL-GIT \"$@\"\n")

	sep := string(os.PathListSeparator)
	env := []string{
		"XDG_STATE_HOME=" + state,
		"HOME=" + root,
		"PATH=" + persistent + sep + runDir + sep + realDir + sep + "/usr/bin:/bin",
	}
	if out := runShim(t, shim, env); !strings.Contains(out, "REAL-GIT --version") {
		t.Fatalf("output = %q, want the real git", out)
	}
}
