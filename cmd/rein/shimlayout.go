package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A shim dir (per-run or persistent) holds the three binaries under their
// SOURCE names, plus git/gh as relative symlinks to the shims. Keeping the
// source names means a rein run from a shim dir finds its siblings there.
var (
	shimSources = []string{"rein", "rein-git", "rein-gh"}
	shimLinks   = []struct{ name, target string }{{"git", "rein-git"}, {"gh", "rein-gh"}}
)

// locateShimSources resolves rein-git and rein-gh as siblings of self (the
// resolved running binary; "rein" is self itself). Siblings only, no PATH
// fallback: a rein-git found elsewhere may be a different version.
func locateShimSources(self string) (map[string]string, error) {
	dir := filepath.Dir(self)
	srcs := map[string]string{"rein": self}
	for _, name := range shimSources[1:] {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			srcs[name] = p
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "git")); err == nil {
			return nil, fmt.Errorf("%s binary not found next to %s, which looks like a shim dir from an older rein; re-run `rein install-shim` from the rein you installed, or run that rein directly", name, self)
		}
		return nil, fmt.Errorf("%s binary not found next to %s; rein, rein-git and rein-gh must be installed side by side (go build -o bin/ ./...)", name, self)
	}
	return srcs, nil
}

// placeShims lays srcs out in dir (copies, mode 0700) and points git/gh at
// them. Each entry is written to a temp name and renamed into place, so a
// re-install never truncates a binary that is running or is its own source.
func placeShims(srcs map[string]string, dir string) error {
	removeStaleShimTemps(dir)
	for _, name := range shimSources {
		src, ok := srcs[name]
		if !ok {
			return fmt.Errorf("place %s: source not located", name)
		}
		if err := atomicCopy(src, filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("place %s: %w", name, err)
		}
	}
	for _, l := range shimLinks {
		if err := atomicSymlink(l.target, filepath.Join(dir, l.name)); err != nil {
			return fmt.Errorf("place %s: %w", l.name, err)
		}
	}
	return nil
}

// removeStaleShimTemps clears temp entries an interrupted placeShims left
// behind (only our own ".<name>.tmp-*" names, never directories).
func removeStaleShimTemps(dir string) {
	names := append([]string{}, shimSources...)
	for _, l := range shimLinks {
		names = append(names, l.name)
	}
	for _, n := range names {
		matches, _ := filepath.Glob(filepath.Join(dir, "."+n+".tmp-*"))
		for _, m := range matches {
			if info, err := os.Lstat(m); err == nil && !info.IsDir() {
				_ = os.Remove(m)
			}
		}
	}
}

func atomicCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o700); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func atomicSymlink(target, dst string) error {
	tmp := filepath.Join(filepath.Dir(dst), fmt.Sprintf(".%s.tmp-%d", filepath.Base(dst), time.Now().UnixNano()))
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// runDirPrefix / runDirPIDFile identify a direct-mode run dir for the sweep.
const (
	runDirPrefix  = "rein-run-"
	runDirPIDFile = "rein-run.pid"
)

// probeShims execs each staged PATH name (rein, git, gh) once, harmlessly:
// rein --help, and the git/gh shims pointed (via their REIN_REAL_* override)
// at the staged rein --help. A noexec temp mount would otherwise make the
// shell silently skip the shims and run the REAL git/gh.
func probeShims(dir string) error {
	rein := filepath.Join(dir, "rein")
	probes := []struct {
		name string
		env  []string
	}{
		{"rein", nil},
		{"git", []string{"REIN_REAL_GIT=" + rein}},
		{"gh", []string{"REIN_REAL_GH=" + rein, "REIN_GH_SHIM_ACTIVE=1"}},
	}
	for _, p := range probes {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cmd := exec.CommandContext(ctx, filepath.Join(dir, p.name), "--help")
		cmd.Env = append(os.Environ(), p.env...)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			detail := err.Error()
			if msg := strings.TrimSpace(string(out)); msg != "" {
				detail += ": " + msg[:min(len(msg), 300)]
			}
			return fmt.Errorf("staged %s in %s cannot run: %s (e.g. a noexec mount; point TMPDIR at an exec-capable directory and retry)",
				p.name, dir, detail)
		}
	}
	return nil
}
