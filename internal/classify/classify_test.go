package classify

import "testing"

func TestClassify_Git(t *testing.T) {
	cases := []struct {
		name             string
		method, path, rq string
		want             Tier
	}{
		{"receive-pack POST", "POST", "/o/r/git-receive-pack", "", Write},
		{"upload-pack POST", "POST", "/o/r/git-upload-pack", "", Read},
		{"push advertisement", "GET", "/o/r/info/refs", "service=git-receive-pack", Write},
		{"fetch advertisement", "GET", "/o/r/info/refs", "service=git-upload-pack", Read},
		{"info/refs no service (fail-closed)", "GET", "/o/r/info/refs", "", Write},
		{"info/refs junk service (fail-closed)", "GET", "/o/r/info/refs", "service=evil", Write},
		{"unknown github.com path (fail-closed)", "GET", "/o/r/blob/main/x", "", Write},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := Classify("github.com", c.method, c.path, c.rq, nil)
			if got != c.want {
				t.Errorf("Classify(git %s %s?%s) = %v (%s), want %v", c.method, c.path, c.rq, got, reason, c.want)
			}
		})
	}
}

func TestClassify_REST(t *testing.T) {
	reads := []string{"GET", "HEAD", "OPTIONS"}
	writes := []string{"POST", "PUT", "PATCH", "DELETE", "WEIRD"}
	for _, m := range reads {
		if got, _ := Classify("api.github.com", m, "/repos/o/r", "", nil); got != Read {
			t.Errorf("REST %s = %v, want Read", m, got)
		}
	}
	for _, m := range writes {
		if got, _ := Classify("api.github.com", m, "/repos/o/r", "", nil); got != Write {
			t.Errorf("REST %s = %v, want Write (fail-closed for unknown)", m, got)
		}
	}
}

func TestClassify_GraphQL(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Tier
	}{
		{"simple query", `{"query":"query { viewer { login } }"}`, Read},
		{"anonymous query", `{"query":"{ viewer { login } }"}`, Read},
		{"mutation", `{"query":"mutation { addStar(input:{}) { clientMutationId } }"}`, Write},
		{"subscription (fail-closed)", `{"query":"subscription { x }"}`, Write},
		{"mutation hidden in string literal is still a query", `{"query":"query { search(query: \"mutation foo\") { x } }"}`, Read},
		{"mutation in a # comment is still a query", `{"query":"# run a mutation later\nquery { viewer { login } }"}`, Read},
		{"mutation in a block string is still a query", "{\"query\":\"query { f(arg: \\\"\\\"\\\"mutation\\\"\\\"\\\") }\"}", Read},
		{"real mutation after a comment", `{"query":"# comment\nmutation { f }"}`, Write},
		{"empty body (fail-closed)", ``, Write},
		{"malformed JSON (fail-closed)", `{not json`, Write},
		{"no query field (fail-closed)", `{"variables":{}}`, Write},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := Classify("api.github.com", "POST", "/graphql", "", []byte(c.body))
			if got != c.want {
				t.Errorf("graphql %q = %v (%s), want %v", c.body, got, reason, c.want)
			}
		})
	}
}

func TestClassify_HostsAndFailClosed(t *testing.T) {
	if got, _ := Classify("uploads.github.com", "POST", "/repos/o/r/releases/1/assets", "", nil); got != Write {
		t.Errorf("uploads.github.com = %v, want Write", got)
	}
	if got, _ := Classify("evil.example.com", "GET", "/", "", nil); got != Write {
		t.Errorf("unknown host = %v, want Write (fail-closed)", got)
	}
	// Host normalization: case + trailing dot.
	if got, _ := Classify("API.GitHub.com.", "GET", "/repos/o/r", "", nil); got != Read {
		t.Errorf("normalized host GET = %v, want Read", got)
	}
}
