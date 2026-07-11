package issuemeta

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeIssueServer serves /repos/o/r/issues/N per the handler table.
func fakeIssueServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetch_OKIssue(t *testing.T) {
	var gotAuth string
	srv := fakeIssueServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/repos/o/r/issues/73" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		fmt.Fprintf(w, `{"number":73,"title":"fix the flux capacitor","state":"open","url":"%s/repos/o/r/issues/73"}`, "https://api.github.com")
	})
	m, err := Fetch(context.Background(), srv.URL, "tok", "o/r", 73)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if m.Title != "fix the flux capacitor" || m.State != "open" || m.IsPR || m.Number != 73 || m.Repo != "o/r" {
		t.Errorf("unexpected meta: %+v", m)
	}
	if m.CanonicalURL != "https://api.github.com/repos/o/r/issues/73" {
		t.Errorf("canonical URL = %q", m.CanonicalURL)
	}
}

func TestFetch_PRFlag(t *testing.T) {
	srv := fakeIssueServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":5,"title":"a pr","state":"open","url":"u","pull_request":{"url":"pu"}}`)
	})
	m, err := Fetch(context.Background(), srv.URL, "tok", "o/r", 5)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !m.IsPR {
		t.Error("pull_request key present must set IsPR (§9: PR numbers are valid declarations, labeled)")
	}
}

func TestFetch_NotFound(t *testing.T) {
	srv := fakeIssueServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	_, err := Fetch(context.Background(), srv.URL, "tok", "o/r", 999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestFetch_TransferredNotFollowed(t *testing.T) {
	followed := false
	srv := fakeIssueServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/other/") {
			followed = true
			fmt.Fprint(w, `{"number":73,"title":"moved","state":"open","url":"u"}`)
			return
		}
		w.Header().Set("Location", "/repos/other/r/issues/73")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	_, err := Fetch(context.Background(), srv.URL, "tok", "o/r", 73)
	if !errors.Is(err, ErrTransferred) {
		t.Errorf("expected ErrTransferred, got %v", err)
	}
	if followed {
		t.Error("client must NEVER follow the redirect (TM-G6: deny, don't re-anchor)")
	}
}

func TestFetch_ServerErrorFailsClosed(t *testing.T) {
	srv := fakeIssueServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := Fetch(context.Background(), srv.URL, "tok", "o/r", 73); err == nil {
		t.Error("5xx must be an error (no prompt without a fetched title — decision E)")
	}
}

func TestFetch_NumberMismatchRefused(t *testing.T) {
	srv := fakeIssueServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":74,"title":"wrong one","state":"open","url":"u"}`)
	})
	if _, err := Fetch(context.Background(), srv.URL, "tok", "o/r", 73); err == nil {
		t.Error("a 200 for a different number must be refused")
	}
}

func TestFetch_InputValidation(t *testing.T) {
	if _, err := Fetch(context.Background(), "http://x", "tok", "not-a-repo", 1); err == nil {
		t.Error("repo without owner/name must be refused")
	}
	if _, err := Fetch(context.Background(), "http://x", "tok", "o/r", 0); err == nil {
		t.Error("non-positive issue number must be refused")
	}
}

func TestCheckCanonical(t *testing.T) {
	srv := fakeIssueServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, `{}`)
		case "/moved":
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(http.StatusMovedPermanently)
		default:
			http.Error(w, "no", http.StatusForbidden)
		}
	})
	if err := CheckCanonical(context.Background(), "tok", srv.URL+"/ok"); err != nil {
		t.Errorf("200 in place should pass: %v", err)
	}
	if err := CheckCanonical(context.Background(), "tok", srv.URL+"/moved"); !errors.Is(err, ErrTransferred) {
		t.Errorf("3xx must be ErrTransferred, got %v", err)
	}
	if err := CheckCanonical(context.Background(), "tok", srv.URL+"/other"); err == nil {
		t.Error("other statuses must fail (could not verify ⇒ fail closed)")
	}
	if err := CheckCanonical(context.Background(), "tok", ""); err == nil {
		t.Error("empty canonical URL must fail")
	}
}

func TestSanitizeTitle(t *testing.T) {
	got := SanitizeTitle("evil\x1b[31mred\x1b[0m\r\nline\x00")
	if strings.ContainsAny(got, "\x1b\r\n\x00") {
		t.Errorf("control characters must be stripped, got %q", got)
	}
	long := strings.Repeat("x", 500)
	if s := SanitizeTitle(long); len([]rune(s)) > maxTitleRunes+1 {
		t.Errorf("title must be truncated, got %d runes", len([]rune(s)))
	}
}
