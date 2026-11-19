package realbin

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeExe(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFindSkipsPersistentShimDir(t *testing.T) {
	t.Setenv(EnvShimDirs, "")
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("REIN_TEST_REAL", "")
	persistent := filepath.Join(state, "rein", "shim")
	writeExe(t, persistent, "git")
	real := writeExe(t, t.TempDir(), "git")
	t.Setenv("PATH", persistent+string(os.PathListSeparator)+filepath.Dir(real))

	got, err := Find("git", "REIN_TEST_REAL", "git")
	if err != nil {
		t.Fatal(err)
	}
	if got != real {
		t.Fatalf("Find = %q, want %q (persistent shim dir must be skipped)", got, real)
	}
}

func TestFindSkipsSymlinkToPersistentShimDir(t *testing.T) {
	t.Setenv(EnvShimDirs, "")
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	persistent := filepath.Join(state, "rein", "shim")
	writeExe(t, persistent, "gh")
	link := filepath.Join(t.TempDir(), "shimlink")
	if err := os.Symlink(persistent, link); err != nil {
		t.Fatal(err)
	}
	real := writeExe(t, t.TempDir(), "gh")
	t.Setenv("PATH", link+string(os.PathListSeparator)+filepath.Dir(real))

	got, err := Find("gh", "REIN_TEST_REAL", "gh")
	if err != nil {
		t.Fatal(err)
	}
	if got != real {
		t.Fatalf("Find = %q, want %q", got, real)
	}
}

func TestFindSkipsOwnDir(t *testing.T) {
	t.Setenv(EnvShimDirs, "")
	own := t.TempDir()
	writeExe(t, own, "git")
	real := writeExe(t, t.TempDir(), "git")
	got, err := find("git", own+string(os.PathListSeparator)+filepath.Dir(real), Canonical(own), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != real {
		t.Fatalf("find = %q, want %q (own dir must be skipped)", got, real)
	}
}

// Two per-run dirs (nested runs) plus the persistent one: every dir listed in
// EnvShimDirs is skipped.
func TestFindSkipsVisitedShimDirs(t *testing.T) {
	t.Setenv(EnvShimDirs, "")
	sep := string(os.PathListSeparator)
	inner, outer, persistent := t.TempDir(), t.TempDir(), t.TempDir()
	for _, d := range []string{inner, outer, persistent} {
		writeExe(t, d, "git")
	}
	real := writeExe(t, t.TempDir(), "git")
	path := inner + sep + outer + sep + persistent + sep + filepath.Dir(real)
	got, err := find("git", path, Canonical(outer), persistent, Canonical(inner), "")
	if err != nil || got != real {
		t.Fatalf("find = %q, %v; want %q", got, err, real)
	}
}

func TestFindRecordsOwnDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(EnvShimDirs, "/already/here")
	real := writeExe(t, t.TempDir(), "git")
	t.Setenv("PATH", filepath.Dir(real))
	if _, err := Find("git", "REIN_TEST_REAL", "git"); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	want := "/already/here" + string(os.PathListSeparator) + Canonical(filepath.Dir(exe))
	if got := os.Getenv(EnvShimDirs); got != want {
		t.Fatalf("%s = %q, want %q", EnvShimDirs, got, want)
	}
}

func TestAppendShimDir(t *testing.T) {
	sep := string(os.PathListSeparator)
	if got, err := AppendShimDir("/a"+sep+"/b", "/a"); err != nil || got != "/a"+sep+"/b" {
		t.Errorf("dedupe: %q, %v", got, err)
	}
	if got, err := AppendShimDir("relative"+sep+sep+"/x/../y", "/z"); err != nil || got != "/y"+sep+"/z" {
		t.Errorf("malformed entries: %q, %v", got, err)
	}
}

// Overflow is an error, never a truncation (which would re-open the loop).
func TestAppendShimDir_CountBound(t *testing.T) {
	sep := string(os.PathListSeparator)
	var dirs []string
	for i := 0; i < maxShimDirs-1; i++ {
		dirs = append(dirs, "/d/"+strconv.Itoa(i))
	}
	full, err := AppendShimDir(strings.Join(dirs, sep), "/last")
	if err != nil || len(strings.Split(full, sep)) != maxShimDirs {
		t.Fatalf("at the bound: %v", err)
	}
	if _, err := AppendShimDir(full, "/d/0"); err != nil {
		t.Errorf("re-adding a present dir at the bound must not fail: %v", err)
	}
	if _, err := AppendShimDir(full, "/one-more"); !errors.Is(err, ErrTooManyShimDirs) {
		t.Errorf("past the bound: err = %v", err)
	}
}

func TestAppendShimDir_LengthBound(t *testing.T) {
	sep := string(os.PathListSeparator)
	long := "/" + strings.Repeat("x", 4000)
	var dirs []string
	for i := 0; i < 9; i++ {
		dirs = append(dirs, long+strconv.Itoa(i))
	}
	if _, err := AppendShimDir(strings.Join(dirs, sep), long+"new"); !errors.Is(err, ErrTooManyShimDirs) {
		t.Errorf("length overflow: err = %v", err)
	}
	if _, err := AppendShimDir(strings.Repeat("garbage", 10000), "/a"); !errors.Is(err, ErrTooManyShimDirs) {
		t.Errorf("huge malformed value: err = %v", err)
	}
}

func TestFind_FullShimDirsFailsClosed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var dirs []string
	for i := 0; i < maxShimDirs; i++ {
		dirs = append(dirs, "/d/"+strconv.Itoa(i))
	}
	t.Setenv(EnvShimDirs, strings.Join(dirs, string(os.PathListSeparator)))
	real := writeExe(t, t.TempDir(), "git")
	t.Setenv("PATH", filepath.Dir(real))
	if _, err := Find("git", "REIN_TEST_REAL", "git"); err == nil || !strings.Contains(err.Error(), "too many nested rein shim dirs") {
		t.Fatalf("err = %v", err)
	}
}

