package issuemeta

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFirstWord pins the confirmation token's derivation. It is the one
// string the human types, so punctuation, case and unicode all matter.
func TestFirstWord(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Add a thing", "Add"},
		{"Fix: the thing", "Fix"},
		{"[bug] crash on save", "bug"},
		{"  leading space", "leading"},
		{"--- fix the build", "fix"},
		{"123 numbered", "123"},
		{"Añadir soporte", "Añadir"},
		{"日本語のタイトル", "日本語のタイトル"},
		{"@mention first", "mention"},
		{"", ""},
		{"--- ???", ""},
		{"!!!", ""},
	}
	for _, tc := range cases {
		if got := FirstWord(tc.in); got != tc.want {
			t.Errorf("FirstWord(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestValidateTitleAgreesWithFirstWord: validation and the prompt must
// never disagree about what a word is — a title that validates must yield
// a typeable word, and one that yields none must be refused.
func TestValidateTitleAgreesWithFirstWord(t *testing.T) {
	for _, in := range []string{"Add a thing", "--- ???", "  ", "[x]", "!!!", "42"} {
		got, err := ValidateTitle(in)
		if err == nil && FirstWord(got) == "" {
			t.Errorf("ValidateTitle(%q) accepted a title with no typeable word", in)
		}
		if err != nil && FirstWord(strings.Join(strings.Fields(in), " ")) != "" {
			t.Errorf("ValidateTitle(%q) refused a title that has a word: %v", in, err)
		}
	}
}

func TestValidateTitle(t *testing.T) {
	cases := []struct {
		name, in, want, wantErr string
	}{
		{"normalizes whitespace", "  Add   a\tthing ", "Add a thing", ""},
		{"empty", "   ", "", "must not be empty"},
		{"escape refused", "ok\x1b[2Jgone", "", "control character"},
		{"newline refused", "ok\nrein: approved", "", "control character"},
		{"DEL refused", "ok\x7f", "", "control character"},
		{"bidi override refused", "ok‮gnop", "", "format character"},
		{"zero-width refused", "o​k", "", "format character"},
		{"too long", strings.Repeat("x", 201), "", "exceeds the 200"},
		{"exactly 200 ok", strings.Repeat("x", 200), strings.Repeat("x", 200), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateTitle(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("got (%q, %v), want %q", got, err, tc.want)
			}
		})
	}
}

func TestValidateBody(t *testing.T) {
	if got, err := ValidateBody("one\ntwo\n"); err != nil || got != "one\ntwo\n" {
		t.Errorf("newlines must survive a body: got (%q, %v)", got, err)
	}
	if got, err := ValidateBody("one\r\ntwo"); err != nil || got != "one\ntwo" {
		t.Errorf("CRLF must normalize: got (%q, %v)", got, err)
	}
	if got, err := ValidateBody(""); err != nil || got != "" {
		t.Errorf("an empty body is valid: got (%q, %v)", got, err)
	}
	for _, bad := range []string{"a\x1b[2Jb", "a\x00b", "a\rb", "a‮b"} {
		if _, err := ValidateBody(bad); err == nil {
			t.Errorf("ValidateBody(%q) accepted an unsafe body", bad)
		}
	}
	if _, err := ValidateBody(strings.Repeat("x", 4001)); err == nil {
		t.Error("an over-long body must be refused")
	}
}

// TestBodyExcerpt: what reaches the human's terminal must be one flat,
// bounded line — a body is prose and could otherwise forge prompt lines.
func TestBodyExcerpt(t *testing.T) {
	if got := BodyExcerpt("one\ntwo   three"); got != "one two three" {
		t.Errorf("excerpt must flatten whitespace: %q", got)
	}
	if got := BodyExcerpt("a\x1b[2Jb"); strings.Contains(got, "\x1b") {
		t.Errorf("excerpt must strip escapes: %q", got)
	}
	long := BodyExcerpt(strings.Repeat("x", 500))
	if !strings.HasSuffix(long, "(truncated)") {
		t.Errorf("a long body must be marked truncated: %q", long)
	}
	if len([]rune(long)) > bodyExcerptRunes+20 {
		t.Errorf("excerpt not bounded: %d runes", len([]rune(long)))
	}
}

func TestCreate(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"number": 7, "title": "Add a thing", "state": "open",
			"url": srvURL(r) + "/repos/o/r/issues/7",
		})
	}))
	defer srv.Close()

	meta, err := Create(context.Background(), srv.URL, "tok", "o/r", "Add a thing", "why")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if meta.Number != 7 || meta.Repo != "o/r" || meta.Title != "Add a thing" || meta.State != "open" {
		t.Errorf("meta = %+v", meta)
	}
	// The TM-G6 anchor must be present, or the per-write-mint transfer
	// re-check would silently exempt the issue rein just filed.
	if meta.CanonicalURL == "" || !strings.HasSuffix(meta.CanonicalURL, "/repos/o/r/issues/7") {
		t.Errorf("canonical URL = %q", meta.CanonicalURL)
	}
	if gotAuth != "Bearer tok" || gotPath != "/repos/o/r/issues" {
		t.Errorf("request = %s %s", gotAuth, gotPath)
	}
	if gotBody["title"] != "Add a thing" || gotBody["body"] != "why" {
		t.Errorf("posted body = %v", gotBody)
	}
}

// TestCreate_SurfacesGitHubsReason: a missing App permission lands here,
// and "status 403" alone would send the human hunting.
func TestCreate_SurfacesGitHubsReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer srv.Close()
	_, err := Create(context.Background(), srv.URL, "tok", "o/r", "T", "")
	if err == nil || !strings.Contains(err.Error(), "Resource not accessible") {
		t.Fatalf("err = %v, want GitHub's message", err)
	}
}

// TestCreate_NoNumberFailsClosed: a 201 that carries no number would
// otherwise be recorded as issue #0.
func TestCreate_NoNumberFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"title":"T"}`))
	}))
	defer srv.Close()
	if _, err := Create(context.Background(), srv.URL, "tok", "o/r", "T", ""); err == nil {
		t.Fatal("a numberless 201 must fail")
	}
}

func srvURL(r *http.Request) string { return "http://" + r.Host }
