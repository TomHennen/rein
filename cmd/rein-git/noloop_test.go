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

// buildShim compiles this package to dir/rein-git with dir/git -> rein-git,
// the layout `rein run` and `rein install-shim` produce.
func buildShim(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "rein-git")
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", out, ".")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	if err := os.Symlink("rein-git", filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "git")
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

// A per-run shim with the persistent shim dir also on PATH goes straight to
// the real git: no hop through the persistent copy (possibly a stale older
// rein-git).
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

	shim := buildShim(t, runDir)
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

// Nested runs put two per-run dirs (inner, outer) plus the persistent dir on
// PATH. Whatever the order, the chain must end at the real git: each hop
// records its dir in REIN_SHIM_DIRS, so inner and outer cannot ping-pong.
func TestShimThreeShimDirsTerminate(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the shim")
	}
	root := t.TempDir()
	state := filepath.Join(root, "state")
	dirs := map[string]string{
		"inner":      filepath.Join(root, "inner"),
		"outer":      filepath.Join(root, "outer"),
		"persistent": filepath.Join(state, "rein", "shim"),
	}
	for _, d := range dirs {
		buildShim(t, d)
	}
	realDir := filepath.Join(root, "real")
	script(t, filepath.Join(realDir, "git"), "echo REAL-GIT \"$@\"\n")

	for _, order := range [][]string{
		{"inner", "outer", "persistent"},
		{"outer", "inner", "persistent"},
		{"persistent", "inner", "outer"},
	} {
		var path []string
		for _, name := range order {
			path = append(path, dirs[name])
		}
		path = append(path, realDir, "/usr/bin", "/bin")
		env := []string{
			"XDG_STATE_HOME=" + state,
			"HOME=" + root,
			"PATH=" + strings.Join(path, string(os.PathListSeparator)),
		}
		if out := runShim(t, filepath.Join(dirs[order[0]], "git"), env); !strings.Contains(out, "REAL-GIT --version") {
			t.Fatalf("order %v: output = %q, want the real git", order, out)
		}
	}
}
