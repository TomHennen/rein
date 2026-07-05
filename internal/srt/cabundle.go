package srt

import (
	"bytes"
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
	if p := os.Getenv("SSL_CERT_FILE"); p != "" {
		if _, err := os.Stat(p); err == nil {
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

// BuildCABundle returns system roots concatenated with reinCAPEM, ready to write
// to the per-run bundle file. The rein CA is appended AFTER the system roots so
// both are present; PEM concatenation order does not affect trust.
func BuildCABundle(reinCAPEM []byte) ([]byte, error) {
	sysPath, err := SystemCAPath()
	if err != nil {
		return nil, err
	}
	sys, err := os.ReadFile(sysPath)
	if err != nil {
		return nil, fmt.Errorf("read system CA bundle %s: %w", sysPath, err)
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
