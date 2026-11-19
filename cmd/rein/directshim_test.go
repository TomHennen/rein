package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeBin(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeInstall lays out rein, rein-git, rein-gh side by side and returns rein.
func fakeInstall(t *testing.T, tag string) string {
	t.Helper()
	bin := t.TempDir()
	for _, n := range shimSources {
		writeBin(t, filepath.Join(bin, n), n+"-"+tag)
	}
	return filepath.Join(bin, "rein")
}

func assertLayout(t *testing.T, dir, tag string) {
	t.Helper()
	for name, want := range map[string]string{"rein": "rein-" + tag, "rein-git": "rein-git-" + tag, "rein-gh": "rein-gh-" + tag, "git": "rein-git-" + tag, "gh": "rein-gh-" + tag} {
		p := filepath.Join(dir, name)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != want {
			t.Errorf("%s = %q, want %q", name, b, want)
		}
	}
	for _, name := range shimSources {
		info, _ := os.Lstat(filepath.Join(dir, name))
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v, want regular 0700", name, info.Mode())
		}
	}
	for _, l := range shimLinks {
		if target, err := os.Readlink(filepath.Join(dir, l.name)); err != nil || target != l.target {
			t.Errorf("%s -> %q (%v), want relative link to %s", l.name, target, err, l.target)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != len(shimSources)+len(shimLinks) {
		t.Errorf("dir has %d entries (temp files left behind?)", len(entries))
	}
}

func TestLocateAndPlaceShims(t *testing.T) {
	self := fakeInstall(t, "v2")
	srcs, err := locateShimSources(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := placeShims(srcs, dir); err != nil {
		t.Fatal(err)
	}
	assertLayout(t, dir, "v2")
}

// Any dir rein stages or installs is itself a valid source: a rein run from
// the persistent shim dir (manual PATH activation), or a nested run via the
// per-run dir's rein.
func TestShimDirIsItselfASource(t *testing.T) {
	self := fakeInstall(t, "v2")
	srcs, _ := locateShimSources(self)
	shimDir := t.TempDir()
	if err := placeShims(srcs, shimDir); err != nil {
		t.Fatal(err)
	}
	srcs2, err := locateShimSources(filepath.Join(shimDir, "rein"))
	if err != nil {
		t.Fatalf("rein in a shim dir must find its siblings: %v", err)
	}
	nested := t.TempDir()
	if err := placeShims(srcs2, nested); err != nil {
		t.Fatal(err)
	}
	assertLayout(t, nested, "v2")
}

// Re-installing over the old layout (git/gh regular copies, no rein-git)
// upgrades it, and re-installing from the shim dir onto itself is safe.
func TestPlaceShimsOverOldLayoutAndOntoItself(t *testing.T) {
	shimDir := t.TempDir()
	for _, n := range []string{"git", "gh", "rein"} {
		writeBin(t, filepath.Join(shimDir, n), "old")
	}
	self := fakeInstall(t, "v2")
	srcs, _ := locateShimSources(self)
	if err := placeShims(srcs, shimDir); err != nil {
		t.Fatal(err)
	}
	assertLayout(t, shimDir, "v2")
	srcs2, err := locateShimSources(filepath.Join(shimDir, "rein"))
	if err != nil {
		t.Fatal(err)
	}
	if err := placeShims(srcs2, shimDir); err != nil {
		t.Fatal(err)
	}
	assertLayout(t, shimDir, "v2")
}

func TestLocateShimSources_MissingSiblingFails(t *testing.T) {
	for _, missing := range []string{"rein-git", "rein-gh"} {
		self := fakeInstall(t, "v1")
		os.Remove(filepath.Join(filepath.Dir(self), missing))
		_, err := locateShimSources(self)
		if err == nil || !strings.Contains(err.Error(), missing+" binary not found next to") || !strings.Contains(err.Error(), "side by side") {
			t.Errorf("missing %s: err = %v", missing, err)
		}
	}
}

// A persistent shim dir from an older rein (git/gh copies, no rein-git) names
// the cause and the fix.
func TestLocateShimSources_OldShimDirIsActionable(t *testing.T) {
	old := t.TempDir()
	for _, n := range []string{"git", "gh", "rein"} {
		writeBin(t, filepath.Join(old, n), "old")
	}
	_, err := locateShimSources(filepath.Join(old, "rein"))
	if err == nil || !strings.Contains(err.Error(), "older rein") || !strings.Contains(err.Error(), "rein install-shim") {
		t.Fatalf("err = %v", err)
	}
}

// No PATH fallback: a rein-git elsewhere on PATH must not stand in for the
// sibling (it could be another version).
func TestLocateShimSources_IgnoresPATH(t *testing.T) {
	self := fakeInstall(t, "v1")
	os.Remove(filepath.Join(filepath.Dir(self), "rein-git"))
	other := t.TempDir()
	writeBin(t, filepath.Join(other, "rein-git"), "other")
	t.Setenv("PATH", other)
	if _, err := locateShimSources(self); err == nil {
		t.Fatal("rein-git on PATH must not satisfy the sibling lookup")
	}
}

func TestPlaceShims_FailureAborts(t *testing.T) {
	self := fakeInstall(t, "v1")
	srcs, err := locateShimSources(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := placeShims(srcs, filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("staging into a missing dir must fail")
	}
	delete(srcs, "rein-gh")
	if err := placeShims(srcs, t.TempDir()); err == nil {
		t.Fatal("an unlocated source must fail")
	}
}

// probeShims with real executables: the staged rein is a script that accepts
// --help, and git/gh are links to scripts that exec $REIN_REAL_GIT/$REIN_REAL_GH
// the way the real shims do.
func probeLayout(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	writeBin(t, filepath.Join(bin, "rein"), "#!/bin/sh\n[ \"$1\" = --help ] || exit 9\n")
	writeBin(t, filepath.Join(bin, "rein-git"), "#!/bin/sh\nexec \"$REIN_REAL_GIT\" \"$@\"\n")
	writeBin(t, filepath.Join(bin, "rein-gh"), "#!/bin/sh\nexec \"$REIN_REAL_GH\" \"$@\"\n")
	srcs, err := locateShimSources(filepath.Join(bin, "rein"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := placeShims(srcs, dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestProbeShims_OK(t *testing.T) {
	if err := probeShims(probeLayout(t)); err != nil {
		t.Fatal(err)
	}
}

// Simulates a noexec temp mount by clearing the exec bits (EACCES on exec,
// which is also what a noexec mount returns). It proves the probe refuses
// when a staged binary cannot be exec'd; it does NOT exercise a real noexec
// mount (the offline smoke does, where mounting is permitted).
func TestProbeShims_NonExecutableFailsClosed(t *testing.T) {
	for _, name := range shimSources {
		dir := probeLayout(t)
		if err := os.Chmod(filepath.Join(dir, name), 0o600); err != nil {
			t.Fatal(err)
		}
		err := probeShims(dir)
		if err == nil || !strings.Contains(err.Error(), "cannot run") || !strings.Contains(err.Error(), "TMPDIR") || !strings.Contains(err.Error(), dir) {
			t.Errorf("%s non-executable: err = %v", name, err)
		}
	}
}

// The running test binary has no rein-git/rein-gh siblings, so runWrapped must
// refuse with 127 before launching anything.
func TestRunWrapped_MissingSiblingFailsClosed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	marker := filepath.Join(t.TempDir(), "ran")
	code, err := runWrapped([]string{"--", "touch", marker})
	if code != 127 || err == nil || !strings.Contains(err.Error(), "binary not found next to") {
		t.Fatalf("runWrapped = %d, %v; want 127 + not-found", code, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("child launched despite missing shim source")
	}
}

// deadPID returns the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func mkRunDir(t *testing.T, root, name string, mode os.FileMode, pid string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if pid != "" {
		writeBin(t, filepath.Join(dir, runDirPIDFile), pid)
	}
	writeBin(t, filepath.Join(dir, "rein"), "x")
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSweepStaleRunDirs(t *testing.T) {
	root := t.TempDir()
	dead := strconv.Itoa(deadPID(t))
	live := strconv.Itoa(os.Getppid())
	stale := mkRunDir(t, root, "rein-run-1", 0o700, dead+"\n")
	keep := map[string]string{
		"live pid":    mkRunDir(t, root, "rein-run-2", 0o700, live),
		"own pid":     mkRunDir(t, root, "rein-run-3", 0o700, strconv.Itoa(os.Getpid())),
		"no pid file": mkRunDir(t, root, "rein-run-4", 0o700, ""),
		"bad mode":    mkRunDir(t, root, "rein-run-5", 0o755, dead),
		"garbage pid": mkRunDir(t, root, "rein-run-6", 0o700, "not-a-pid"),
		"other name":  mkRunDir(t, root, "rein-sandbox-7", 0o700, dead),
	}
	// A symlinked run dir and a symlinked pid file are never followed.
	target := mkRunDir(t, t.TempDir(), "rein-run-target", 0o700, dead)
	link := filepath.Join(root, "rein-run-8")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	keep["symlinked dir target"] = target
	pidLinkDir := mkRunDir(t, root, "rein-run-9", 0o700, "")
	deadFile := filepath.Join(t.TempDir(), "pid")
	writeBin(t, deadFile, dead)
	if err := os.Symlink(deadFile, filepath.Join(pidLinkDir, runDirPIDFile)); err != nil {
		t.Fatal(err)
	}
	keep["symlinked pid file"] = pidLinkDir

	sweepStaleRunDirs(root)

	if _, err := os.Lstat(stale); err == nil {
		t.Error("dead-pid run dir was not swept")
	}
	for why, dir := range keep {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s: %s was removed", why, dir)
		}
	}
	if _, err := os.Lstat(link); err != nil {
		t.Error("symlink entry was removed")
	}
}

func installPersistent(t *testing.T, self string) string {
	t.Helper()
	srcs, err := locateShimSources(self)
	if err != nil {
		t.Fatal(err)
	}
	shimDir := filepath.Join(t.TempDir(), "shim")
	if err := os.Mkdir(shimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := placeShims(srcs, shimDir); err != nil {
		t.Fatal(err)
	}
	return shimDir
}

func setAllMtimes(t *testing.T, when time.Time, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		for _, n := range shimSources {
			if err := os.Chtimes(filepath.Join(d, n), when, when); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Nix: every store path has the epoch mtime. Differing content must still be
// reported stale (the old mtime comparison said "up to date" here).
func TestShimFreshness_EqualMtimesDifferentContent(t *testing.T) {
	self := fakeInstall(t, "v2")
	shimDir := installPersistent(t, self)
	writeBin(t, filepath.Join(shimDir, "rein-git"), "rein-git-v1")
	setAllMtimes(t, time.Unix(1, 0), filepath.Dir(self), shimDir)

	r := shimFreshness(shimDir, self)
	if r.status != statusWarn || !strings.Contains(r.message, "[rein-git]") || !strings.Contains(r.message, "`rein run` stages fresh copies") {
		t.Fatalf("got %+v, want stale warn naming rein-git", r)
	}
}

// Same size, different bytes: still stale (the size short-circuit is not the
// whole comparison).
func TestShimFreshness_SameSizeDifferentContent(t *testing.T) {
	self := fakeInstall(t, "v2")
	shimDir := installPersistent(t, self)
	writeBin(t, filepath.Join(shimDir, "rein"), "rein-v3")
	if r := shimFreshness(shimDir, self); r.status != statusWarn || !strings.Contains(r.message, "[rein]") {
		t.Fatalf("got %+v", r)
	}
}

// Identical content with the shim OLDER than its source is up to date.
func TestShimFreshness_SameContentDifferentMtimes(t *testing.T) {
	self := fakeInstall(t, "v2")
	shimDir := installPersistent(t, self)
	setAllMtimes(t, time.Unix(1, 0), shimDir)

	r := shimFreshness(shimDir, self)
	if r.status != statusOK || r.message != "3 shims under "+shimDir+" up to date" {
		t.Fatalf("got %+v, want ok", r)
	}
}

func TestShimFreshness_OldLayoutWarns(t *testing.T) {
	self := fakeInstall(t, "v2")
	shimDir := filepath.Join(t.TempDir(), "shim")
	for _, n := range []string{"git", "gh"} {
		writeBin(t, filepath.Join(shimDir, n), n)
	}
	writeBin(t, filepath.Join(shimDir, "rein"), "rein-v2")
	r := shimFreshness(shimDir, self)
	if r.status != statusWarn || !strings.Contains(r.message, "older layout") || !strings.Contains(r.message, "rein install-shim") {
		t.Fatalf("got %+v", r)
	}
}

func TestShimFreshness_BrokenLinkIsStale(t *testing.T) {
	self := fakeInstall(t, "v2")
	shimDir := installPersistent(t, self)
	os.Remove(filepath.Join(shimDir, "gh"))
	writeBin(t, filepath.Join(shimDir, "gh"), "rein-gh-v2")
	if r := shimFreshness(shimDir, self); r.status != statusWarn || !strings.Contains(r.message, "[gh]") {
		t.Fatalf("got %+v", r)
	}
}

func TestShimFreshness_MissingShimWarns(t *testing.T) {
	self := fakeInstall(t, "v2")
	shimDir := installPersistent(t, self)
	os.Remove(filepath.Join(shimDir, "rein"))
	r := shimFreshness(shimDir, self)
	if r.status != statusWarn || !strings.Contains(r.message, "missing") || !strings.Contains(r.message, "rein install-shim") {
		t.Fatalf("got %+v, want missing warn", r)
	}
	if rem, ok := remediationFor(r); !ok || rem.tier != remedyNoPriv {
		t.Fatalf("missing shim must stay remediable by --fix")
	}
}

func TestShimFreshness_NoSourceWarns(t *testing.T) {
	self := fakeInstall(t, "v2")
	shimDir := installPersistent(t, self)
	os.Remove(filepath.Join(filepath.Dir(self), "rein-gh"))
	r := shimFreshness(shimDir, self)
	if r.status != statusWarn || !strings.Contains(r.message, "can't verify freshness") {
		t.Fatalf("got %+v, want no-source warn", r)
	}
}
