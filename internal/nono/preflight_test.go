package nono

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// fakeFileInfo is a stand-in os.FileInfo for Stat fakes (only Size is read).
type fakeFileInfo struct {
	os.FileInfo
	size int64
}

func (f fakeFileInfo) Size() int64 { return f.size }

// baseEnv returns an Env whose every op reports the healthy path. Individual
// tests override single fields to exercise one verdict at a time.
func baseEnv() Env {
	return Env{
		NonoPath:         func() (string, error) { return "/managed/nono", nil },
		Stat:             func(string) (os.FileInfo, error) { return fakeFileInfo{size: 10}, nil },
		Platform:         func() (string, error) { return "x86_64-unknown-linux-gnu", nil },
		VerifyInstalled:  func(_, _, _ string) error { return nil },
		RunNono:          func(_ string, _ ...string) ([]byte, error) { return []byte("Result: valid"), nil },
		BuildProfileJSON: func() ([]byte, error) { return []byte(`{"linux":{}}`), nil },
		CAFile:           func() (string, bool, error) { return "/cfg/proxy-ca.pem", true, nil },
		BindLoopback:     func() error { return nil },
	}
}

func find(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %v", name, checks)
	return Check{}
}

func TestPreflightAllHealthy(t *testing.T) {
	for _, c := range Preflight(baseEnv()) {
		if c.Status != StatusOK {
			t.Errorf("%s: want OK, got %v (%s)", c.Name, c.Status, c.Message)
		}
		if c.Hard() {
			t.Errorf("%s: healthy check must not be Hard()", c.Name)
		}
	}
}

func TestNonoPresent(t *testing.T) {
	tests := []struct {
		name    string
		mut     func(*Env)
		want    Status
		hard    bool
		wantMsg string
	}{
		{"absent", func(e *Env) {
			e.Stat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
		}, StatusFail, true, "not installed"},
		{"digest-mismatch", func(e *Env) {
			e.VerifyInstalled = func(_, _, _ string) error { return errors.New("digest mismatch") }
		}, StatusFail, true, "pin verification"},
		{"path-error", func(e *Env) {
			e.NonoPath = func() (string, error) { return "", errors.New("no config dir") }
		}, StatusFail, true, "resolve managed nono"},
		{"present-ok", func(e *Env) {}, StatusOK, false, "digest ok"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := baseEnv()
			tc.mut(&e)
			c := find(t, Preflight(e), "nono present")
			if c.Status != tc.want {
				t.Errorf("status: want %v, got %v (%s)", tc.want, c.Status, c.Message)
			}
			if c.Hard() != tc.hard {
				t.Errorf("Hard(): want %v, got %v", tc.hard, c.Hard())
			}
			if !strings.Contains(c.Message, tc.wantMsg) {
				t.Errorf("message %q missing %q", c.Message, tc.wantMsg)
			}
		})
	}
}

func TestProfileValidate(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Env)
		want Status
		hard bool
	}{
		{"valid", func(e *Env) {}, StatusOK, false},
		{"nono-absent-skips", func(e *Env) {
			e.Stat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
		}, StatusWarn, false}, // Warn (not Fail) on a bare box so doctor still runs.
		{"nono-rejects-exit", func(e *Env) {
			e.RunNono = func(_ string, _ ...string) ([]byte, error) {
				return []byte("Result: invalid (1 error)"), &exitErr{1}
			}
		}, StatusFail, true},
		{"nono-rejects-body", func(e *Env) {
			// exit 0 but body says invalid — belt-and-suspenders path.
			e.RunNono = func(_ string, _ ...string) ([]byte, error) {
				return []byte("Result: invalid (1 error)"), nil
			}
		}, StatusFail, true},
		{"build-fails", func(e *Env) {
			e.BuildProfileJSON = func() ([]byte, error) { return nil, errors.New("bad params") }
		}, StatusFail, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := baseEnv()
			tc.mut(&e)
			c := find(t, Preflight(e), "nono profile validate")
			if c.Status != tc.want {
				t.Errorf("status: want %v, got %v (%s)", tc.want, c.Status, c.Message)
			}
			if c.Hard() != tc.hard {
				t.Errorf("Hard(): want %v, got %v (%s)", tc.hard, c.Hard(), c.Message)
			}
		})
	}
}