// A symlink to the shim in an unlisted dir is not mistaken for the real git.
func TestFind_SkipsCandidateThatIsSelf(t *testing.T) {
	shimDir := t.TempDir()
	self := writeExe(t, shimDir, "rein-git")
	other := t.TempDir()
	if err := os.Symlink(self, filepath.Join(other, "git")); err != nil {
		t.Fatal(err)
	}
	real := writeExe(t, t.TempDir(), "git")
	path := other + string(os.PathListSeparator) + filepath.Dir(real)
	got, err := find("git", path, Canonical(shimDir), "", "", Canonical(self))
	if err != nil || got != real {
		t.Fatalf("find = %q, %v; want %q", got, err, real)
	}
}

func TestFindOverride(t *testing.T) {
	t.Setenv(EnvShimDirs, "")
	p := writeExe(t, t.TempDir(), "git")
	t.Setenv("REIN_TEST_REAL", p)
	t.Setenv("PATH", "")
	if got, err := Find("git", "REIN_TEST_REAL", "git"); err != nil || got != p {
		t.Fatalf("Find = %q, %v; want %q", got, err, p)
	}
	t.Setenv("REIN_TEST_REAL", filepath.Join(t.TempDir(), "nope"))
	if _, err := Find("git", "REIN_TEST_REAL", "git"); err == nil {
		t.Fatal("missing override must error")
	}
}

func TestFindNoneFound(t *testing.T) {
	t.Setenv(EnvShimDirs, "")
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	persistent := filepath.Join(state, "rein", "shim")
	writeExe(t, persistent, "git")
	t.Setenv("PATH", persistent)
	_, err := Find("git", "REIN_TEST_REAL", "git")
	if err == nil || !strings.Contains(err.Error(), "no git binary") {
		t.Fatalf("err = %v, want no-git error", err)
	}
}
