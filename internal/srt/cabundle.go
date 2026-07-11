package srt

import (
	"bytes"
	"encoding/pem"
	"fmt"
	"os"
)

// systemCABundleCandidates are the common on-disk system CA bundle paths, in
// preference order. rein reads system roots from a FILE (not crypto/x509's
// SystemCertPool, which returns an opaque pool with no way to re-emit PEM) so it
// can concatenate them with rein's CA into a single bundle the sandboxed clients
// trust. The bundle must include system roots: CDN hosts (passthrough) get
// direct TLS with GitHub's real cert, which a rein-only bundle would reject.
var systemCABundleCandidates = []string{
	"/etc/ssl/certs/ca-certificates.crt", // Debian/Ubuntu/Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",   // Fedora/RHEL/CentOS
	"/etc/ssl/ca-bundle.pem",             // OpenSUSE
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/ssl/cert.pem", // Alpine/macOS-brew
}

// SystemCAPath returns the first existing system CA bundle path. SSL_CERT_FILE
// in rein's OWN environment (if set) wins, matching how OpenSSL clients resolve
// it. Fails closed (error) if none is found — a bundle without system roots
// would break the CDN passthrough path, so guessing is not acceptable.
func SystemCAPath() (string, error) {
	// $SSL_CERT_FILE wins — but only if it actually holds CA material. An empty
	// or garbage file that merely Stats would produce a bundle with no system
	// roots (breaking CDN passthrough) or no CA at all; validate it holds at
	// least one PEM CERTIFICATE block before trusting it, else fall through.
	if p := os.Getenv("SSL_CERT_FILE"); p != "" {
		if containsPEMCertificate(p) {
			return p, nil
		}
	}
	for _, p := range systemCABundleCandidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no system CA bundle found (looked in %v and $SSL_CERT_FILE); cannot build a trust bundle that keeps CDN hosts working", systemCABundleCandidates)
}

// containsPEMCertificate reports whether the file at path is readable and holds
// at least one PEM "CERTIFICATE" block. Used to reject an empty/garbage
// $SSL_CERT_FILE before trusting it as the system CA source.
func containsPEMCertificate(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return pemHasCertificate(data)
}

// pemHasCertificate reports whether data holds at least one PEM "CERTIFICATE"
// block.
func pemHasCertificate(data []byte) bool {
	for len(data) > 0 {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			return true
		}
	}
	return false
}

// errNoSystemCerts is the fail-closed verdict for a system trust store that
// exists but holds no PEM certificates (real failure modes: broken container
// image, botched update-ca-certificates). Shared by BuildCABundle (launch
// path) and systemCAProbe (preflight/doctor) so both name the same fix.
func errNoSystemCerts(path string) error {
	return fmt.Errorf("system CA bundle %s contains no PEM certificates (empty or corrupt trust store). "+
		"Refusing to build the sandbox bundle: SSL_CERT_FILE REPLACES the default roots in-sandbox, so a bundle "+
		"without system roots would break every allowed non-GitHub HTTPS host (including the agent's own API endpoint). "+
		"Repair the system store (Debian/Ubuntu: `sudo update-ca-certificates`) or point $SSL_CERT_FILE at a valid bundle", path)
}

// systemCAProbe resolves the system CA bundle path AND validates it holds at
// least one PEM certificate. This is the preflight/doctor view of the same
// fail-closed gate BuildCABundle enforces at launch.
func systemCAProbe() (string, error) {
	path, err := SystemCAPath()
	if err != nil {
		return "", err
	}
	if !containsPEMCertificate(path) {
		return "", errNoSystemCerts(path)
	}
	return path, nil
}

// BuildCABundle returns system roots concatenated with reinCAPEM, ready to write
// to the per-run bundle file. The rein CA is appended AFTER the system roots so
// both are present; PEM concatenation order does not affect trust.
func BuildCABundle(reinCAPEM []byte) ([]byte, error) {
	if len(bytes.TrimSpace(reinCAPEM)) == 0 {
		return nil, fmt.Errorf("BuildCABundle: rein CA PEM is empty; refusing to build a bundle that omits the MITM CA")
	}
	sysPath, err := SystemCAPath()
	if err != nil {
		return nil, err
	}
	sys, err := os.ReadFile(sysPath)
	if err != nil {
		return nil, fmt.Errorf("read system CA bundle %s: %w", sysPath, err)
	}
	// Fail closed on an existing-but-empty or garbage system store (#47): a
	// bundle holding only the rein CA would silently break the CDN passthrough
	// path and the agent's own API endpoint in-sandbox.
	if !pemHasCertificate(sys) {
		return nil, errNoSystemCerts(sysPath)
	}
	var buf bytes.Buffer
	buf.Write(sys)
	if len(sys) > 0 && sys[len(sys)-1] != '\n' {
		buf.WriteByte('\n')
	}
	buf.WriteString("# --- rein per-run MITM CA (injecting proxy) ---\n")
	buf.Write(reinCAPEM)
	if len(reinCAPEM) > 0 && reinCAPEM[len(reinCAPEM)-1] != '\n' {
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}
