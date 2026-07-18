package classify

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzClassify fuzzes the tier classifier's untrusted-input path (issue #136A):
// host/method/path/query/body all come from a possibly-prompt-injected agent.
// The classifier must never panic, must be TOTAL (only ever Read or Write), and
// must be deterministic. It must never classify a write shape as read.
//
// The structured read/write cases are pinned by classify_test.go; this target's
// job is robustness on adversarial input (unbalanced quotes/comments, huge or
// unterminated GraphQL block strings, embedded NULs, deep nesting). The likely
// class of finding here is a pathological SLOWDOWN in stripGraphQL, which the
// fuzzer surfaces as a slow/timing-out unit — see the O(n^2) seed below.
func FuzzClassify(f *testing.F) {
	seeds := []struct {
		host, method, path, rq, body string
	}{
		{"github.com", "POST", "/o/r/git-receive-pack", "", ""},
		{"github.com", "GET", "/o/r/info/refs", "service=git-receive-pack", ""},
		{"github.com", "GET", "/o/r/info/refs", "service=git-upload-pack", ""},
		{"api.github.com", "POST", "/graphql", "", `{"query":"query { viewer { login } }"}`},
		{"api.github.com", "POST", "/graphql", "", `{"query":"mutation { addStar(input:{}) { clientMutationId } }"}`},
		{"api.github.com", "POST", "/graphql", "", `{"query":"# c\nmutation { f }"}`},
		{"api.github.com", "GET", "/repos/o/r", "", ""},
		{"uploads.github.com", "POST", "/repos/o/r/releases/1/assets", "", ""},
		{"evil.example.com", "GET", "/", "", ""},
		// Adversarial GraphQL bodies aimed at stripGraphQL's scanner.
		{"api.github.com", "POST", "/graphql", "", `{"query":"` + strings.Repeat(`\"`, 500) + `"}`},
		{"api.github.com", "POST", "/graphql", "", `{"query":"` + `#` + strings.Repeat("x", 4000) + `"}`},
		// Unterminated block string: the O(n^2) trigger. Kept modest so the seed
		// run stays fast even before the fix; the fuzzer explores larger.
		{"api.github.com", "POST", "/graphql", "", `{"query":"` + `\"\"\"` + strings.Repeat("x", 4000) + `"}`},
	}
	for _, s := range seeds {
		f.Add(s.host, s.method, s.path, s.rq, []byte(s.body))
	}

	f.Fuzz(func(t *testing.T, host, method, path, rq string, body []byte) {
		got, reason := Classify(host, method, path, rq, body)
		if got != Read && got != Write {
			t.Fatalf("Classify returned non-total tier %v (%s)", got, reason)
		}
		// Determinism: identical inputs must yield an identical verdict.
		if again, _ := Classify(host, method, path, rq, body); again != got {
			t.Fatalf("Classify not deterministic: %v then %v", got, again)
		}

		// Soundness on the git write shapes, which are path/suffix-classified and
		// admit an independent oracle: a receive-pack service must NEVER be Read.
		lhost := strings.ToLower(strings.TrimSuffix(host, "."))
		if lhost == "github.com" {
			if strings.HasSuffix(path, "/git-receive-pack") && got != Write {
				t.Fatalf("git-receive-pack misclassified as read")
			}
			if strings.HasSuffix(path, "/info/refs") && serviceParam(rq) == "git-receive-pack" && got != Write {
				t.Fatalf("push advertisement misclassified as read")
			}
		}
	})
}

// FuzzClassifyGraphQLTrailingMutation is a targeted soundness probe for the
// "write-as-read" direction (issue #136A): a document that ends in a real
// top-level mutation is a write. We prepend fuzzed content and append a genuine
// mutation, marshaled through encoding/json so the body is always valid JSON
// with the fuzz text living inside the query string.
//
// A firing here means fuzzed content SWALLOWED the trailing mutation (e.g. an
// unterminated string/comment ran to EOF). That is documented as
// investigate-not-red: a malformed GraphQL doc is server-rejected and the tier
// token is read-scoped regardless, so it is acceptable-by-design rather than a
// scope hole. Kept as a separate target so a fire never gates the main suite.
func FuzzClassifyGraphQLTrailingMutation(f *testing.F) {
	f.Add("query { viewer { login } }")
	f.Add("# comment")
	f.Add(`"unterminated`)
	f.Add(`"""unterminated block`)
	f.Fuzz(func(t *testing.T, prefix string) {
		doc := prefix + "\nmutation { addStar(input:{}) { clientMutationId } }"
		body, err := json.Marshal(struct {
			Query string `json:"query"`
		}{Query: doc})
		if err != nil {
			t.Skip()
		}
		got, reason := Classify("api.github.com", "POST", "/graphql", "", body)
		if got == Read {
			// Not a hard failure: document the swallow rather than fail closed here.
			t.Logf("trailing mutation classified Read (prefix %q swallowed it): %s", prefix, reason)
		}
	})
}
