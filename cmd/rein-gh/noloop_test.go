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

// Nested runs put two per-run dirs (inner, outer) plus the persistent dir on
// PATH; each holds rein-gh with gh -> rein-gh. Whatever the order, the chain
// must end at the real gh. REIN_GH_SHIM_ACTIVE takes the re-entry path so no
// token is minted; the lookup under test is the same.
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
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", filepath.Join(d, "rein-gh"), ".")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, b)
		}
		if err := os.Symlink("rein-gh", filepath.Join(d, "gh")); err != nil {
			t.Fatal(err)
		}
	}
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "gh"), []byte("#!/bin/sh\necho REAL-GH \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

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
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, filepath.Join(dirs[order[0]], "gh"), "--version")
		cmd.Args[0] = "gh"
		cmd.Env = []string{
			"XDG_STATE_HOME=" + state,
			"HOME=" + root,
			"REIN_GH_SHIM_ACTIVE=1",
			"PATH=" + strings.Join(path, string(os.PathListSeparator)),
		}
		out, err := cmd.CombinedOutput()
		timedOut := ctx.Err() != nil
		cancel()
		if timedOut {
			t.Fatalf("order %v: shim did not terminate (exec loop?)", order)
		}
		if err != nil || !strings.Contains(string(out), "REAL-GH --version") {
			t.Fatalf("order %v: %v, output = %q, want the real gh", order, err, out)
		}
	}
}
