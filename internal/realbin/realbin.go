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

// maxShimDirs bounds how much of EnvShimDirs is honoured or kept; deeper
// nesting than this is not a real configuration.
const maxShimDirs = 64

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
	_ = os.Setenv(EnvShimDirs, AppendShimDir(visited, own))

	if override := os.Getenv(overrideEnv); override != "" {
		if _, err := os.Stat(override); err == nil {
			return override, nil
		}
		return "", fmt.Errorf("%s=%q does not exist", overrideEnv, override)
	}
	persistent, _ := config.ShimDir()
	return find(name, os.Getenv("PATH"), own, persistent, visited)
}

func find(name, path, own, persistent, visited string) (string, error) {
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
			return cand, nil
		}
	}
	return "", errors.New("no " + name + " binary found on PATH (excluding rein's shim directories)")
}

// AppendShimDir returns list with dir appended (deduplicated, at most the last
// maxShimDirs entries). Malformed entries in list are dropped.
func AppendShimDir(list, dir string) string {
	dirs := parseShimDirs(list)
	dir = filepath.Clean(dir)
	for _, d := range dirs {
		if d == dir {
			return strings.Join(dirs, string(os.PathListSeparator))
		}
	}
	dirs = append(dirs, dir)
	if len(dirs) > maxShimDirs {
		dirs = dirs[len(dirs)-maxShimDirs:]
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

// parseShimDirs keeps the last maxShimDirs absolute, sanely sized entries.
func parseShimDirs(v string) []string {
	var out []string
	for _, d := range filepath.SplitList(v) {
		if d == "" || len(d) > 4096 || !filepath.IsAbs(d) {
			continue
		}
		out = append(out, filepath.Clean(d))
	}
	if len(out) > maxShimDirs {
		out = out[len(out)-maxShimDirs:]
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
