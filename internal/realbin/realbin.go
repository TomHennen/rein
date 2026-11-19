// Package realbin finds the real git/gh binary a rein shim wraps.
package realbin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/TomHennen/rein/internal/config"
)

// Find returns the first executable `name` on PATH, skipping the calling
// shim's own directory AND the persistent shim dir (config.ShimDir). Skipping
// both is what stops a per-run shim and a persistent shim (possibly a stale
// copy from an older rein) from exec'ing each other in a loop when both are on
// PATH. A non-empty os.Getenv(overrideEnv) wins over the scan.
//
// The own dir comes from os.Executable(), falling back to argv0: invoked as
// bare `git`, argv0 alone would resolve to the CWD.
func Find(name, overrideEnv, argv0 string) (string, error) {
	if override := os.Getenv(overrideEnv); override != "" {
		if _, err := os.Stat(override); err == nil {
			return override, nil
		}
		return "", fmt.Errorf("%s=%q does not exist", overrideEnv, override)
	}

	self := argv0
	if exe, err := os.Executable(); err == nil {
		self = exe
	} else if abs, err := filepath.Abs(argv0); err == nil {
		self = abs
	}
	skip := map[string]bool{canonical(filepath.Dir(self)): true}
	if d, err := config.ShimDir(); err == nil {
		skip[canonical(d)] = true
	}
	return scan(name, os.Getenv("PATH"), skip)
}

func scan(name, path string, skip map[string]bool) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || skip[canonical(dir)] {
			continue
		}
		cand := filepath.Join(dir, name)
		if info, err := os.Stat(cand); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return cand, nil
		}
	}
	return "", errors.New("no " + name + " binary found on PATH (excluding rein's shim directories)")
}

// canonical is the absolute, symlink-resolved form of dir (best-effort).
func canonical(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return dir
}
