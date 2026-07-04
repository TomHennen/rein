// Package proxy is the TLS-terminating, credential-injecting MITM that
// backs sandboxed mode (Phase 1, design §4). It productizes the CP1 spike
// relay (docs/phase1-srt-spike-findings.md "CP1 results") into a per-session
// unix-socket proxy: terminate TLS with a rein-CA leaf for the SNI host, read
// HTTP/1.1 requests, classify the tier (internal/classify), get a credential
// decision (internal/brokercore), inject per host class (design §4.3), and
// relay to the real upstream — streaming the response back verbatim.
//
// The proxy owns none of the mint/scope/approval brains; those live in
// brokercore.Core (shared with direct mode). The proxy's job is the network
// boundary: identity (SNI==Host), host-class injection, the relay hygiene the
// CP1 recipe pins, and never letting a real token onto the response path.
package proxy

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/TomHennen/rein/internal/keystore"
)

// caEntryName is the keystore entry under which the CA's combined cert+key
// PEM is stored. The private key is read ONLY through the keystore (CLAUDE.md
// hard constraint #6); we store the cert alongside it (in the same PEM) so a
// restarted daemon reuses the SAME certificate — see LoadOrCreateCA.
const caEntryName = "proxy-ca"

// caValidity is how long a freshly-minted CA is valid. ~2 years (design §5.4).
const caValidity = 2 * 365 * 24 * time.Hour

// leafValidity bounds a per-host leaf. Leaves are cached in daemon memory for
// the process lifetime, so this only needs to comfortably exceed a run; keep
// it short so a leaked leaf (it never leaves the daemon) ages out quickly.
const leafValidity = 90 * 24 * time.Hour

// CA is rein's local certificate authority for the proxy. It mints a leaf per
// SNI host on demand (cached in memory) so one listener serves github.com,
// api.github.com, etc. off a single root. The root's private key lives only in
// the keystore and this process's memory; it is delivered as trust to the
// sandbox via env vars (design §5.4), never to the host trust store.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certDER []byte
	certPEM []byte

	leafMu sync.Mutex
	leaves map[string]*tls.Certificate
}

// LoadOrCreateCA returns the CA stored in ks, generating and persisting a new
// one on first use (keystore.ErrNotFound).
//
// The cert is persisted WITH the key (combined PEM, one entry) rather than
// regenerated from the key on each start: an X.509 cert embeds a serial and
// NotBefore/NotAfter, so regenerating would yield a DIFFERENT cert each run —
// and CP3 exports this cert as the sandbox's trust anchor, which must keep
// validating the leaves a restarted daemon serves. Persist-and-reuse.
func LoadOrCreateCA(ks keystore.Keystore) (*CA, error) {
	if ks == nil {
		return nil, fmt.Errorf("proxy: keystore is required for CA")
	}
	pemBytes, err := ks.Get(caEntryName)
	switch {
	case err == nil:
		return parseCA(pemBytes)
	case err == keystore.ErrNotFound:
		return createCA(ks)
	default:
		return nil, fmt.Errorf("proxy: read CA from keystore: %w", err)
	}
}

// createCA generates a fresh CA, persists the combined cert+key PEM through
// the keystore, and returns it.
func createCA(ks keystore.Keystore) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("proxy: generate CA key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "rein local CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("proxy: create CA cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("proxy: marshal CA key: %w", err)
	}
	var buf bytes.Buffer
	if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return nil, fmt.Errorf("proxy: encode CA cert PEM: %w", err)
	}
	if err := pem.Encode(&buf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		return nil, fmt.Errorf("proxy: encode CA key PEM: %w", err)
	}
	if err := ks.Set(caEntryName, buf.Bytes()); err != nil {
		return nil, fmt.Errorf("proxy: persist CA to keystore: %w", err)
	}
	return parseCA(buf.Bytes())
}

// parseCA decodes a combined cert+key PEM into a CA. It requires both a
// CERTIFICATE and an EC PRIVATE KEY block; a partial entry fails closed.
func parseCA(pemBytes []byte) (*CA, error) {
	var certDER, keyDER []byte
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch block.Type {
		case "CERTIFICATE":
			certDER = block.Bytes
		case "EC PRIVATE KEY":
			keyDER = block.Bytes
		}
	}
	if certDER == nil || keyDER == nil {
		return nil, fmt.Errorf("proxy: CA PEM missing CERTIFICATE or EC PRIVATE KEY block")
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("proxy: parse CA cert: %w", err)
	}
	key, err := x509.ParseECPrivateKey(keyDER)
	if err != nil {
		return nil, fmt.Errorf("proxy: parse CA key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	return &CA{
		cert:    cert,
		key:     key,
		certDER: certDER,
		certPEM: certPEM,
		leaves:  map[string]*tls.Certificate{},
	}, nil
}

// CertPEM returns the CA certificate in PEM form (no private key). CP3 writes
// this into the sandbox trust bundle (system roots + this) so in-sandbox
// clients trust the proxy's leaves. Safe to expose — it is not secret.
func (c *CA) CertPEM() []byte {
	out := make([]byte, len(c.certPEM))
	copy(out, c.certPEM)
	return out
}

// getLeaf returns a leaf certificate for the SNI host, minting and caching one
// on first request. It is the tls.Config.GetCertificate callback. A missing
// SNI (host == "") is served a leaf CN'd "unknown"; the request is rejected
// later by the SNI==Host check, so this only needs to complete the handshake.
func (c *CA) getLeaf(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := hello.ServerName
	if host == "" {
		host = "unknown"
	}
	c.leafMu.Lock()
	defer c.leafMu.Unlock()
	if cert, ok := c.leaves[host]; ok {
		return cert, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{Certificate: [][]byte{der, c.certDER}, PrivateKey: key}
	// Only CACHE leaves for hosts we actually serve. An in-sandbox client can
	// open connections with arbitrary SNI strings; caching each would let it
	// grow the daemon's memory unbounded. Unknown-host connections still get a
	// leaf (so the handshake completes and the HTTP layer can return a clean
	// 403), it just isn't retained. The known set is tiny (design §4.3).
	if cacheableLeafHost(host) {
		c.leaves[host] = cert
	}
	return cert, nil
}

// cacheableLeafHost reports whether a leaf for host should be retained. True
// only for the GitHub host set the proxy serves (inject + never-inject
// classes), so the leaf cache stays bounded regardless of client SNI.
func cacheableLeafHost(host string) bool {
	return classifyHost(host) != classRefuse
}

// leafCached reports whether a leaf for host is already in the in-memory cache.
// Exposed for tests asserting the "second connection reuses the leaf" property.
func (c *CA) leafCached(host string) bool {
	c.leafMu.Lock()
	defer c.leafMu.Unlock()
	_, ok := c.leaves[host]
	return ok
}

// randSerial returns a positive 128-bit random certificate serial.
func randSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("proxy: random serial: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil
}
