package githubapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMint_RequestBodyPinsScopeCeiling pins the scope-ceiling invariant at
// the wire (conformance audit #44 §2): every Mint* tier must send an
// installation-token request whose JSON body carries BOTH the full session
// repository set (Config.RepoNames — token scope == scope ceiling, issue
// #10) AND exactly the tier's permission map. Before this test, a
// regression that dropped `opts` from the mint call would still return a
// working token (GitHub defaults to the installation's full grant) and
// pass every other test — silently widening every minted credential.
//
// The mint is pointed at a local fake /access_tokens endpoint via the
// apiBaseURL test seam (see client.go); no network is touched.
func TestMint_RequestBodyPinsScopeCeiling(t *testing.T) {
	cases := []struct {
		name      string
		mint      func(*Client, context.Context) (string, time.Time, error)
		wantPerms map[string]string
		// readTier marks a read-only TIER: the tier-split invariant says such
		// a token must carry NO "write" permission on ANY resource, so a
		// token cached/exfiltrated on the read path grants read-only
		// capability only. Asserted explicitly below (in addition to the
		// exact-map check) so the split is a named, self-documenting invariant.
		readTier bool
	}{
		{
			name: "read-only",
			mint: (*Client).MintReadOnlyToken,
			wantPerms: map[string]string{
				"contents": "read",
				"metadata": "read",
			},
			readTier: true,
		},
		{
			name: "write",
			mint: (*Client).MintWriteToken,
			wantPerms: map[string]string{
				"contents": "write",
				"metadata": "read",
			},
		},
		{
			name: "gh-read-only",
			mint: (*Client).MintGhReadOnlyToken,
			wantPerms: map[string]string{
				"contents":      "read",
				"issues":        "read",
				"pull_requests": "read",
				"metadata":      "read",
			},
			readTier: true,
		},
		{
			name: "gh-session",
			mint: (*Client).MintGhSessionToken,
			wantPerms: map[string]string{
				"contents":      "write",
				"issues":        "write",
				"pull_requests": "write",
				"metadata":      "read",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu   sync.Mutex
				path string
				auth string
				body []byte
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				path = r.URL.Path
				auth = r.Header.Get("Authorization")
				body = b
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				resp := map[string]string{
					"token":      "ghs_test_" + tc.name,
					"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				}
				_ = json.NewEncoder(w).Encode(resp)
			}))
			defer srv.Close()

			ks := newStubKeystore()
			if err := ks.Set("primary", genTestPEM(t)); err != nil {
				t.Fatalf("seed keystore: %v", err)
			}
			c, err := NewClient(Config{
				ClientID:       "Iv23li-x",
				InstallationID: 42,
				// Multi-repo ceiling: the WHOLE set must reach the wire,
				// not just the first repo (issue #10 regression class).
				RepoNames: []string{"alpha", "beta"},
			}, ks, "primary")
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			c.apiBaseURL = srv.URL // test seam; production never sets this

			tok, _, err := tc.mint(c, context.Background())
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			if want := "ghs_test_" + tc.name; tok != want {
				t.Errorf("token = %q, want %q (fake endpoint's response)", tok, want)
			}

			mu.Lock()
			defer mu.Unlock()

			// Right endpoint, for the right installation, authenticated as
			// the App (JWT bearer), not with some ambient credential.
			if !strings.HasSuffix(path, "/app/installations/42/access_tokens") {
				t.Errorf("request path = %q, want suffix /app/installations/42/access_tokens", path)
			}
			if !strings.HasPrefix(auth, "Bearer ") || len(auth) < len("Bearer ")+20 {
				t.Errorf("Authorization = %q, want a Bearer app JWT", auth)
			}

			// The body must carry the scope ceiling: repositories AND
			// permissions, and nothing else that could widen the grant.
			var got struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal request body %q: %v", body, err)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatalf("unmarshal request body (raw) %q: %v", body, err)
			}
			for k := range raw {
				if k != "repositories" && k != "permissions" {
					t.Errorf("unexpected top-level field %q in token request body %s", k, body)
				}
			}

			if len(got.Repositories) != 2 || got.Repositories[0] != "alpha" || got.Repositories[1] != "beta" {
				t.Errorf("repositories = %v, want [alpha beta] (full session scope ceiling)", got.Repositories)
			}
			if len(got.Permissions) != len(tc.wantPerms) {
				t.Errorf("permissions = %v, want exactly %v (no extra grants)", got.Permissions, tc.wantPerms)
			}
			for k, v := range tc.wantPerms {
				if got.Permissions[k] != v {
					t.Errorf("permissions[%q] = %q, want %q", k, got.Permissions[k], v)
				}
			}

			// Tier-split invariant: a read tier must confer NO write anywhere.
			if tc.readTier {
				for k, v := range got.Permissions {
					if v == "write" {
						t.Errorf("read tier %q leaked a write permission on the wire: %q=write (tier split broken)", tc.name, k)
					}
				}
			}
		})
	}
}
