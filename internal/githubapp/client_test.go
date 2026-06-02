package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/keystore"
)

// stubKeystore is a tiny in-memory Keystore for unit tests. It is
// deliberately local to this test file rather than exported from the
// keystore package — production code does not need an in-memory
// backend, and exporting one would invite accidental use.
type stubKeystore struct {
	data map[string][]byte
	err  map[string]error
}

func newStubKeystore() *stubKeystore {
	return &stubKeystore{data: map[string][]byte{}, err: map[string]error{}}
}

func (s *stubKeystore) Get(name string) ([]byte, error) {
	if e, ok := s.err[name]; ok {
		return nil, e
	}
	if d, ok := s.data[name]; ok {
		return d, nil
	}
	return nil, keystore.ErrNotFound
}

func (s *stubKeystore) Set(name string, data []byte) error {
	s.data[name] = data
	return nil
}

func (s *stubKeystore) Delete(name string) error {
	delete(s.data, name)
	delete(s.err, name)
	return nil
}

func (s *stubKeystore) Fingerprint(name string) (string, error) {
	return "stub-fingerprint:" + name, nil
}

// genTestPEM produces a valid 2048-bit RSA PKCS#1 PEM. Mint never
// actually runs (no live network in these unit tests), but go-githubauth's
// app token source parses the PEM up front, so the bytes must be valid
// even for paths that never reach the wire.
func genTestPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	body := x509.MarshalPKCS1PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: body})
}

func TestNewClient_Validates(t *testing.T) {
	good := Config{ClientID: "Iv23li-x", InstallationID: 42, RepoName: "throwaway"}
	ks := newStubKeystore()

	cases := []struct {
		name     string
		cfg      Config
		ks       keystore.Keystore
		roleName string
		wantErr  string
	}{
		{
			name: "missing-client-id",
			cfg:  Config{InstallationID: 42, RepoName: "throwaway"},
			ks:   ks, roleName: "primary",
			wantErr: "ClientID is required",
		},
		{
			name: "missing-installation-id",
			cfg:  Config{ClientID: "Iv23li-x", RepoName: "throwaway"},
			ks:   ks, roleName: "primary",
			wantErr: "InstallationID is required",
		},
		{
			name: "missing-repo-name",
			cfg:  Config{ClientID: "Iv23li-x", InstallationID: 42},
			ks:   ks, roleName: "primary",
			wantErr: "RepoName is required",
		},
		{
			name: "missing-keystore",
			cfg:  good,
			ks:   nil, roleName: "primary",
			wantErr: "keystore is required",
		},
		{
			name: "missing-role-name",
			cfg:  good,
			ks:   ks, roleName: "",
			wantErr: "roleName is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewClient(tc.cfg, tc.ks, tc.roleName)
			if err == nil {
				t.Fatalf("got Client=%v, want error containing %q", c, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestNewClient_FromKeystore_OK(t *testing.T) {
	ks := newStubKeystore()
	if err := ks.Set("primary", genTestPEM(t)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c, err := NewClient(Config{
		ClientID:       "Iv23li-x",
		InstallationID: 42,
		RepoName:       "throwaway",
	}, ks, "primary")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c == nil {
		t.Fatal("NewClient returned nil client with nil error")
	}
}

func TestMint_PropagatesKeystoreError(t *testing.T) {
	ks := newStubKeystore()
	ks.err["primary"] = keystore.ErrNotFound
	c, err := NewClient(Config{
		ClientID:       "Iv23li-x",
		InstallationID: 42,
		RepoName:       "throwaway",
	}, ks, "primary")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, _, mintErr := c.MintReadOnlyToken(context.Background())
	if mintErr == nil {
		t.Fatal("expected mint to fail on keystore error, got nil")
	}
	if !errors.Is(mintErr, keystore.ErrNotFound) {
		t.Errorf("err chain missing keystore.ErrNotFound: %v", mintErr)
	}
	if !strings.Contains(mintErr.Error(), "read private key from keystore[primary]") {
		t.Errorf("err = %q, want substring %q", mintErr.Error(), "read private key from keystore[primary]")
	}
}
