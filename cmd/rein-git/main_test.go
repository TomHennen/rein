package main

import "testing"

// TestClassify covers the argv parser. The interesting cases are git's
// global options: the shim must skip past `-c key=val`, `-C path`,
// `--git-dir=path` etc. to find the actual subcommand. A bug here would
// mean a write op silently classified as read (push fails noisily — okay)
// or a read op classified as write (extra write mints — wasteful but safe).
func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want string
	}{
		// Trivial.
		{"no args", nil, "unknown"},
		{"empty arg", []string{""}, "unknown"},
		{"bare verb push", []string{"push"}, "write"},
		{"bare verb fetch", []string{"fetch"}, "read"},
		{"bare verb send-pack", []string{"send-pack", "origin", "main"}, "write"},
		{"bare verb commit (no network)", []string{"commit", "-m", "x"}, "unknown"},
		{"bare verb ls-remote", []string{"ls-remote"}, "read"},
		{"bare verb pull", []string{"pull"}, "read"},
		{"bare verb clone", []string{"clone", "https://github.com/x/y.git"}, "read"},

		// Verb with its own args.
		{"push with refspec", []string{"push", "origin", "main"}, "write"},
		{"push with options", []string{"push", "--force-with-lease", "origin", "feature"}, "write"},
		{"fetch with options", []string{"fetch", "--prune", "origin"}, "read"},

		// Global options that take an argument as the NEXT token.
		{"-c key=val push", []string{"-c", "http.proxy=http://p:8080", "push"}, "write"},
		{"-C path push", []string{"-C", "/some/repo", "push"}, "write"},
		{"--git-dir path push", []string{"--git-dir", "/some/.git", "push"}, "write"},
		{"--work-tree path fetch", []string{"--work-tree", "/some/wt", "fetch"}, "read"},
		{"--namespace ns push", []string{"--namespace", "x", "push"}, "write"},
		{"--exec-path path push", []string{"--exec-path", "/opt/git-core", "push"}, "write"},
		{"--attr-source val push", []string{"--attr-source", "HEAD", "push"}, "write"},
		{"--config-env val push", []string{"--config-env", "k=v", "push"}, "write"},
		{"--super-prefix val push", []string{"--super-prefix", "sub/", "push"}, "write"},

		// --name=value form: value is in the same token, do NOT skip the next arg.
		{"--git-dir=path push", []string{"--git-dir=/some/.git", "push"}, "write"},
		{"--work-tree=path push", []string{"--work-tree=/some/wt", "push"}, "write"},

		// Boolean global options (no argument).
		{"-p push", []string{"-p", "push"}, "write"},
		{"--paginate push", []string{"--paginate", "push"}, "write"},
		{"--no-pager push", []string{"--no-pager", "push"}, "write"},
		{"--no-optional-locks push", []string{"--no-optional-locks", "push"}, "write"},
		{"--no-replace-objects push", []string{"--no-replace-objects", "push"}, "write"},
		{"--bare push", []string{"--bare", "push"}, "write"},

		// Combined: multiple global options + verb.
		{"complex: -c, -C, push", []string{"-c", "a=b", "-C", "/p", "push"}, "write"},
		{"complex: --git-dir=, -c, fetch", []string{"--git-dir=/x", "-c", "a=b", "fetch"}, "read"},

		// Pathological / unknown verbs.
		{"unknown subcommand", []string{"watusi"}, "unknown"},
		{"only options", []string{"-h", "--version"}, "unknown"},

		// Things that shouldn't be misclassified.
		{"branch named push exists, but verb is checkout", []string{"checkout", "push"}, "unknown"},
		{"verb is push, ref happens to be 'fetch'", []string{"push", "origin", "fetch"}, "write"},
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
