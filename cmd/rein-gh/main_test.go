package main

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want string
	}{
		// Empty/help.
		{"no args", nil, "unknown"},
		{"--version", []string{"--version"}, "unknown"},
		{"--help", []string{"--help"}, "unknown"},

		// Read commands.
		{"repo view", []string{"repo", "view"}, "read"},
		{"issue list", []string{"issue", "list"}, "read"},
		{"issue view", []string{"issue", "view", "123"}, "read"},
		{"pr view", []string{"pr", "view"}, "read"},
		{"pr diff", []string{"pr", "diff", "42"}, "read"},
		{"pr status", []string{"pr", "status"}, "read"},
		{"workflow list", []string{"workflow", "list"}, "read"},
		{"run watch (read-only polling)", []string{"run", "watch", "111"}, "read"},
		{"release list", []string{"release", "list"}, "read"},

		// Write commands.
		{"issue create", []string{"issue", "create", "--title", "x"}, "write"},
		{"issue comment", []string{"issue", "comment", "1", "--body", "hi"}, "write"},
		{"issue close", []string{"issue", "close", "1"}, "write"},
		{"pr create", []string{"pr", "create", "--title", "x"}, "write"},
		{"pr merge", []string{"pr", "merge", "42", "--squash"}, "write"},
		{"pr review (any args)", []string{"pr", "review", "42", "--approve"}, "write"},
		{"release create", []string{"release", "create", "v1.0"}, "write"},
		{"workflow run", []string{"workflow", "run", "ci.yml"}, "write"},
		{"repo edit", []string{"repo", "edit", "--description", "x"}, "write"},
		{"repo sync", []string{"repo", "sync"}, "write"},
		{"secret set", []string{"secret", "set", "FOO", "--body", "x"}, "write"},
		{"variable set", []string{"variable", "set", "BAR", "--body", "x"}, "write"},
		{"run rerun", []string{"run", "rerun", "111"}, "write"},
		{"run cancel", []string{"run", "cancel", "111"}, "write"},

		// gh api defaults to GET (read).
		{"api GET", []string{"api", "/repos/foo/bar"}, "read"},
		{"api explicit GET", []string{"api", "/repos/foo/bar", "-X", "GET"}, "read"},
		{"api explicit GET --method=", []string{"api", "/repos/foo/bar", "--method=GET"}, "read"},

		// gh api with mutating method or fields → write.
		{"api -X PUT", []string{"api", "/repos/foo/bar/contents/x", "-X", "PUT"}, "write"},
		{"api -X POST", []string{"api", "/repos/foo/bar/issues", "-X", "POST"}, "write"},
		{"api -X PATCH", []string{"api", "/x", "-X", "PATCH"}, "write"},
		{"api -X DELETE", []string{"api", "/repos/foo/bar", "-X", "DELETE"}, "write"},
		{"api --method=POST", []string{"api", "/x", "--method=POST"}, "write"},
		{"api -f field", []string{"api", "/repos/foo/bar/issues", "-f", "title=x"}, "write"},
		{"api -F field", []string{"api", "/x", "-F", "name=value"}, "write"},
		{"api --field=", []string{"api", "/x", "--field=foo=bar"}, "write"},
		{"api --raw-field", []string{"api", "/x", "--raw-field", "k=v"}, "write"},

		// Subcommands not in the table → read (safer default).
		{"unknown noun", []string{"newcmd"}, "read"},
		{"unknown verb", []string{"issue", "totally-new-action"}, "read"},
		{"new gh extension", []string{"copilot", "explain"}, "read"},

		// Options before verb are skipped to find the verb.
		{"repo --json X view", []string{"repo", "--json", "name", "view"}, "read"},
		{"issue --repo foo/bar create", []string{"issue", "--repo", "foo/bar", "create"}, "write"},

		// gh codespace ssh (write — modifies/uses a remote codespace).
		{"codespace ssh", []string{"codespace", "ssh"}, "read"}, // not in table; defaults to read

		// Things that wouldn't be misclassified.
		{"verb name shadows another", []string{"issue", "create", "list"}, "write"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.argv)
			if got != tc.want {
				t.Errorf("classify(%v) = %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}
