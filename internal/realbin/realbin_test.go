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
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	own := filepath.Dir(exe)
	shim := writeExe(t, own, "git")
	t.Cleanup(func() { os.Remove(shim) })
	real := writeExe(t, t.TempDir(), "git")
	t.Setenv("PATH", own+string(os.PathListSeparator)+filepath.Dir(real))

	got, err := Find("git", "REIN_TEST_REAL", "git")
	if err != nil {
		t.Fatal(err)
	}
	if got != real {
		t.Fatalf("Find = %q, want %q (own dir must be skipped)", got, real)
	}
}

func TestFindOverride(t *testing.T) {
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
