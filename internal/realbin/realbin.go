// Package realbin finds the real git/gh binary a rein shim wraps.
package realbin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TomHennen/rein/internal/config"
)

// EnvShimDirs lists the shim dirs already traversed, PATH-list separated.
// Each shim appends its own dir before exec'ing on, so the set survives exec
// hops and git's own git subprocesses. It is a LOOP GUARD, not a security
// boundary: whoever can set it can equally call the real binary directly.
const EnvShimDirs = "REIN_SHIM_DIRS"

// Bounds on EnvShimDirs. Exceeding either is an error, never a truncation:
// dropping visited dirs would re-open the loop. The length bound keeps the
// variable far below the kernel's per-string env limit (E2BIG on exec).
const (
	maxShimDirs    = 64
	maxShimDirsLen = 32 << 10
)

// ErrTooManyShimDirs means EnvShimDirs is at its bound.
var ErrTooManyShimDirs = errors.New("too many nested rein shim dirs (" + EnvShimDirs + " is full); refusing to continue rather than risk an exec loop")

// Find returns the first executable `name` on PATH that is not in a rein shim
// dir: the calling shim's own dir, the persistent shim dir (config.ShimDir),
// or any dir in EnvShimDirs. A non-empty os.Getenv(overrideEnv) wins over the
// scan. As a side effect it records the shim's own dir in EnvShimDirs in this
// process's environment, so whatever it execs next inherits it.
//
// The own dir comes from os.Executable(), falling back to argv0: invoked as
// bare `git`, argv0 alone would resolve to the CWD.
func Find(name, overrideEnv, argv0 string) (string, error) {
	self := argv0
	if exe, err := os.Executable(); err == nil {
		self = exe
	} else if abs, err := filepath.Abs(argv0); err == nil {
		self = abs
	}
	own := Canonical(filepath.Dir(self))
	visited := os.Getenv(EnvShimDirs)
	updated, err := AppendShimDir(visited, own)
	if err != nil {
		return "", err
	}
	_ = os.Setenv(EnvShimDirs, updated)

	if override := os.Getenv(overrideEnv); override != "" {
		if _, err := os.Stat(override); err == nil {
			return override, nil
		}
		return "", fmt.Errorf("%s=%q does not exist", overrideEnv, override)
	}
	persistent, _ := config.ShimDir()
	return find(name, os.Getenv("PATH"), own, persistent, visited, Canonical(self))
}

// find skips shim dirs and, as a backstop, any candidate that resolves to
// selfExe itself (e.g. a symlink to the shim from an unlisted dir).
func find(name, path, own, persistent, visited, selfExe string) (string, error) {
	skip := map[string]bool{own: true}
	if persistent != "" {
		skip[Canonical(persistent)] = true
	}
	for _, d := range parseShimDirs(visited) {
		skip[d] = true
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || skip[Canonical(dir)] {
			continue
		}
		cand := filepath.Join(dir, name)
		if info, err := os.Stat(cand); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			if selfExe != "" && Canonical(cand) == selfExe {
				continue
			}
			return cand, nil
		}
	}
	return "", errors.New("no " + name + " binary found on PATH (excluding rein's shim directories)")
}

// AppendShimDir returns list with dir appended (deduplicated; malformed
// entries dropped), or ErrTooManyShimDirs if that would exceed the bounds.
func AppendShimDir(list, dir string) (string, error) {
	if len(list) > maxShimDirsLen {
		return "", ErrTooManyShimDirs
	}
	dirs := parseShimDirs(list)
	dir = filepath.Clean(dir)
	present := false
	for _, d := range dirs {
		present = present || d == dir
	}
	if !present {
		dirs = append(dirs, dir)
	}
	out := strings.Join(dirs, string(os.PathListSeparator))
	if len(dirs) > maxShimDirs || len(out) > maxShimDirsLen {
		return "", ErrTooManyShimDirs
	}
	return out, nil
}

// parseShimDirs keeps the absolute, sanely sized entries of v.
func parseShimDirs(v string) []string {
	var out []string
	for _, d := range filepath.SplitList(v) {
		if d == "" || len(d) > 4096 || !filepath.IsAbs(d) {
			continue
		}
		out = append(out, filepath.Clean(d))
	}
	return out
}

// Canonical is the absolute, symlink-resolved form of dir (best-effort).
func Canonical(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return dir
}
