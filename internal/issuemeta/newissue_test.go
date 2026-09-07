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

// TestValidateTitleAgreesWithValidConfirmWord: validation and the prompt
// must never disagree about what makes a usable token — a title that
// validates must yield one, and one that does not must be refused.
func TestValidateTitleAgreesWithValidConfirmWord(t *testing.T) {
	for _, in := range []string{
		"Add a thing", "--- ???", "  ", "[x]", "!!!", "42",
		"a thing", "an apple", "42 things", "Fix: it", "日本語 の タイトル",
	} {
		got, err := ValidateTitle(in)
		normalized := strings.Join(strings.Fields(in), " ")
		if err == nil && !ValidConfirmWord(FirstWord(got)) {
			t.Errorf("ValidateTitle(%q) accepted a title whose first word is not a usable token", in)
		}
		if err != nil && ValidConfirmWord(FirstWord(normalized)) {
			t.Errorf("ValidateTitle(%q) refused a title with a usable token: %v", in, err)
		}
	}
}

// TestValidConfirmWord pins the token floor. It is NOT a denylist of
// affirmative words — "yes"/"ok" stay legal (settled: design §2.2 makes
// the title's first word the token). It rules out tokens that are too
// weak to be a deliberate act, and numeric ones that collide with the
// `rein declare <n>` number token.
func TestValidConfirmWord(t *testing.T) {
	for _, ok := range []string{"Add", "fix", "bug", "yes", "okay", "Añadir", "日本語", "a1b", "sbom-action"} {
		if !ValidConfirmWord(ok) {
			t.Errorf("ValidConfirmWord(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "a", "an", "1", "42", "123", "---", "42x"[:2]} {
		if ValidConfirmWord(bad) {
			t.Errorf("ValidConfirmWord(%q) = true, want false", bad)
		}
	}
}

// TestValidateTitle_RejectsWeakTokens: the error must tell the agent to
// rephrase, since it is the one who can fix it.
func TestValidateTitle_RejectsWeakTokens(t *testing.T) {
	for _, in := range []string{"a broken thing", "42 is wrong", "1 crash on save", "an error occurs"} {
		_, err := ValidateTitle(in)
		if err == nil {
			t.Errorf("ValidateTitle(%q) accepted a weak approval token", in)
			continue
		}
		if !strings.Contains(err.Error(), "rephrase") {
			t.Errorf("ValidateTitle(%q) error should tell the agent to rephrase: %v", in, err)
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
	// The marker must COUNT what is withheld: the human is approving the
	// filing of text they cannot see, and needs to know how much.
	long := BodyExcerpt(strings.Repeat("x", 500))
	for _, want := range []string{"truncated", "300 more characters", "unseen"} {
		if !strings.Contains(long, want) {
			t.Errorf("truncation marker missing %q: %q", want, long)
		}
	}
	if len([]rune(long)) > bodyExcerptRunes+80 {
		t.Errorf("excerpt not bounded: %d runes", len([]rune(long)))
	}
}

// TestSanitizeProposedTitle: the human must see EVERY character that
// will be filed. SanitizeTitle's 140-rune budget is for a FETCHED title
// and would hide 60 agent-controlled characters of a 200-char proposal.
func TestSanitizeProposedTitle(t *testing.T) {
	full := strings.Repeat("x", MaxTitleChars)
	if got := SanitizeProposedTitle(full); got != full {
		t.Errorf("a title at the accepted maximum must render whole: %d of %d runes",
			len([]rune(got)), MaxTitleChars)
	}
	if got := SanitizeTitle(full); len([]rune(got)) >= MaxTitleChars {
		t.Skip("SanitizeTitle no longer truncates; this test's premise is gone")
	}
	// Still sanitized, and still bounded for anything longer.
	if got := SanitizeProposedTitle("a\x1b[2Jb"); strings.Contains(got, "\x1b") {
		t.Errorf("escape survived: %q", got)
	}
	if got := SanitizeProposedTitle(strings.Repeat("y", 500)); len([]rune(got)) > MaxTitleChars+1 {
		t.Errorf("not bounded: %d runes", len([]rune(got)))
	}
}

func TestCreate(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// The same issue, as Fetch would read it back.
			json.NewEncoder(w).Encode(map[string]any{
				"number": 7, "title": "Add a thing", "state": "open",
				"url": srvURL(r) + "/repos/o/r/issues/7",
			})
			return
		}
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"number": 7, "title": "Add a thing", "state": "open",
			// GitHub returns both; only `url` is the REST anchor
			// CheckCanonical can GET with a Bearer token.
			"url":      srvURL(r) + "/repos/o/r/issues/7",
			"html_url": "https://github.com/o/r/issues/7",
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
	if meta.CanonicalURL != srv.URL+"/repos/o/r/issues/7" {
		t.Errorf("canonical URL = %q, want the REST url (not html_url)", meta.CanonicalURL)
	}
	// Same anchor shape the declared-issue path records, so the TM-G6
	// re-check treats a filed issue exactly like a declared one.
	if fetched := mustFetch(t, srv.URL); fetched.CanonicalURL != meta.CanonicalURL {
		t.Errorf("Create anchor %q != Fetch anchor %q", meta.CanonicalURL, fetched.CanonicalURL)
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

func mustFetch(t *testing.T, base string) Meta {
	t.Helper()
	m, err := Fetch(context.Background(), base, "tok", "o/r", 7)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	return m
}
