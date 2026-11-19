package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	tmp := fmt.Sprintf("%s.tmp-%d", dst, time.Now().UnixNano())
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

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
			return fmt.Errorf("staged %s in %s cannot run (%s); is that directory on a noexec mount? point TMPDIR at an exec-capable directory and retry",
				p.name, dir, detail)
		}
	}
	return nil
}

// runDirPrefix / runDirPIDFile identify a direct-mode run dir for the sweep.
const (
	runDirPrefix  = "rein-run-"
	runDirPIDFile = "rein-run.pid"
)

func writeRunDirPID(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, runDirPIDFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(f, "%d\n", os.Getpid())
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// sweepStaleRunDirs removes direct-mode run dirs left by runs that died
// without their deferred cleanup (Ctrl-C, SIGKILL). Deliberately narrow: only
// real directories (never symlinks) directly in root named rein-run-*, owned
// by this uid, mode exactly 0700, holding a regular pid file whose pid is not
// alive. Anything else, or any error, is left alone.
func sweepStaleRunDirs(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), runDirPrefix) || !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if staleRunDir(dir) {
			_ = os.RemoveAll(dir)
		}
	}
}

func staleRunDir(dir string) bool {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return false
	}
	f, err := os.OpenFile(filepath.Join(dir, runDirPIDFile), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	buf := make([]byte, 32)
	n, _ := io.ReadFull(f, buf)
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return false
	}
	return !processAlive(pid)
}

// processAlive mirrors approvals.pidAlive: signal 0; EPERM means it exists.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
