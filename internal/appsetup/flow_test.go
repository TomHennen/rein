package appsetup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TomHennen/rein/internal/keystore"
)

// fakeGitHubAPI returns an httptest.Server that simulates GitHub's
// /app-manifests/{code}/conversions endpoint. It returns a different
// slug per call so primary and audit don't collide.
func fakeGitHubAPI(t *testing.T, pem string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		role := "primary"
		if calls > 1 {
			role = "audit"
		}
		fmt.Fprintf(w, `{"id":%d,"slug":"rein-%s-aabbccdd0%d","name":"rein-%s-aabbccdd0%d","client_id":"Iv23li%d","pem":%q,"html_url":"https://github.com/apps/rein-%s-aabbccdd0%d","owner":{"login":"alice"}}`,
			1000+calls, role, calls, role, calls, calls, pem, role, calls)
	}))
	return ts, &calls
}

// genTestPEMString returns a PEM-encoded RSA key as a string suitable
// for embedding in the fake conversion response.
func genTestPEMString(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	body := x509.MarshalPKCS1PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: body}))
}

// driveBrowserCallback installs a browserOpenerOverride that POSTs a
// fake state and code to the local listener's /callback path.
//
// The opener has access to the URL the listener serves on, which
// matches the manifest's redirect_url path/port. We extract the port
// from that URL and fire /callback?code=fake&state=<extracted>.
func driveBrowserCallback(t *testing.T, codePrefix string) func() {
	t.Helper()
	orig := browserOpenerOverride
	browserOpenerOverride = func(landingURL string) error {
		// landingURL is http://127.0.0.1:<port>/. Open it, parse the
		// HTML, extract the state nonce from the form action.
		go func() {
			// Tiny delay to let the listener fully install handlers.
			time.Sleep(50 * time.Millisecond)
			resp, err := http.Get(landingURL)
			if err != nil {
				t.Logf("fake opener get landing: %v", err)
				return
			}
			body := readAll(t, resp.Body)
			resp.Body.Close()
			// Extract state nonce from action="...settings/apps/new?state=NONCE"
			const marker = "?state="
			idx := strings.Index(body, marker)
			if idx < 0 {
				t.Logf("fake opener: no state in landing page")
				return
			}
			rest := body[idx+len(marker):]
			endIdx := strings.IndexAny(rest, `"`)
			if endIdx < 0 {
				t.Logf("fake opener: state nonce close-quote missing")
				return
			}
			nonce := rest[:endIdx]
			u, _ := url.Parse(landingURL)
			cb := fmt.Sprintf("http://%s/callback?code=%s&state=%s", u.Host, codePrefix, nonce)
			r, err := http.Get(cb)
			if err != nil {
				t.Logf("fake opener: callback fire: %v", err)
				return
			}
			r.Body.Close()
		}()
		return nil
	}
	return func() { browserOpenerOverride = orig }
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func TestRunManifestFlow_HappyPath(t *testing.T) {
	pem := genTestPEMString(t)
	ts, calls := fakeGitHubAPI(t, pem)
	defer ts.Close()
	defer driveBrowserCallback(t, "code")()

	dir := t.TempDir()
	ks := keystore.NewFileKeystore(dir)

	var stdout bytes.Buffer
	opts := RunOptions{
		ConfigDir: dir,
		Keystore:  ks,
		Stdout:    &stdout,
		Stderr:    io.Discard,
		APIBase:   ts.URL,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := RunManifestFlow(ctx, opts); err != nil {
		t.Fatalf("flow: %v\nstdout:\n%s", err, stdout.String())
	}
	if *calls != 2 {
		t.Errorf("api calls = %d, want 2", *calls)
	}
	s, err := ReadState(dir)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if s.Phase != PhaseAuditDone {
		t.Errorf("phase = %q", s.Phase)
	}
	if s.Source != SourceManifest {
		t.Errorf("source = %q", s.Source)
	}
	if s.Primary == nil || s.Audit == nil {
		t.Errorf("primary/audit = %+v / %+v", s.Primary, s.Audit)
	}
	if s.Primary != nil && s.Primary.KeyFingerprint == "" {
		t.Errorf("primary fingerprint empty")
	}
	// Both PEMs landed.
	if _, err := ks.Get("primary"); err != nil {
		t.Errorf("primary pem: %v", err)
	}
	if _, err := ks.Get("audit"); err != nil {
		t.Errorf("audit pem: %v", err)
	}
}

func TestRunManifestFlow_SkipAudit(t *testing.T) {
	pem := genTestPEMString(t)
	ts, calls := fakeGitHubAPI(t, pem)
	defer ts.Close()
	defer driveBrowserCallback(t, "code")()

	dir := t.TempDir()
	ks := keystore.NewFileKeystore(dir)
	var stdout bytes.Buffer
	opts := RunOptions{
		ConfigDir: dir,
		Keystore:  ks,
		SkipAudit: true,
		Stdout:    &stdout,
		Stderr:    io.Discard,
		APIBase:   ts.URL,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := RunManifestFlow(ctx, opts); err != nil {
		t.Fatalf("flow: %v\nstdout:\n%s", err, stdout.String())
	}
	if *calls != 1 {
		t.Errorf("api calls = %d, want 1", *calls)
	}
	s, err := ReadState(dir)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if s.Phase != PhasePrimaryDone {
		t.Errorf("phase = %q, want primary_done", s.Phase)
	}
	if s.Audit != nil {
		t.Errorf("audit should be nil, got %+v", s.Audit)
	}
	if _, err := ks.Get("audit"); !errors.Is(err, keystore.ErrNotFound) {
		t.Errorf("audit pem: %v (want ErrNotFound)", err)
	}
}

func TestRunManifestFlow_ResumeFromPrimaryDone(t *testing.T) {
	pem := genTestPEMString(t)
	ts, calls := fakeGitHubAPI(t, pem)
	defer ts.Close()
	defer driveBrowserCallback(t, "code")()

	dir := t.TempDir()
	ks := keystore.NewFileKeystore(dir)
	// Seed primary_done state.
	prior := State{
		Phase:  PhasePrimaryDone,
		Source: SourceManifest,
		Primary: &AppRecord{
			Slug:      "rein-primary-existing0",
			AppID:     999,
			ClientID:  "existing-cid",
			CreatedAt: time.Now().UTC(),
		},
	}
	if err := WriteState(dir, prior); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var stdout bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := RunManifestFlow(ctx, RunOptions{
		ConfigDir: dir,
		Keystore:  ks,
		Stdout:    &stdout,
		Stderr:    io.Discard,
		APIBase:   ts.URL,
	}); err != nil {
		t.Fatalf("flow: %v\nstdout:\n%s", err, stdout.String())
	}
	if *calls != 1 {
		t.Errorf("api calls = %d, want 1 (audit only)", *calls)
	}
	s, _ := ReadState(dir)
	if s.Phase != PhaseAuditDone {
		t.Errorf("phase = %q", s.Phase)
	}
	if s.Primary == nil || s.Primary.Slug != "rein-primary-existing0" {
		t.Errorf("primary record overwritten: %+v", s.Primary)
	}
	if s.Audit == nil {
		t.Errorf("audit not created")
	}
}

func TestRunManifestFlow_OwnerMismatch(t *testing.T) {
	pem := genTestPEMString(t)
	ts, _ := fakeGitHubAPI(t, pem)
	defer ts.Close()
	defer driveBrowserCallback(t, "code")()

	dir := t.TempDir()
	ks := keystore.NewFileKeystore(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := RunManifestFlow(ctx, RunOptions{
		ConfigDir:     dir,
		Keystore:      ks,
		Stdout:        io.Discard,
		Stderr:        io.Discard,
		APIBase:       ts.URL,
		ExpectedOwner: "bob", // fake returns "alice"
	})
	if err == nil {
		t.Fatal("expected owner-mismatch error")
	}
	if !strings.Contains(err.Error(), "alice") || !strings.Contains(err.Error(), "bob") {
		t.Errorf("err %q should name both owners", err.Error())
	}
	// No state.json should exist — we refused to persist anything.
	if _, err := ReadState(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("state.json was written despite owner mismatch: %v", err)
	}
	// Primary PEM should also be absent.
	if _, err := ks.Get("primary"); !errors.Is(err, keystore.ErrNotFound) {
		t.Errorf("primary PEM was saved despite owner mismatch: %v", err)
	}
}

func TestRunManifestFlow_RefusesOrphanPEM(t *testing.T) {
	// Pre-seed a primary.pem with no covering state.json record.
	// Without the guard, RunManifestFlow would create a new App at
	// GitHub and silently overwrite the local PEM, orphaning the old
	// App. With the guard it must refuse.
	pem := genTestPEMString(t)
	ts, _ := fakeGitHubAPI(t, pem)
	defer ts.Close()
	defer driveBrowserCallback(t, "code")()

	dir := t.TempDir()
	ks := keystore.NewFileKeystore(dir)
	if err := ks.Set("primary", []byte(pem)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := RunManifestFlow(ctx, RunOptions{
		ConfigDir: dir,
		Keystore:  ks,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		APIBase:   ts.URL,
	})
	if err == nil {
		t.Fatal("expected refusal due to orphan PEM")
	}
	if !strings.Contains(err.Error(), "refusing") || !strings.Contains(err.Error(), "primary") {
		t.Errorf("error %q should mention refusal", err.Error())
	}
}

func TestRunManifestFlow_ForceBypassesOrphanGuard(t *testing.T) {
	pem := genTestPEMString(t)
	ts, _ := fakeGitHubAPI(t, pem)
	defer ts.Close()
	defer driveBrowserCallback(t, "code")()

	dir := t.TempDir()
	ks := keystore.NewFileKeystore(dir)
	if err := ks.Set("primary", []byte(pem)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := RunManifestFlow(ctx, RunOptions{
		ConfigDir: dir,
		Keystore:  ks,
		Force:     true,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		APIBase:   ts.URL,
	}); err != nil {
		t.Fatalf("force should bypass orphan guard: %v", err)
	}
}

func TestNeedsManifestFlow(t *testing.T) {
	// NeedsManifestFlow is the single source of truth for "does the
	// manifest flow run?". It returns true in exactly three cases:
	// Force, absent state with env-absent, or primary_done with
	// env-absent (implicit resume). Everything else returns false,
	// including the env-present overrides and the corrupt-state case.
	cases := []struct {
		name       string
		state      State
		stateErr   error
		envPresent bool
		opts       RunOptions
		want       bool
	}{
		{"absent-no-env", State{}, fs.ErrNotExist, false, RunOptions{}, true},
		// env-present + absent state → env marker is written, flow doesn't run.
		{"absent-with-env", State{}, fs.ErrNotExist, true, RunOptions{}, false},
		{"audit-done", State{Phase: PhaseAuditDone}, nil, false, RunOptions{}, false},
		{"managed-externally", State{Phase: PhaseManagedExternally}, nil, true, RunOptions{}, false},
		{"primary-done-no-env-implicit-resume", State{Phase: PhasePrimaryDone}, nil, false, RunOptions{}, true},
		{"primary-done-with-env-no-resume", State{Phase: PhasePrimaryDone}, nil, true, RunOptions{}, false},
		// env present with primary_done routes to classifyEnvMatch;
		// flow doesn't run on this invocation. (Pre-removal this case
		// also asserted --resume had no effect; the field is gone now,
		// so the row keeps the env-present-overrides-primary-done
		// coverage without the resume axis.)
		{"primary-done-with-env", State{Phase: PhasePrimaryDone}, nil, true, RunOptions{}, false},
		{"force-overrides", State{Phase: PhaseAuditDone}, nil, true, RunOptions{Force: true}, true},
		// Corrupt state.json: refuse (BridgeStateCorrupt), don't run.
		{"corrupt-state-no-env", State{}, errors.New("parse failed"), false, RunOptions{}, false},
		// Force trumps corrupt: --force is "ignore disk, start over".
		{"corrupt-state-force", State{}, errors.New("parse failed"), false, RunOptions{Force: true}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := NeedsManifestFlow(tc.state, tc.stateErr, tc.opts, tc.envPresent); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunManifestFlow_StateJSONShape(t *testing.T) {
	// Lock in the JSON serialization for the happy-path output so
	// downstream readers (rein doctor, Stage 2 migrations) can rely
	// on it.
	pem := genTestPEMString(t)
	ts, _ := fakeGitHubAPI(t, pem)
	defer ts.Close()
	defer driveBrowserCallback(t, "code")()

	dir := t.TempDir()
	ks := keystore.NewFileKeystore(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := RunManifestFlow(ctx, RunOptions{
		ConfigDir: dir,
		Keystore:  ks,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		APIBase:   ts.URL,
	}); err != nil {
		t.Fatalf("flow: %v", err)
	}
	body, err := readFile(StatePath(dir))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, want := range []string{"phase", "source", "primary", "audit", "schema_version"} {
		if _, ok := m[want]; !ok {
			t.Errorf("state.json missing %q (got %v)", want, m)
		}
	}
}

func readFile(p string) ([]byte, error) {
	return os.ReadFile(p)
}

// setFailingKeystore wraps a real keystore but fails the first N Set
// calls (per role). Get/Delete/Fingerprint delegate through. Used to
// exercise the PEM-write-after-conversion failure path.
type setFailingKeystore struct {
	inner    keystore.Keystore
	failOn   map[string]int // role → remaining Set failures
	failWith error
}

func (s *setFailingKeystore) Get(name string) ([]byte, error) { return s.inner.Get(name) }
func (s *setFailingKeystore) Delete(name string) error        { return s.inner.Delete(name) }
func (s *setFailingKeystore) Fingerprint(name string) (string, error) {
	return s.inner.Fingerprint(name)
}
func (s *setFailingKeystore) Set(name string, data []byte) error {
	if s.failOn[name] > 0 {
		s.failOn[name]--
		return s.failWith
	}
	return s.inner.Set(name, data)
}

// TestRunManifestFlow_PEMWriteFailurePersistsPartialRecord verifies
// that when Keystore.Set fails after a successful conversion, the
// partial AppRecord is still written to state.json. This is the bug
// the orphan-PEM guard would otherwise re-create: without the persisted
// record, the user's recovery `rein init` re-run (after manually
// placing the PEM per the error message) would be refused by the
// orphan-PEM guard. With the partial record present, the implicit
// resume sees state.Primary != nil and skips step 1 — adopting the
// placed PEM.
func TestRunManifestFlow_PEMWriteFailurePersistsPartialRecord(t *testing.T) {
	pem := genTestPEMString(t)
	ts, _ := fakeGitHubAPI(t, pem)
	defer ts.Close()
	defer driveBrowserCallback(t, "code")()

	dir := t.TempDir()
	inner := keystore.NewFileKeystore(dir)
	ks := &setFailingKeystore{
		inner:    inner,
		failOn:   map[string]int{"primary": 1},
		failWith: errors.New("simulated disk full"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := RunManifestFlow(ctx, RunOptions{
		ConfigDir: dir,
		Keystore:  ks,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		APIBase:   ts.URL,
	})
	if err == nil {
		t.Fatal("expected PEM-write failure to surface")
	}
	if !strings.Contains(err.Error(), "save PEM to keystore") {
		t.Errorf("err %q should mention PEM save failure", err.Error())
	}
	// Partial state must exist so the orphan guard does not refuse the
	// recovery re-run (implicit resume on env-absence).
	s, serr := ReadState(dir)
	if serr != nil {
		t.Fatalf("state.json should exist after partial failure: %v", serr)
	}
	if s.Primary == nil || s.Primary.Slug == "" {
		t.Fatalf("partial primary record missing: %+v", s)
	}
	if s.Phase != PhasePrimaryDone {
		t.Errorf("phase = %q, want %q", s.Phase, PhasePrimaryDone)
	}

	// Simulate the user manually placing the PEM (the error message's
	// step 3) and re-running `rein init` (implicit resume).
	if err := inner.Set("primary", []byte(pem)); err != nil {
		t.Fatalf("simulate user PEM placement: %v", err)
	}
	// Clear the failure for the audit step's Set.
	ks.failOn = map[string]int{}

	if err := RunManifestFlow(ctx, RunOptions{
		ConfigDir: dir,
		Keystore:  ks,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		APIBase:   ts.URL,
	}); err != nil {
		t.Fatalf("resume after recovery: %v", err)
	}
	// Resume should have adopted the placed primary PEM (skipped step
	// 1) and proceeded to create the audit App.
	s2, _ := ReadState(dir)
	if s2.Phase != PhaseAuditDone {
		t.Errorf("phase after resume = %q, want %q", s2.Phase, PhaseAuditDone)
	}
	if s2.Primary == nil || s2.Primary.Slug != s.Primary.Slug {
		t.Errorf("primary record changed across resume: was %q, now %q",
			s.Primary.Slug, func() string {
				if s2.Primary == nil {
					return "<nil>"
				}
				return s2.Primary.Slug
			}())
	}
	if s2.Audit == nil {
		t.Errorf("audit not created on resume")
	}
}
