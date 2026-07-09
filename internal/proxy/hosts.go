package proxy

import (
	"strings"

	"github.com/TomHennen/rein/internal/brokercore"
)

// InjectHosts are the exact GitHub hosts rein TLS-terminates and injects a
// credential into (design §4.3): the git smart-HTTP host and the two REST/
// upload API hosts. These — and ONLY these — go in srt's mitmProxy.domains
// (gap #6: mitmProxy.domains must be exact hosts, never a wildcard, or a CDN
// host gets pulled into the injector). Kept next to classifyHost so the two
// never drift: every entry here must classify as an inject class below.
var InjectHosts = []string{
	"github.com",
	"api.github.com",
	"uploads.github.com",
}

// CDNHosts are the GitHub asset/CDN hosts the agent reaches via tokenized
// redirects from github.com. They are allowed egress (in srt's allowedDomains)
// but are NEVER injected into — they get direct TLS with GitHub's real cert
// (classPassthrough). They must NOT appear in mitmProxy.domains.
var CDNHosts = []string{
	"codeload.github.com",
	"objects.githubusercontent.com",
	"raw.githubusercontent.com",
}

// hostClass is the injection treatment for a GitHub host (design §4.3).
type hostClass int

const (
	// classRefuse: not a GitHub host we intercept — fail closed.
	classRefuse hostClass = iota
	// classInjectBearer: api.github.com, uploads.github.com — inject Bearer.
	classInjectBearer
	// classInjectBasic: github.com git smart-HTTP — inject Basic x-access-token.
	classInjectBasic
	// classPassthrough: CDN / asset hosts — relay egress, NEVER inject.
	classPassthrough
)

// classifyHost maps an SNI host to its injection treatment (design §4.3 table).
// The never-inject CDN hosts are reached via tokenized redirects from
// github.com; injecting there would leak the token to an S3-style pre-signed
// URL and can break the request. Any other host fails closed.
func classifyHost(host string) hostClass {
	switch strings.ToLower(strings.TrimSuffix(host, ".")) {
	case "api.github.com", "uploads.github.com":
		return classInjectBearer
	case "github.com":
		return classInjectBasic
	case "objects.githubusercontent.com", "codeload.github.com", "raw.githubusercontent.com":
		return classPassthrough
	default:
		return classRefuse
	}
}

// requestRepo derives the "owner/repo" a request targets, for the scope check.
// It is best-effort defense-in-depth — the minted token's repo scope is the
// hard backstop — so an empty result (unknown repo) is safe (EmptyPathScope
// governs, default allow).
//
//   - github.com: the git smart-HTTP path is /owner/repo(.git)/info/refs etc.
//     brokercore.RepoFromPath extracts owner/repo; Contains normalizes the
//     trailing ".git".
//   - api/uploads.github.com: the repo lives at /repos/{owner}/{repo}/… for
//     repo-scoped endpoints; other endpoints (/user, /graphql, /orgs) have no
//     repo and return "".
func requestRepo(host, path string) string {
	switch strings.ToLower(strings.TrimSuffix(host, ".")) {
	case "github.com":
		return brokercore.RepoFromPath(path)
	case "api.github.com", "uploads.github.com":
		return repoFromRESTPath(path)
	default:
		return ""
	}
}

func repoFromRESTPath(path string) string {
	p := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(p, "/", 4)
	if len(parts) >= 3 && parts[0] == "repos" && parts[1] != "" && parts[2] != "" {
		return parts[1] + "/" + parts[2]
	}
	return ""
}
