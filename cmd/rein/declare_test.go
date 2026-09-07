package main

import (
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/proxy"
)

func TestParseDeclareArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    declareArgs
		wantErr string
	}{
		{"plain number", []string{"73"}, declareArgs{number: 73}, ""},
		{"with repo flag", []string{"73", "--repo", "o/r"}, declareArgs{number: 73, repo: "o/r"}, ""},
		{"repo flag equals form", []string{"73", "--repo=o/r"}, declareArgs{number: 73, repo: "o/r"}, ""},
		{"repo flag before number", []string{"--repo", "o/r", "73"}, declareArgs{number: 73, repo: "o/r"}, ""},
		{"no args", nil, declareArgs{}, "usage"},
		{"leading zero rejected", []string{"073"}, declareArgs{}, "not a valid issue number"},
		{"zero rejected", []string{"0"}, declareArgs{}, "not a valid issue number"},
		{"negative rejected", []string{"-7"}, declareArgs{}, "unknown flag"},
		{"non-numeric rejected", []string{"seventy"}, declareArgs{}, "not a valid issue number"},
		{"too long rejected", []string{"12345678901"}, declareArgs{}, "not a valid issue number"},
		{"extra arg rejected", []string{"73", "74"}, declareArgs{}, "unexpected argument"},
		{"dangling repo flag", []string{"73", "--repo"}, declareArgs{}, "usage"},

		// --new (issue #180)
		{"new with title", []string{"--new", "Add a thing"},
			declareArgs{isNew: true, title: "Add a thing"}, ""},
		{"new equals form", []string{"--new=Add a thing"},
			declareArgs{isNew: true, title: "Add a thing"}, ""},
		{"new with body and repo", []string{"--new", "Add a thing", "--body", "why", "--repo", "o/r"},
			declareArgs{isNew: true, title: "Add a thing", body: "why", repo: "o/r"}, ""},
		{"new normalizes whitespace", []string{"--new", "  Add   a  thing  "},
			declareArgs{isNew: true, title: "Add a thing"}, ""},
		{"new keeps body newlines", []string{"--new", "Title", "--body", "one\ntwo"},
			declareArgs{isNew: true, title: "Title", body: "one\ntwo"}, ""},
		{"new rejects a number too", []string{"--new", "Title", "73"}, declareArgs{}, "takes no issue number"},
		{"new rejects an empty title", []string{"--new", "   "}, declareArgs{}, "must not be empty"},
		{"new rejects a wordless title", []string{"--new", "--- ???"}, declareArgs{}, "approval token"},
		// The first word IS the token the human types, so it must be strong
		// enough to be a deliberate act — and never a bare number, which
		// collides with the `rein declare <n>` token.
		{"new rejects a one-letter first word", []string{"--new", "a broken thing"}, declareArgs{}, "approval token"},
		{"new rejects a numeric first word", []string{"--new", "42 is wrong"}, declareArgs{}, "approval token"},
		{"new rejects an escape in the title", []string{"--new", "ok\x1b[2Jgone"}, declareArgs{}, "control character"},
		{"new rejects a newline in the title", []string{"--new", "ok\nrein: approved"}, declareArgs{}, "control character"},
		{"new rejects a bidi override", []string{"--new", "ok‮gnop"}, declareArgs{}, "format character"},
		{"new rejects an over-long title", []string{"--new", strings.Repeat("x", 201)}, declareArgs{}, "exceeds the 200"},
		{"new rejects an over-long body", []string{"--new", "Title", "--body", strings.Repeat("x", 4001)}, declareArgs{}, "exceeds the 4000"},
		{"new rejects an escape in the body", []string{"--new", "Title", "--body", "a\x07b"}, declareArgs{}, "control character"},
		{"body without new rejected", []string{"73", "--body", "x"}, declareArgs{}, "only applies to --new"},
		{"dangling new flag", []string{"--new"}, declareArgs{}, "--new needs a value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDeclareArgs(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestNewIssueBody pins the attribution rein appends: the issue is filed
// under the bot identity, so the body must say it was filed on a human's
// behalf — and must @-mention nobody, because rein never learns the
// operator's GitHub login and a guessed mention notifies the wrong account.
func TestNewIssueBody(t *testing.T) {
	const trailer = "_Filed by rein on behalf of the operator of this session._"
	for _, tc := range []struct{ name, body, want string }{
		{"empty", "", trailer},
		{"blank", "   \n ", trailer},
		{"plain", "why", "why\n\n" + trailer},
		{"trailing newlines collapse", "why\n\n\n", "why\n\n" + trailer},
		{"multiline preserved", "one\ntwo", "one\ntwo\n\n" + trailer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newIssueBody(tc.body); got != tc.want {
				t.Errorf("newIssueBody(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
	if strings.Contains(newIssueBody("x"), "@") {
		t.Error("the trailer must not @-mention anyone")
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
