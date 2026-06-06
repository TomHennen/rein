package githubapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// appKeystore returns a stubKeystore (defined in client_test.go) seeded
// with a valid RSA PEM under "primary" so the App-JWT mint succeeds offline.
func appKeystore(t *testing.T) *stubKeystore {
	t.Helper()
	ks := newStubKeystore()
	ks.data["primary"] = genTestPEM(t)
	return ks
}

func TestRepoInstallationID_OK(t *testing.T) {
	var gotAuth, gotAccept, gotVersion, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotVersion = r.Header.Get("X-GitHub-Api-Version")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id": 999, "app_id": 12}`))
	}))
	defer srv.Close()

	c, err := NewAppClient("Iv23li-x", appKeystore(t), "primary", srv.URL)
	if err != nil {
		t.Fatalf("NewAppClient: %v", err)
	}
	id, err := c.RepoInstallationID(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("RepoInstallationID: %v", err)
	}
	if id != 999 {
		t.Errorf("id = %d, want 999", id)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") || len(gotAuth) <= len("Bearer ") {
		t.Errorf("Authorization = %q, want non-empty Bearer token", gotAuth)
	}
	if gotAccept != "application/vnd.github+json" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if gotVersion != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q", gotVersion)
	}
	if gotPath != "/repos/owner/repo/installation" {
		t.Errorf("path = %q, want /repos/owner/repo/installation", gotPath)
	}
}

func TestRepoInstallationID_404NotInstalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message": "Not Found"}`))
	}))
	defer srv.Close()

	c, err := NewAppClient("Iv23li-x", appKeystore(t), "primary", srv.URL)
	if err != nil {
		t.Fatalf("NewAppClient: %v", err)
	}
	_, err = c.RepoInstallationID(context.Background(), "owner", "repo")
	if !errors.Is(err, ErrAppNotInstalled) {
		t.Fatalf("err = %v, want ErrAppNotInstalled", err)
	}
}

func TestRepoInstallationID_500Descriptive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message": "boom"}`))
	}))
	defer srv.Close()

	c, err := NewAppClient("Iv23li-x", appKeystore(t), "primary", srv.URL)
	if err != nil {
		t.Fatalf("NewAppClient: %v", err)
	}
	_, err = c.RepoInstallationID(context.Background(), "owner", "repo")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errors.Is(err, ErrAppNotInstalled) {
		t.Errorf("500 must not map to ErrAppNotInstalled: %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status 500: %v", err)
	}
}

func TestRepoInstallationID_ZeroIDIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": 0}`))
	}))
	defer srv.Close()

	c, _ := NewAppClient("Iv23li-x", appKeystore(t), "primary", srv.URL)
	_, err := c.RepoInstallationID(context.Background(), "owner", "repo")
	if err == nil {
		t.Fatal("expected error on id=0")
	}
}

func TestNewAppClient_Validates(t *testing.T) {
	ks := appKeystore(t)
	if _, err := NewAppClient("", ks, "primary", ""); err == nil {
		t.Error("empty clientID should error")
	}
	if _, err := NewAppClient("x", nil, "primary", ""); err == nil {
		t.Error("nil keystore should error")
	}
	if _, err := NewAppClient("x", ks, "", ""); err == nil {
		t.Error("empty roleName should error")
	}
	c, err := NewAppClient("x", ks, "primary", "")
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	if c.apiBase != DefaultAPIBase {
		t.Errorf("apiBase default = %q, want %q", c.apiBase, DefaultAPIBase)
	}
}
