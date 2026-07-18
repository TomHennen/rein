package sandboxutil

// CAEnvVars are the four CA-trust variables every sandboxed launch points at its
// per-run CA bundle (system roots + rein's MITM CA), so tooling trusts the proxy's
// leaf on the inject path and real certs on passthrough. All point at one bundle.
var CAEnvVars = []string{
	"SSL_CERT_FILE",       // openssl / git (OpenSSL build) / python
	"GIT_SSL_CAINFO",      // git explicitly
	"NODE_EXTRA_CA_CERTS", // node-based tooling
	"CURL_CA_BUNDLE",      // curl / libcurl
}
