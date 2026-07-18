// Package classify decides whether a GitHub-bound request needs a READ or a
// WRITE token — the proxy's tier signal in sandboxed mode (design §5.1).
//
// It is deliberately conservative: anything it cannot classify as definitely
// read-only is treated as WRITE, which forces a human approval prompt and a
// write-scoped mint. Under-classifying a write (serving a read token) at worst
// 403s; OVER-classifying (prompting on a read) is merely annoying. So the bias
// is always toward Write. The hard security boundary is still token SCOPE
// (read-tier tokens are minted with zero write permissions); this classifier
// is the defense-in-depth layer above it.
//
// This is also where the direct-mode rein-gh classifier (issue #9) conceptually
// moves: gh's git ops are github.com smart-HTTP, its API ops are
// api.github.com REST/GraphQL — all covered here at the wire.
package classify

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Tier is the token tier a request needs.
type Tier int

const (
	Read Tier = iota
	Write
)

func (t Tier) String() string {
	if t == Write {
		return "write"
	}
	return "read"
}

// Classify returns the tier for a request to a GitHub host, with a short
// reason for logging/audit.
//
//   - host:     request Host (github.com, api.github.com, uploads.github.com…)
//   - method:   HTTP method
//   - path:     request path
//   - rawQuery: request URL raw query (git uses ?service=… on info/refs)
//   - body:     request body bytes — needed ONLY for api.github.com/graphql
//     (query vs mutation). Pass nil for everything else; callers must not
//     buffer large git packs just to classify (git is path-classified).
//
// Fail-closed: unrecognized host/shape ⇒ Write.
func Classify(host, method, path, rawQuery string, body []byte) (Tier, string) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	method = strings.ToUpper(method)

	switch host {
	case "github.com":
		return classifyGit(method, path, rawQuery)
	case "api.github.com":
		return classifyAPI(method, path, body)
	case "uploads.github.com":
		// Release-asset uploads are writes.
		return Write, "uploads.github.com (asset write)"
	default:
		return Write, "unrecognized host (fail-closed)"
	}
}

// classifyGit handles github.com smart-HTTP. The two services are
// git-upload-pack (fetch/clone — read) and git-receive-pack (push — write),
// each reachable as the info/refs advertisement (GET ?service=…) and the
// service POST. Method is NOT the signal — git fetch also POSTs.
func classifyGit(method, path, rawQuery string) (Tier, string) {
	switch {
	case strings.HasSuffix(path, "/git-receive-pack"):
		return Write, "git-receive-pack (push)"
	case strings.HasSuffix(path, "/git-upload-pack"):
		return Read, "git-upload-pack (fetch)"
	case strings.HasSuffix(path, "/info/refs"):
		switch serviceParam(rawQuery) {
		case "git-receive-pack":
			return Write, "info/refs?service=git-receive-pack (push advertisement)"
		case "git-upload-pack":
			return Read, "info/refs?service=git-upload-pack (fetch advertisement)"
		default:
			return Write, "info/refs with unknown/absent service (fail-closed)"
		}
	default:
		return Write, "unrecognized github.com path (fail-closed)"
	}
}

// classifyAPI handles api.github.com: REST by method, GraphQL by body.
func classifyAPI(method, path string, body []byte) (Tier, string) {
	if IsGraphQLPath(path) {
		return classifyGraphQL(body)
	}
	switch method {
	case "GET", "HEAD", "OPTIONS":
		return Read, "REST " + method + " (read)"
	case "POST", "PUT", "PATCH", "DELETE":
		return Write, "REST " + method + " (write)"
	default:
		return Write, "REST unknown method (fail-closed)"
	}
}

// IsGraphQLPath reports whether path is the GraphQL endpoint (body-classified).
// Exported as the single source of truth: internal/proxy calls this rather than
// keeping its own copy, so the graphql gate can't drift between the two.
func IsGraphQLPath(path string) bool {
	p := strings.TrimSuffix(path, "/")
	return p == "/graphql" || p == "/api/graphql"
}

// classifyGraphQL inspects a GraphQL POST body. Conservative: only an
// unambiguously query-only document is Read; the presence of any mutation or
// subscription operation — or any failure to parse/peek — is Write.
func classifyGraphQL(body []byte) (Tier, string) {
	if len(body) == 0 {
		return Write, "graphql with empty body (fail-closed)"
	}
	var payload struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Query == "" {
		return Write, "graphql body unparseable (fail-closed)"
	}
	stripped := stripGraphQL(payload.Query)
	if mutationOrSubscription.MatchString(stripped) {
		return Write, "graphql mutation/subscription"
	}
	return Read, "graphql query-only"
}

// mutationOrSubscription matches a top-level operation keyword. After
// stripGraphQL removes strings and comments, a literal `mutation`/`subscription`
// keyword can only be an operation definition (GraphQL forbids those words as
// the leading token elsewhere). Word-boundaried to avoid matching e.g. a field
// named "mutationCount".
var mutationOrSubscription = regexp.MustCompile(`\b(mutation|subscription)\b`)

// stripGraphQL removes string/block-string literals and # comments so the
// keyword scan can't be fooled by a query string that merely CONTAINS the word
// "mutation" inside a literal or comment.
//
// Every prefix test is a fixed-width rune-index check, never string(runes[i:]):
// the latter reallocates the whole remaining slice at each position, making the
// scan O(n^2) on the NORMAL path (the block-string case was tested at every
// character). Since the body can be up to 1 MiB (proxy.go maxGraphQL) and comes
// from a possibly-injected agent, that was a CPU-DoS; the rune-index form is
// linear and byte-for-byte equivalent (issue #136A).
func stripGraphQL(q string) string {
	var b strings.Builder
	runes := []rune(q)
	tripleQuote := func(i int) bool {
		return i+2 < len(runes) && runes[i] == '"' && runes[i+1] == '"' && runes[i+2] == '"'
	}
	for i := 0; i < len(runes); {
		switch {
		case runes[i] == '#': // comment to end of line
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
		case tripleQuote(i): // block string
			i += 3
			for i < len(runes) && !tripleQuote(i) {
				i++
			}
			i += 3
		case runes[i] == '"': // ordinary string
			i++
			for i < len(runes) && runes[i] != '"' {
				if runes[i] == '\\' && i+1 < len(runes) {
					i++
				}
				i++
			}
			i++
		default:
			b.WriteRune(runes[i])
			i++
		}
	}
	return b.String()
}

// serviceParam extracts the value of service= from a raw query string without
// a full url.ParseQuery (the value is a fixed token, no escaping).
func serviceParam(rawQuery string) string {
	for _, kv := range strings.Split(rawQuery, "&") {
		if v, ok := strings.CutPrefix(kv, "service="); ok {
			return v
		}
	}
	return ""
}
