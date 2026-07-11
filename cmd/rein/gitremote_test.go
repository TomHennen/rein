package main

import "testing"

func TestRepoFromRemoteURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/TomH/repo.git":     "TomH/repo",
		"https://github.com/TomH/repo":         "TomH/repo",
		"git@github.com:TomH/repo.git":         "TomH/repo",
		"git@github.com:TomH/repo":             "TomH/repo",
		"ssh://git@github.com/TomH/repo.git":   "TomH/repo",
		"http://github.com/TomH/repo":          "TomH/repo",
		"https://gitlab.com/TomH/repo.git":     "", // not github.com
		"git@github.example.com:TomH/repo.git": "", // enterprise host is not github.com
		"":                                     "",
		"not a url":                            "",
		"https://github.com/TomH":              "", // no repo half
	}
	for in, want := range cases {
		if got := repoFromRemoteURL(in); got != want {
			t.Errorf("repoFromRemoteURL(%q) = %q, want %q", in, got, want)
		}
	}
}
