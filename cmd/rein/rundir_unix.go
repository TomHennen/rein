//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// runDirSweepMinAge: a run dir younger than this is never swept, which also
// covers the window between MkdirTemp and lockRunDir.
const runDirSweepMinAge = time.Hour

// lockRunDir creates the run dir's pid file and takes an exclusive flock on
// it. The caller keeps the file open for the run's lifetime and passes it to
// the child, so the lock lives as long as rein OR any process inheriting it.
func lockRunDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, runDirPIDFile), os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// sweepStaleRunDirs removes direct-mode run dirs left by runs that died
// without their deferred cleanup (Ctrl-C, SIGKILL). Liveness is the pid
// file's flock: removal REQUIRES acquiring it (held while removing), on top
// of name/type/owner/mode/age checks and a dead recorded pid. A dead pid is
// never sufficient (pids are per namespace). Any doubt or error: keep.
func sweepStaleRunDirs(root string, minAge time.Duration, now time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), runDirPrefix) || !e.IsDir() {
			continue
		}
		sweepRunDir(filepath.Join(root, e.Name()), minAge, now)
	}
}

func sweepRunDir(dir string, minAge time.Duration, now time.Time) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || now.Sub(info.ModTime()) < minAge {
		return
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, runDirPIDFile), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return // EWOULDBLOCK: a live run holds it; anything else: unsure
	}
	buf := make([]byte, 32)
	n, _ := io.ReadFull(f, buf)
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil || pid <= 0 || pid == os.Getpid() || processAlive(pid) {
		return
	}
	_ = os.RemoveAll(dir)
}

// processAlive mirrors approvals.pidAlive: signal 0; EPERM means it exists.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
