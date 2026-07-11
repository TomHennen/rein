package main

import (
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/proxy"
)

func TestParseDeclareArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantN    int
		wantRepo string
		wantErr  string
	}{
		{"plain number", []string{"73"}, 73, "", ""},
		{"with repo flag", []string{"73", "--repo", "o/r"}, 73, "o/r", ""},
		{"repo flag equals form", []string{"73", "--repo=o/r"}, 73, "o/r", ""},
		{"repo flag before number", []string{"--repo", "o/r", "73"}, 73, "o/r", ""},
		{"no args", nil, 0, "", "usage"},
		{"leading zero rejected", []string{"073"}, 0, "", "not a valid issue number"},
		{"zero rejected", []string{"0"}, 0, "", "not a valid issue number"},
		{"negative rejected", []string{"-7"}, 0, "", "unknown flag"},
		{"non-numeric rejected", []string{"seventy"}, 0, "", "not a valid issue number"},
		{"too long rejected", []string{"12345678901"}, 0, "", "not a valid issue number"},
		{"extra arg rejected", []string{"73", "74"}, 0, "", "unexpected argument"},
		{"dangling repo flag", []string{"73", "--repo"}, 0, "", "usage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, repo, err := parseDeclareArgs(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if n != tc.wantN || repo != tc.wantRepo {
				t.Errorf("got (%d, %q), want (%d, %q)", n, repo, tc.wantN, tc.wantRepo)
			}
		})
	}
}

// TestDeclareHostURLMatchesProxyConstant pins the client-side URL to the
// proxy's virtual-host constant so the two can't drift.
func TestDeclareHostURLMatchesProxyConstant(t *testing.T) {
	want := "https://" + proxy.DeclareHost + "/v1/declare"
	if declareHostURL != want {
		t.Errorf("declareHostURL = %q, want %q (must match proxy.DeclareHost)", declareHostURL, want)
	}
}
