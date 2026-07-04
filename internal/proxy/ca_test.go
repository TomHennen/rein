package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/TomHennen/rein/internal/keystore"
)

func TestCARoundTripReusesCert(t *testing.T) {
	ks := keystore.NewFileKeystore(t.TempDir())

	ca1, err := LoadOrCreateCA(ks)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A second load from the same keystore must return the SAME cert (persist-
	// and-reuse), not a freshly generated one — CP3 exports this cert as the
	// sandbox trust anchor and a restarted daemon must keep validating its
	// leaves.
	ca2, err := LoadOrCreateCA(ks)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if string(ca1.CertPEM()) != string(ca2.CertPEM()) {
		t.Fatalf("CA cert changed across reloads; not persisted")
	}
	if ca1.cert.SerialNumber.Cmp(ca2.cert.SerialNumber) != 0 {
		t.Fatalf("CA serial changed across reloads")
	}
}

func TestCACertPEMIsCAOnly(t *testing.T) {
	ks := keystore.NewFileKeystore(t.TempDir())
	ca, err := LoadOrCreateCA(ks)
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(ca.CertPEM())
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("CertPEM not a CERTIFICATE block")
	}
	if len(rest) != 0 {
		t.Fatalf("CertPEM contains extra blocks (key must NOT be exported)")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !cert.IsCA {
		t.Errorf("exported cert is not a CA")
	}
}

func TestLeafCacheCaseInsensitive(t *testing.T) {
	ks := keystore.NewFileKeystore(t.TempDir())
	ca, err := LoadOrCreateCA(ks)
	if err != nil {
		t.Fatal(err)
	}
	// Case variants of an allowed host must collapse to ONE cache entry — else
	// an in-sandbox client enumerates variants to grow memory unbounded.
	a, _ := ca.getLeaf(&tls.ClientHelloInfo{ServerName: "GitHub.com"})
	b, _ := ca.getLeaf(&tls.ClientHelloInfo{ServerName: "github.com"})
	if a != b {
		t.Errorf("case-variant SNI produced distinct cached leaves")
	}
	if len(ca.leaves) != 1 {
		t.Errorf("leaf cache size = %d, want 1", len(ca.leaves))
	}
	// An unknown host is served a leaf but NOT cached (bounded).
	ca.getLeaf(&tls.ClientHelloInfo{ServerName: "evil.example.com"})
	if len(ca.leaves) != 1 {
		t.Errorf("unknown host was cached; leaf cache size = %d, want 1", len(ca.leaves))
	}
}

func TestLeafSignedByCA(t *testing.T) {
	ks := keystore.NewFileKeystore(t.TempDir())
	ca, err := LoadOrCreateCA(ks)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.getLeaf(&tls.ClientHelloInfo{ServerName: "github.com"})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("append CA")
	}
	leafCert, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leafCert.Verify(x509.VerifyOptions{DNSName: "github.com", Roots: roots}); err != nil {
		t.Fatalf("leaf does not verify against the CA: %v", err)
	}
}
