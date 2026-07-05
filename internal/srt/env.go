package srt

import (
	"sort"
	"strings"
)

// EnvParams are the inputs to BuildEnv.
type EnvParams struct {
	// Parent is the environment to draw the passthrough vars FROM (normally
	// os.Environ()). Only the allowlisted names are carried over; everything
	// else — including secrets like ANTHROPIC_API_KEY, AWS_*, GITHUB_TOKEN,
	// SSH_AUTH_SOCK, DBUS_SESSION_BUS_ADDRESS — is dropped.
	Parent []string

	// CABundlePath is the per-run CA bundle (system roots + rein CA). It is
	// pointed at by all four CA env vars so git/curl/node/openssl trust rein's
	// MITM leaf on the inject path while still trusting real certs on the CDN
	// (passthrough) path.
	CABundlePath string

	// StubGHToken is the value for GH_TOKEN inside the sandbox. It is a
	// non-secret placeholder: real gh/git auth is injected by rein's proxy, so
	// the agent never needs a real token, but tools that branch on GH_TOKEN
	// presence behave. Must be non-empty (an empty GH_TOKEN can make gh prompt
	// or fall back to keyring).
	StubGHToken string
}

// passthroughExact is the allowlist of environment variable NAMES carried from
// the parent unchanged. This is the strict-allowlist gap (#1): srt cannot
// express "unset all but these" (its envVars are a per-name denylist), so rein
// execs srt under an explicit env built here. The load-bearing property tested
// in env_test.go is that NO name outside this set (plus the CA vars + GH_TOKEN
// set below) survives — so a secret in the parent env can never reach the agent.
//
// PATH is REQUIRED (srt's whichSync needs bwrap/socat/rg/bash). HOME/LANG are
// needed by most tooling. TERM is a usability addition for the interactive
// agent path (a terminal type is not a secret); dropping it only degrades TUI
// rendering, so it is included deliberately, not by oversight.
var passthroughExact = map[string]bool{
	"PATH": true,
	"HOME": true,
	"LANG": true,
	"TERM": true,
}

// passthroughPrefix carries any parent var whose name starts with one of these
// prefixes (locale settings: LC_ALL, LC_CTYPE, LC_TIME, …). Prefix rather than
// enumerate because the LC_* set is open-ended and none of them are secrets.
var passthroughPrefix = []string{
	"LC_",
}

// caEnvVars are the four CA-trust variables. On the mitmProxy path srt sets NO
// CA vars (mitmCA is undefined), so rein must point every client's trust store
// at the bundle itself. All four point at the same bundle file.
var caEnvVars = []string{
	"SSL_CERT_FILE",       // openssl / git (OpenSSL build) / python
	"GIT_SSL_CAINFO",      // git explicitly
	"NODE_EXTRA_CA_CERTS", // node-based tooling
	"CURL_CA_BUNDLE",      // curl / libcurl
}

// BuildEnv returns the explicit environment slice ("KEY=VALUE") for exec.Cmd.Env
// on the srt launch. It is an allowlist, not a filter of the parent: the result
// contains ONLY the passthrough vars present in Parent, the four CA vars, and
// the stub GH_TOKEN. The output is sorted for deterministic tests and logs.
//
// Explicitly NOT propagated (even if set in Parent): HTTP_PROXY/HTTPS_PROXY/
// NO_PROXY/TMPDIR (srt owns those), and every secret-bearing var. This is the
// single most valuable gap-closure in CP3 (gap #1).
func BuildEnv(p EnvParams) []string {
	out := make([]string, 0, len(passthroughExact)+len(caEnvVars)+1)

	for _, kv := range p.Parent {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if allowedEnvName(name) {
			out = append(out, kv)
		}
	}
	for _, name := range caEnvVars {
		out = append(out, name+"="+p.CABundlePath)
	}
	out = append(out, "GH_TOKEN="+p.StubGHToken)

	sort.Strings(out)
	return out
}

// allowedEnvName reports whether a parent env var name is on the passthrough
// allowlist. The CA vars and GH_TOKEN are set explicitly by BuildEnv (not
// carried from the parent), so they are NOT allowlisted here — a stale value in
// the parent must not shadow the value rein sets.
func allowedEnvName(name string) bool {
	if passthroughExact[name] {
		return true
	}
	for _, pre := range passthroughPrefix {
		if strings.HasPrefix(name, pre) {
			return true
		}
	}
	return false
}
