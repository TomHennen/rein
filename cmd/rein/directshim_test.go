package main

import (
	"os"
	"path/filepath"
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
	for _, n := range []string{"rein", "rein-git", "rein-gh"} {
		writeBin(t, filepath.Join(bin, n), n+"-"+tag)
	}
	return filepath.Join(bin, "rein")
}

func TestLocateAndStageDirectShims(t *testing.T) {
	self := fakeInstall(t, "v2")
	srcs, err := locateDirectShims(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := stageDirectShims(srcs, dir); err != nil {
		t.Fatal(err)
	}
	for dst, want := range map[string]string{"rein": "rein-v2", "git": "rein-git-v2", "gh": "rein-gh-v2"} {
		p := filepath.Join(dir, dst)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != want {
			t.Errorf("%s = %q, want %q", dst, b, want)
		}
		info, _ := os.Stat(p)
		if info.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v, want 0700", dst, info.Mode().Perm())
		}
	}
}

func TestLocateDirectShims_MissingSiblingFails(t *testing.T) {
	for _, missing := range []string{"rein-git", "rein-gh"} {
		self := fakeInstall(t, "v1")
		os.Remove(filepath.Join(filepath.Dir(self), missing))
		_, err := locateDirectShims(self)
		if err == nil || !strings.Contains(err.Error(), missing+" binary not found next to") {
			t.Errorf("missing %s: err = %v", missing, err)
		}
	}
}

// No PATH fallback: a rein-git elsewhere on PATH must not stand in for the
// sibling (it could be another version).
func TestLocateDirectShims_IgnoresPATH(t *testing.T) {
	self := fakeInstall(t, "v1")
	os.Remove(filepath.Join(filepath.Dir(self), "rein-git"))
	other := t.TempDir()
	writeBin(t, filepath.Join(other, "rein-git"), "other")
	t.Setenv("PATH", other)
	if _, err := locateDirectShims(self); err == nil {
		t.Fatal("rein-git on PATH must not satisfy the sibling lookup")
	}
}

func TestStageDirectShims_FailureAborts(t *testing.T) {
	self := fakeInstall(t, "v1")
	srcs, err := locateDirectShims(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := stageDirectShims(srcs, filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("staging into a missing dir must fail")
	}
	delete(srcs, "rein-gh")
	if err := stageDirectShims(srcs, t.TempDir()); err == nil {
		t.Fatal("an unlocated source must fail")
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

func installPersistent(t *testing.T, self string, content map[string]string) string {
	t.Helper()
	shimDir := filepath.Join(t.TempDir(), "shim")
	for _, s := range directShims {
		src := self
		if s.src != "rein" {
			src = filepath.Join(filepath.Dir(self), s.src)
		}
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if c, ok := content[s.dst]; ok {
			b = []byte(c)
		}
		writeBin(t, filepath.Join(shimDir, s.dst), string(b))
	}
	return shimDir
}

func setAllMtimes(t *testing.T, when time.Time, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
}

// Nix: every store path has the epoch mtime. Differing content must still be
// reported stale (mtime comparison said "up to date" here).
func TestShimFreshness_EqualMtimesDifferentContent(t *testing.T) {
	self := fakeInstall(t, "v2")
	shimDir := installPersistent(t, self, map[string]string{"git": "rein-git-v1"})
	epoch := time.Unix(1, 0)
	setAllMtimes(t, epoch, self, filepath.Join(filepath.Dir(self), "rein-git"), filepath.Join(filepath.Dir(self), "rein-gh"),
		filepath.Join(shimDir, "rein"), filepath.Join(shimDir, "git"), filepath.Join(shimDir, "gh"))

	r := shimFreshness(shimDir, self)
	if r.status != statusWarn || !strings.Contains(r.message, "[git]") || !strings.Contains(r.message, "`rein run` stages fresh copies") {
		t.Fatalf("got %+v, want stale warn naming git", r)
	}
}

// Identical content with the shim OLDER than its source is up to date.
func TestShimFreshness_SameContentDifferentMtimes(t *testing.T) {
	self := fakeInstall(t, "v2")
	shimDir := installPersistent(t, self, nil)
	setAllMtimes(t, time.Unix(1, 0), filepath.Join(shimDir, "rein"), filepath.Join(shimDir, "git"), filepath.Join(shimDir, "gh"))

	r := shimFreshness(shimDir, self)
	if r.status != statusOK || r.message != "3 shims under "+shimDir+" up to date" {
		t.Fatalf("got %+v, want ok", r)
	}
}

func TestShimFreshness_MissingShimWarns(t *testing.T) {
	self := fakeInstall(t, "v2")
	shimDir := installPersistent(t, self, nil)
	os.Remove(filepath.Join(shimDir, "gh"))
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
	shimDir := installPersistent(t, self, nil)
	os.Remove(filepath.Join(filepath.Dir(self), "rein-gh"))
	r := shimFreshness(shimDir, self)
	if r.status != statusWarn || !strings.Contains(r.message, "can't verify freshness") {
		t.Fatalf("got %+v, want no-source warn", r)
	}
}
