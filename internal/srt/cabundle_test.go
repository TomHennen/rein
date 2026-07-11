package srt

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// genTestCertPEM returns a freshly generated self-signed CA certificate in PEM
// form — a real parseable cert, not just a CERTIFICATE-typed block.
func genTestCertPEM(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// useSystemStore points SystemCAPath at exactly the given file: SSL_CERT_FILE
// is cleared and the candidate list is swapped for the test's fixture.
func useSystemStore(t *testing.T, path string) {
	t.Helper()
	t.Setenv("SSL_CERT_FILE", "")
	old := systemCABundleCandidates
	systemCABundleCandidates = []string{path}
	t.Cleanup(func() { systemCABundleCandidates = old })
}

func writeFixture(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	return p
}

func TestBuildCABundleEmptySystemStoreFailsClosed(t *testing.T) {
	p := writeFixture(t, "empty.crt", nil)
	useSystemStore(t, p)
	_, err := BuildCABundle(genTestCertPEM(t, "rein test CA"))
	if err == nil {
		t.Fatal("empty system trust store must fail closed, got nil error")
	}
	if !strings.Contains(err.Error(), p) {
		t.Errorf("error should name the offending path %s: %v", p, err)
	}
	if !strings.Contains(err.Error(), "update-ca-certificates") {
		t.Errorf("error should suggest update-ca-certificates: %v", err)
	}
}

func TestBuildCABundleGarbageSystemStoreFailsClosed(t *testing.T) {
	p := writeFixture(t, "garbage.crt", []byte("this is not PEM at all\n"))
	useSystemStore(t, p)
	if _, err := BuildCABundle(genTestCertPEM(t, "rein test CA")); err == nil {
		t.Fatal("garbage (non-PEM) system trust store must fail closed, got nil error")
	}
}

func TestBuildCABundleNonCertPEMSystemStoreFailsClosed(t *testing.T) {
	// A PEM file with no CERTIFICATE block (e.g. a stray key) is still not a
	// trust store.
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("xx")})
	p := writeFixture(t, "key-only.crt", block)
	useSystemStore(t, p)
	if _, err := BuildCABundle(genTestCertPEM(t, "rein test CA")); err == nil {
		t.Fatal("system store with no CERTIFICATE blocks must fail closed, got nil error")
	}
}

func TestBuildCABundleEmptyReinCAFailsClosed(t *testing.T) {
	p := writeFixture(t, "sys.crt", genTestCertPEM(t, "system root"))
	useSystemStore(t, p)
	if _, err := BuildCABundle([]byte("  \n")); err == nil {
		t.Fatal("empty rein CA must fail closed, got nil error")
	}
}

func TestBuildCABundleValid(t *testing.T) {
	sysPEM := genTestCertPEM(t, "system root")
	reinPEM := genTestCertPEM(t, "rein per-run CA")
	p := writeFixture(t, "sys.crt", sysPEM)
	useSystemStore(t, p)

	bundle, err := BuildCABundle(reinPEM)
	if err != nil {
		t.Fatalf("BuildCABundle: %v", err)
	}
	if !bytes.Contains(bundle, bytes.TrimSpace(sysPEM)) {
		t.Error("bundle missing the system root cert")
	}
	if !bytes.Contains(bundle, bytes.TrimSpace(reinPEM)) {
		t.Error("bundle missing the rein CA cert")
	}
	// Both blocks must be parseable certs in the emitted bundle.
	var count int
	rest := bundle
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			t.Errorf("bundle holds an unparseable CERTIFICATE block: %v", err)
		}
		count++
	}
	if count != 2 {
		t.Errorf("bundle has %d CERTIFICATE blocks, want 2 (system + rein)", count)
	}
}

func TestSystemCAProbe(t *testing.T) {
	t.Run("valid store ok", func(t *testing.T) {
		p := writeFixture(t, "sys.crt", genTestCertPEM(t, "system root"))
		useSystemStore(t, p)
		got, err := systemCAProbe()
		if err != nil {
			t.Fatalf("systemCAProbe: %v", err)
		}
		if got != p {
			t.Errorf("systemCAProbe = %s, want %s", got, p)
		}
	})
	t.Run("empty store fails", func(t *testing.T) {
		p := writeFixture(t, "empty.crt", nil)
		useSystemStore(t, p)
		if _, err := systemCAProbe(); err == nil {
			t.Fatal("empty system trust store must fail the probe")
		}
	})
}

func TestSystemCAPathSetButInvalidSSLCertFileFailsLoud(t *testing.T) {
	// A set-but-invalid $SSL_CERT_FILE must be a loud error, not a silent
	// fall-through to the system candidates: the operator pinned their trust
	// source, and widening it on error would be fail-open (security review of
	// #47). The valid system store here proves the fall-through is NOT taken.
	sys := writeFixture(t, "sys.crt", genTestCertPEM(t, "system root"))
	useSystemStore(t, sys)
	bad := writeFixture(t, "bad.crt", []byte("not a certificate"))
	t.Setenv("SSL_CERT_FILE", bad)
	_, err := SystemCAPath()
	if err == nil {
		t.Fatal("set-but-invalid SSL_CERT_FILE must fail loud, got nil error")
	}
	if !strings.Contains(err.Error(), "SSL_CERT_FILE") || !strings.Contains(err.Error(), bad) {
		t.Errorf("error should name SSL_CERT_FILE and the bad path %s: %v", bad, err)
	}
}

func TestSystemCAPathValidSSLCertFileWins(t *testing.T) {
	sys := writeFixture(t, "sys.crt", genTestCertPEM(t, "system root"))
	useSystemStore(t, sys)
	pinned := writeFixture(t, "pinned.crt", genTestCertPEM(t, "pinned root"))
	t.Setenv("SSL_CERT_FILE", pinned)
	got, err := SystemCAPath()
	if err != nil {
		t.Fatalf("SystemCAPath: %v", err)
	}
	if got != pinned {
		t.Errorf("SystemCAPath = %s, want the pinned SSL_CERT_FILE %s", got, pinned)
	}
}
