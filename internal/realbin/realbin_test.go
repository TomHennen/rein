package realbin

import (
	"os"
	"path/filepath"
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
	got, err := find("git", own+string(os.PathListSeparator)+filepath.Dir(real), Canonical(own), "", "")
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
	got, err := find("git", path, Canonical(outer), persistent, Canonical(inner))
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
	if got := AppendShimDir("/a"+sep+"/b", "/a"); got != "/a"+sep+"/b" {
		t.Errorf("dedupe: %q", got)
	}
	if got := AppendShimDir("relative"+sep+sep+"/x/../y", "/z"); got != "/y"+sep+"/z" {
		t.Errorf("malformed entries: %q", got)
	}
	var many []string
	for i := 0; i < 1000; i++ {
		many = append(many, "/d"+strings.Repeat("x", i%50))
	}
	many = append(many, "/"+strings.Repeat("y", 10000))
	got := strings.Split(AppendShimDir(strings.Join(many, sep), "/new"), sep)
	if len(got) > maxShimDirs || got[len(got)-1] != "/new" {
		t.Errorf("cap: %d entries, last %q", len(got), got[len(got)-1])
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
