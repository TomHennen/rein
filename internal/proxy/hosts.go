package proxy

import (
	"strings"

	"github.com/TomHennen/rein/internal/brokercore"
)

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

// isGraphQLPath reports whether path is the GraphQL endpoint (body-classified).
func isGraphQLPath(path string) bool {
	p := strings.TrimSuffix(path, "/")
	return p == "/graphql" || p == "/api/graphql"
}
