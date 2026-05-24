package broker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

// stubMint returns a MintFunc that ignores ctx and returns the configured
// token and error.
func stubMint(token string, err error) MintFunc {
	return func(ctx context.Context) (string, time.Time, error) {
		if err != nil {
			return "", time.Time{}, err
		}
		return token, time.Now().Add(time.Hour), nil
	}
}

func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

// TestRunCredentialHelper exercises the protocol surface with a stubbed
// minter so we can assert on stdout, exit behavior, and — most importantly —
// the TM-G8 invariant: github.com gets always produce a non-empty credential.
func TestRunCredentialHelper(t *testing.T) {
	tests := []struct {
		name             string
		action           string
		stdin            string
		mintToken        string
		mintErr          error
		wantStdoutHasPwd bool   // must contain "password=..."
		wantPasswordExact string // exact password value, if set
	}{
		{
			name:              "github.com get with successful mint returns real token",
			action:            "get",
			stdin:             "protocol=https\nhost=github.com\n\n",
			mintToken:         "ghs_real_token_value",
			wantStdoutHasPwd:  true,
			wantPasswordExact: "ghs_real_token_value",
		},
		{
			name:              "TM-G8: github.com get with mint failure still returns placeholder",
			action:            "get",
			stdin:             "protocol=https\nhost=github.com\n\n",
			mintErr:           errors.New("simulated mint failure"),
			wantStdoutHasPwd:  true,
			wantPasswordExact: "rein-placeholder-mint-failed",
		},
		{
			name:             "non-github.com host returns empty",
			action:           "get",
			stdin:            "protocol=https\nhost=gitlab.com\n\n",
			mintToken:        "should-not-be-used",
			wantStdoutHasPwd: false,
		},
		{
			name:             "ssh protocol returns empty (Bearer token wouldn't help)",
			action:           "get",
			stdin:            "protocol=ssh\nhost=github.com\n\n",
			mintToken:        "should-not-be-used",
			wantStdoutHasPwd: false,
		},
		{
			name:              "url= attribute is parsed into protocol/host",
			action:            "get",
			stdin:             "url=https://github.com/owner/repo\n\n",
			mintToken:         "ghs_url_form_token",
			wantStdoutHasPwd:  true,
			wantPasswordExact: "ghs_url_form_token",
		},
		{
			name:             "store action is a no-op (no stdout)",
			action:           "store",
			stdin:            "protocol=https\nhost=github.com\nusername=x\npassword=y\n\n",
			mintToken:        "should-not-be-used",
			wantStdoutHasPwd: false,
		},
		{
			name:             "erase action is a no-op (no stdout)",
			action:           "erase",
			stdin:            "protocol=https\nhost=github.com\n\n",
			mintToken:        "should-not-be-used",
			wantStdoutHasPwd: false,
		},
		{
			name:             "unknown action is a no-op",
			action:           "watusi",
			stdin:            "protocol=https\nhost=github.com\n\n",
			mintToken:        "should-not-be-used",
			wantStdoutHasPwd: false,
		},
		{
			// TM-G8 hardening: a single malformed stdin line must not
			// prevent the github.com guard from running. If parseAttrs
			// aborted on the bad line, host/protocol would be unknown and
			// we'd silently return empty for a github.com request — the
			// exact behavior that triggers `gh auth setup-git`.
			name:              "TM-G8: malformed line is skipped, github.com guard still runs",
			action:            "get",
			stdin:             "garbage-line-no-equals\nprotocol=https\nhost=github.com\n\n",
			mintToken:         "ghs_after_malformed",
			wantStdoutHasPwd:  true,
			wantPasswordExact: "ghs_after_malformed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			cfg := Config{
				Mint:        stubMint(tc.mintToken, tc.mintErr),
				MintTimeout: 5 * time.Second,
				Logger:      discardLogger(),
			}
			err := RunCredentialHelper(tc.action, strings.NewReader(tc.stdin), &stdout, cfg)
			if err != nil {
				t.Fatalf("RunCredentialHelper returned error: %v (expected nil for well-formed input)", err)
			}
			got := stdout.String()
			hasPwd := strings.Contains(got, "password=")
			if hasPwd != tc.wantStdoutHasPwd {
				t.Fatalf("stdout pwd presence = %v, want %v; stdout = %q", hasPwd, tc.wantStdoutHasPwd, got)
			}
			if tc.wantPasswordExact != "" {
				wantLine := "password=" + tc.wantPasswordExact
				if !strings.Contains(got, wantLine) {
					t.Fatalf("stdout missing %q; got %q", wantLine, got)
				}
			}
		})
	}
}

// TestParseAttrsURL confirms the url= backfill matches what git sends in the
// modern protocol (gitcredentials(7)).
func TestParseAttrsURL(t *testing.T) {
	in := "url=https://github.com/TomHennen/agentcreds-validation-a.git\n\n"
	attrs, err := parseAttrs(strings.NewReader(in), discardLogger())
	if err != nil {
		t.Fatalf("parseAttrs error: %v", err)
	}
	if attrs["protocol"] != "https" {
		t.Errorf("protocol = %q, want %q", attrs["protocol"], "https")
	}
	if attrs["host"] != "github.com" {
		t.Errorf("host = %q, want %q", attrs["host"], "github.com")
	}
	if attrs["path"] != "TomHennen/agentcreds-validation-a.git" {
		t.Errorf("path = %q, want %q", attrs["path"], "TomHennen/agentcreds-validation-a.git")
	}
}

// TestRunCredentialHelperRequiresConfig confirms missing config returns
// a programming-error, not a silent no-op.
func TestRunCredentialHelperRequiresConfig(t *testing.T) {
	t.Run("missing logger", func(t *testing.T) {
		var stdout bytes.Buffer
		err := RunCredentialHelper("get", strings.NewReader(""), &stdout, Config{
			Mint: stubMint("x", nil),
		})
		if err == nil {
			t.Fatal("expected error for missing Logger")
		}
	})
	t.Run("missing mint", func(t *testing.T) {
		var stdout bytes.Buffer
		err := RunCredentialHelper("get", strings.NewReader(""), &stdout, Config{
			Logger: discardLogger(),
		})
		if err == nil {
			t.Fatal("expected error for missing Mint")
		}
	})
}
