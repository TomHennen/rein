package appsetup

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const fixturePEM = "-----BEGIN RSA PRIVATE KEY-----\nFIXTURE\n-----END RSA PRIVATE KEY-----\n"

func TestConvertManifestCode_HappyPath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app-manifests/abc/conversions" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.Error(w, "wrong path", 404)
			return
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization header sent (must NOT be): %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":123,"slug":"rein-primary-deadbeef00","name":"rein-primary-deadbeef00","client_id":"Iv23liX","pem":%q,"html_url":"https://github.com/apps/rein-primary-deadbeef00","owner":{"login":"alice"}}`, fixturePEM)
	}))
	defer ts.Close()

	cfg, err := ConvertManifestCode(context.Background(), ts.URL, "abc")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if cfg.ID != 123 || cfg.Slug != "rein-primary-deadbeef00" || cfg.ClientID != "Iv23liX" {
		t.Errorf("got %+v", cfg)
	}
	if cfg.Owner.Login != "alice" {
		t.Errorf("owner.login = %q", cfg.Owner.Login)
	}
	if cfg.PEM != fixturePEM {
		t.Errorf("pem mismatch")
	}
}

func TestConvertManifestCode_NoAuthHeader(t *testing.T) {
	// Belt and braces — happy-path test asserts no Authorization, but
	// this case fails the test loudly if a refactor ever introduces
	// one.
	seen := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			seen = true
		}
		w.WriteHeader(404)
	}))
	defer ts.Close()

	_, _ = ConvertManifestCode(context.Background(), ts.URL, "abc")
	if seen {
		t.Error("Authorization header was sent")
	}
}

func TestConvertManifestCode_404(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprint(w, `{"message":"Not Found","status":"404"}`)
	}))
	defer ts.Close()

	_, err := ConvertManifestCode(context.Background(), ts.URL, "abc")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q should mention 404", err.Error())
	}
	if !strings.Contains(err.Error(), "Not Found") {
		t.Errorf("error %q should include response body", err.Error())
	}
}

func TestConvertManifestCode_NetworkError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := ts.URL
	ts.Close()
	_, err := ConvertManifestCode(context.Background(), addr, "abc")
	if err == nil {
		t.Fatal("expected network error")
	}
}

func TestConvertManifestCode_EmptyCode(t *testing.T) {
	_, err := ConvertManifestCode(context.Background(), "http://example.com", "")
	if err == nil {
		t.Error("expected empty-code rejection")
	}
}

func TestConvertManifestCode_MissingFields(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":1,"slug":""}`)
	}))
	defer ts.Close()
	_, err := ConvertManifestCode(context.Background(), ts.URL, "abc")
	if err == nil {
		t.Error("expected missing-fields error")
	}
}
