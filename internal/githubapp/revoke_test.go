package githubapp

// Status mapping for RevokeToken (issue #67).
//
// The exit-revoke path revokes every token in the run's write-token ledger.
// Because the proxy memoizes one write token per run and re-records it on every
// write-serving request, the SAME token is presented for revocation more than
// once (the ledger is deduped at the consumer, but the expiry path can also
// have revoked it already). GitHub answers a repeat revocation with 404: "this
// is not a live installation token" — i.e. it is ALREADY GONE. That is the
// desired end state, not a failure, so RevokeToken maps it to success. Anything
// else still errors.
//
// These use the apiBaseURL test seam (see client.go), so no network is touched.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRevokeToken_StatusMapping(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"204 revoked", http.StatusNoContent, false},
		// The issue #67 case: a repeat revoke of an already-dead token. The
		// token is gone — which is the whole goal — so this must NOT be an
		// error, and must not print a scary best-effort warning.
		{"404 already gone", http.StatusNotFound, false},
		{"401 unauthorized", http.StatusUnauthorized, true},
		{"403 forbidden", http.StatusForbidden, true},
		{"500 server error", http.StatusInternalServerError, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath, gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := &Client{apiBaseURL: srv.URL} // test seam; production never sets this
			err := c.RevokeToken(context.Background(), "ghs_sometoken")

			if tc.wantErr && err == nil {
				t.Errorf("status %d: got nil error, want an error", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("status %d: got error %v, want nil (the token is dead — that IS success)", tc.status, err)
			}
			// The token itself authenticates the DELETE; assert we actually sent
			// it, at the right method and path.
			if gotMethod != http.MethodDelete {
				t.Errorf("method = %q, want DELETE", gotMethod)
			}
			if gotPath != "/installation/token" {
				t.Errorf("path = %q, want /installation/token", gotPath)
			}
			if gotAuth != "Bearer ghs_sometoken" {
				t.Errorf("Authorization = %q, want the token as Bearer", gotAuth)
			}
		})
	}
}

// RevokeToken must target the same API base as the mint path. If it kept
// hardcoding api.github.com while the mint pointed elsewhere, a revoke would hit
// the wrong host, 404, and — since 404 maps to "already gone" — be reported as a
// SUCCESSFUL revoke of a token that is in fact still live. Fail-open.
func TestRevokeToken_HonorsAPIBaseURL(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{apiBaseURL: srv.URL}
	if err := c.RevokeToken(context.Background(), "ghs_sometoken"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !hit {
		t.Error("revoke did not reach the configured API base — it is still hardcoding api.github.com, so a 404 from the wrong host would be misread as 'already revoked'")
	}
}
