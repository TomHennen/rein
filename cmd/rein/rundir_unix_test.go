//go:build linux || darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// deadPID returns the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

var old = time.Now().Add(-2 * time.Hour)

// mkRunDir makes an unlocked run dir (backdated past the min age) whose pid
// file says pid ("" = no pid file).
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
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	return dir
}

// holdLock locks dir's pid file on a separate open file description, as a
// live run (or its inheriting child) would.
func holdLock(t *testing.T, dir string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, runDirPIDFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// Proves: an old, unlocked dir with a dead pid is removed; a locked one is
// kept even with a dead pid (the lock, not the pid, is what says "alive"); a
// young unlocked one is kept; and every narrower check still holds.
func TestSweepStaleRunDirs(t *testing.T) {
	root := t.TempDir()
	dead := strconv.Itoa(deadPID(t))
	stale := mkRunDir(t, root, "rein-run-1", 0o700, dead+"\n")
	locked := mkRunDir(t, root, "rein-run-locked", 0o700, dead)
	holdLock(t, locked)
	young := mkRunDir(t, root, "rein-run-young", 0o700, dead)
	if err := os.Chtimes(young, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	keep := map[string]string{
		"locked (dead pid)": locked,
		"younger than min":  young,
		"live pid":          mkRunDir(t, root, "rein-run-2", 0o700, strconv.Itoa(os.Getppid())),
		"own pid":           mkRunDir(t, root, "rein-run-3", 0o700, strconv.Itoa(os.Getpid())),
		"no pid file":       mkRunDir(t, root, "rein-run-4", 0o700, ""),
		"bad mode":          mkRunDir(t, root, "rein-run-5", 0o755, dead),
		"garbage pid":       mkRunDir(t, root, "rein-run-6", 0o700, "not-a-pid"),
		"other name":        mkRunDir(t, root, "rein-sandbox-7", 0o700, dead),
	}
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
	if err := os.Chtimes(pidLinkDir, old, old); err != nil {
		t.Fatal(err)
	}
	keep["symlinked pid file"] = pidLinkDir

	sweepStaleRunDirs(root, runDirSweepMinAge, time.Now())

	if exists(stale) {
		t.Error("old unlocked dead-pid run dir was not swept")
	}
	for why, dir := range keep {
		if !exists(dir) {
			t.Errorf("%s: %s was removed", why, dir)
		}
	}
	if !exists(link) {
		t.Error("symlink entry was removed")
	}
}

// Proves the run's real lock (lockRunDir) survives the parent closing its fd
// while a child that inherited it (ExtraFiles, as runWrapped passes it) is
// alive, and is released once the last holder exits.
func TestLockRunDir_HeldByInheritingChild(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "rein-run-x")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := lockRunDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sleep", "30")
	child.ExtraFiles = []*os.File{f}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	f.Close() // rein "dies"; the child still holds the lock
	writeBin(t, filepath.Join(dir, runDirPIDFile), strconv.Itoa(deadPID(t)))
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}

	sweepStaleRunDirs(root, runDirSweepMinAge, time.Now())
	if !exists(dir) {
		t.Fatal("dir swept while a child holding the lock is alive")
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	sweepStaleRunDirs(root, runDirSweepMinAge, time.Now())
	if exists(dir) {
		t.Fatal("dir not swept after the last lock holder exited")
	}
}

// Helper body for the cross-namespace test (runs only as its subprocess).
func TestSweepHelperProcess(t *testing.T) {
	root := os.Getenv("REIN_TEST_SWEEP_ROOT")
	if root == "" {
		t.Skip("helper for TestSweep_OtherPIDNamespace")
	}
	sweepStaleRunDirs(root, runDirSweepMinAge, time.Now())
}

// Proves the reviewer's repro is closed: a sweep from ANOTHER pid namespace
// sees a live owner's pid as dead, yet keeps the dir because it is locked.
// The unlocked control with the same pid IS removed there, showing the pid
// check alone would have deleted the live run's dir. Skips if unshare is
// unavailable or not permitted.
func TestSweep_OtherPIDNamespace(t *testing.T) {
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skip("unshare not available")
	}
	root := t.TempDir()
	livePID := strconv.Itoa(os.Getpid())
	locked := mkRunDir(t, root, "rein-run-live", 0o700, livePID)
	holdLock(t, locked)
	control := mkRunDir(t, root, "rein-run-control", 0o700, livePID)

	cmd := exec.Command(unshare, "-pf", "--mount-proc", os.Args[0], "-test.run=^TestSweepHelperProcess$")
	cmd.Env = append(os.Environ(), "REIN_TEST_SWEEP_ROOT="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("unshare -pf not permitted here: %v\n%s", err, out)
	}
	if !exists(locked) {
		t.Fatal("locked dir of a live run was swept from another pid namespace")
	}
	if exists(control) {
		t.Fatal("control not swept: the helper did not see the pid as dead, so this test proves nothing")
	}
}
