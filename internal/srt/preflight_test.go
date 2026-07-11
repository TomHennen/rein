package srt

import (
	"errors"
	"testing"
	"time"
)

// stubEnv builds an Env whose checks all pass by default; individual tests
// override single fields to exercise the failure decisions.
func stubEnv() Env {
	return Env{
		LookPath:       func(string) (string, error) { return "/usr/bin/srt", nil },
		PackageVersion: func(string) (string, error) { return PinnedVersion, nil },
		BwrapUserns:    func() error { return nil },
		SeccompPresent: func(string) (bool, error) { return true, nil },
		SystemCA:       func() (string, error) { return "/etc/ssl/certs/ca-certificates.crt", nil },
		Now:            time.Now,
	}
}

func statusOf(checks []Check, name string) Status {
	for _, c := range checks {
		if c.Name == name {
			return c.Status
		}
	}
	return StatusFail // absent == worst
}

func anyHardFail(checks []Check) bool {
	for _, c := range checks {
		if c.Status == StatusFail {
			return true
		}
	}
	return false
}

func TestPreflightAllGreen(t *testing.T) {
	checks := Preflight(stubEnv())
	if anyHardFail(checks) {
		t.Errorf("all-good env produced a hard fail: %+v", checks)
	}
}

func TestPreflightSrtMissing(t *testing.T) {
	e := stubEnv()
	e.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	checks := Preflight(e)
	if statusOf(checks, "srt present") != StatusFail {
		t.Error("missing srt should fail")
	}
	if !anyHardFail(checks) {
		t.Error("missing srt should hard-fail the launch")
	}
}

func TestPreflightVersionMismatch(t *testing.T) {
	e := stubEnv()
	e.PackageVersion = func(string) (string, error) { return "0.0.62", nil }
	if statusOf(Preflight(e), "srt version") != StatusFail {
		t.Error("version mismatch should fail")
	}
}

func TestPreflightVersionUnreadable(t *testing.T) {
	e := stubEnv()
	e.PackageVersion = func(string) (string, error) { return "", errors.New("no package.json") }
	if statusOf(Preflight(e), "srt version") != StatusFail {
		t.Error("unreadable version should fail")
	}
}

func TestPreflightSeccompMissingFailsClosed(t *testing.T) {
	e := stubEnv()
	e.SeccompPresent = func(string) (bool, error) { return false, nil }
	checks := Preflight(e)
	if statusOf(checks, "seccomp") != StatusFail {
		t.Error("missing seccomp MUST hard-fail (the unix-socket block guarantee)")
	}
	if !anyHardFail(checks) {
		t.Error("missing seccomp must block the launch")
	}
}

func TestPreflightSystemCABroken(t *testing.T) {
	e := stubEnv()
	e.SystemCA = func() (string, error) { return "", errors.New("no PEM certificates") }
	checks := Preflight(e)
	if statusOf(checks, "system CA bundle") != StatusFail {
		t.Error("broken system trust store should fail")
	}
	if !anyHardFail(checks) {
		t.Error("broken system trust store must block the launch (SSL_CERT_FILE replaces roots in-sandbox)")
	}
}

func TestPreflightBwrapUnhealthy(t *testing.T) {
	e := stubEnv()
	e.BwrapUserns = func() error { return errors.New("userns disabled by AppArmor") }
	if statusOf(Preflight(e), "bwrap userns") != StatusFail {
		t.Error("broken userns should fail")
	}
}