func TestAfUnixMediation(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Env)
		want Status
		hard bool
	}{
		{"accepted", func(e *Env) {}, StatusOK, false},
		{"nono-absent-skips", func(e *Env) {
			e.Stat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
		}, StatusWarn, false},
		{"rejected-is-hard", func(e *Env) {
			e.RunNono = func(_ string, _ ...string) ([]byte, error) {
				return []byte("unknown variant `pathname`"), &exitErr{1}
			}
		}, StatusFail, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := baseEnv()
			tc.mut(&e)
			c := find(t, Preflight(e), "nono af_unix_mediation")
			if c.Status != tc.want {
				t.Errorf("status: want %v, got %v (%s)", tc.want, c.Status, c.Message)
			}
			if c.Hard() != tc.hard {
				t.Errorf("Hard(): want %v, got %v", tc.hard, c.Hard())
			}
		})
	}
}

func TestCAFile(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Env)
		want Status
	}{
		{"present", func(e *Env) {}, StatusOK},
		{"not-yet-created-warns", func(e *Env) {
			e.CAFile = func() (string, bool, error) { return "/cfg/proxy-ca.pem", false, nil }
		}, StatusWarn}, // lazily minted on first run; read-only doctor must not create it.
		{"unreadable-fails", func(e *Env) {
			e.CAFile = func() (string, bool, error) {
				return "/cfg/proxy-ca.pem", false, &fs.PathError{Err: errors.New("permission denied")}
			}
		}, StatusFail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := baseEnv()
			tc.mut(&e)
			c := find(t, Preflight(e), "rein CA")
			if c.Status != tc.want {
				t.Errorf("status: want %v, got %v (%s)", tc.want, c.Status, c.Message)
			}
			// CA is never a hard gate in doctor (not-yet-created is legitimate).
			if tc.want != StatusFail && c.Hard() {
				t.Errorf("non-fail CA check must not be Hard(): %s", c.Message)
			}
		})
	}
}

func TestLoopbackPort(t *testing.T) {
	e := baseEnv()
	if c := find(t, Preflight(e), "loopback proxy port"); c.Status != StatusOK {
		t.Errorf("bindable: want OK, got %v", c.Status)
	}
	e.BindLoopback = func() error { return errors.New("address in use") }
	c := find(t, Preflight(e), "loopback proxy port")
	if c.Status != StatusWarn { // Warn, not Fail: launch binds :0, a busy port isn't fatal.
		t.Errorf("unbindable: want Warn, got %v", c.Status)
	}
	if c.Hard() {
		t.Errorf("loopback check must never be Hard()")
	}
}

// TestBareBox asserts the reviewer's crux: with no nono installed, the presence
// row HARD-fails (gating a real `rein run --nono`) while the binary-dependent
// rows only WARN (so `rein doctor` still completes on a bare CI box).
func TestBareBox(t *testing.T) {
	e := baseEnv()
	e.Stat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	checks := Preflight(e)
	if c := find(t, checks, "nono present"); !c.Hard() {
		t.Errorf("bare box: `nono present` must HARD-fail, got %v", c.Status)
	}
	for _, name := range []string{"nono profile validate", "nono af_unix_mediation"} {
		if c := find(t, checks, name); c.Status != StatusWarn || c.Hard() {
			t.Errorf("bare box: %q must Warn (not Hard), got %v", name, c.Status)
		}
	}
}

// exitErr mimics *exec.ExitError for the RunNono fake (non-nil ⇒ nonzero exit).
type exitErr struct{ code int }

func (e *exitErr) Error() string { return "exit status " + itoa(e.code) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	return string(rune('0' + i))
}
