package gitupstream

import "testing"

func curBranch(name string) func() (string, error) {
	return func() (string, error) { return name, nil }
}

func TestParsePush(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		cur    string
		want   Intent
		wantOK bool
	}{
		{"HEAD:ref form", []string{"push", "-u", "origin", "HEAD:refs/heads/agent/73/ab"}, "agent/73/ab",
			Intent{"origin", "agent/73/ab", "refs/heads/agent/73/ab"}, true},
		{"bare name form", []string{"push", "-u", "origin", "agent/73/ab"}, "unused",
			Intent{"origin", "agent/73/ab", "refs/heads/agent/73/ab"}, true},
		{"HEAD:short-dst", []string{"push", "-u", "origin", "HEAD:mybranch"}, "feature",
			Intent{"origin", "feature", "refs/heads/mybranch"}, true},
		{"no refspec uses current branch", []string{"push", "-u"}, "feature",
			Intent{"origin", "feature", "refs/heads/feature"}, true},
		{"src:dst distinct", []string{"push", "-u", "origin", "local:remote"}, "unused",
			Intent{"origin", "local", "refs/heads/remote"}, true},
		{"force marker stripped", []string{"push", "-u", "origin", "+HEAD:refs/heads/x"}, "cur",
			Intent{"origin", "cur", "refs/heads/x"}, true},
		{"push-option consumes token", []string{"push", "-u", "-o", "ci.skip", "origin", "b"}, "unused",
			Intent{"origin", "b", "refs/heads/b"}, true},
		{"custom remote", []string{"push", "-u", "upstream", "b"}, "unused",
			Intent{"upstream", "b", "refs/heads/b"}, true},
		{"delete push declines", []string{"push", "-d", "origin", "b"}, "unused", Intent{}, false},
		{"src-only declines", []string{"push", "-u", "origin", "b:"}, "unused", Intent{}, false},
		{"--all declines (bulk, multi-branch)", []string{"push", "-u", "--all", "origin"}, "cur", Intent{}, false},
		{"--tags declines", []string{"push", "-u", "--tags", "origin"}, "cur", Intent{}, false},
		{"--mirror declines", []string{"push", "-u", "--mirror", "origin"}, "cur", Intent{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParsePush(c.args, curBranch(c.cur))
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestParsePushDetachedHeadDeclines(t *testing.T) {
	fail := func() (string, error) { return "", errDetached }
	if _, ok := ParsePush([]string{"push", "-u", "origin", "HEAD:refs/heads/x"}, fail); ok {
		t.Fatal("detached HEAD should decline capture")
	}
}

var errDetached = &detachedErr{}

type detachedErr struct{}

func (*detachedErr) Error() string { return "detached" }

func TestEncodeParseRoundTrip(t *testing.T) {
	in := Intent{"origin", "agent/73/ab", "refs/heads/agent/73/ab"}
	got, err := ParseLine(EncodeLine(in))
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestParseLineRejectsShape(t *testing.T) {
	for _, bad := range []string{"", "a\tb", "a\tb\tc\td", "no-tabs-at-all"} {
		if _, err := ParseLine(bad); err == nil {
			t.Errorf("ParseLine(%q) should error", bad)
		}
	}
}

func TestValidate(t *testing.T) {
	yes := func(string) bool { return true }
	no := func(string) bool { return false }
	good := Intent{"origin", "agent/73/ab", "refs/heads/agent/73/ab"}

	if !Validate(good, yes, yes) {
		t.Fatal("valid intent rejected")
	}
	if Validate(good, no, yes) {
		t.Fatal("missing remote must be rejected")
	}
	if Validate(good, yes, no) {
		t.Fatal("missing local branch must be rejected")
	}
	// merge must be refs/heads/<valid>
	if Validate(Intent{"origin", "b", "refs/tags/v1"}, yes, yes) {
		t.Fatal("non-refs/heads merge must be rejected")
	}
	if Validate(Intent{"origin", "b", "b"}, yes, yes) {
		t.Fatal("bare merge must be rejected")
	}
	// injection-y / malformed values rejected even if 'exists' says yes
	for _, in := range []Intent{
		{"origin", "-x", "refs/heads/x"},
		{"origin", "a\nb", "refs/heads/x"},
		{"origin", "../etc", "refs/heads/x"},
		{"or gin", "x", "refs/heads/x"},
		{"origin", "x", "refs/heads/-bad"},
	} {
		if Validate(in, yes, yes) {
			t.Errorf("malformed intent accepted: %+v", in)
		}
	}
}

func TestHasSetUpstream(t *testing.T) {
	yes := [][]string{
		{"push", "-u", "origin", "b"},
		{"push", "--set-upstream", "origin", "b"},
		{"push", "origin", "b", "-u"},
	}
	no := [][]string{
		{"push", "origin", "b"},
		{"push", "origin", "HEAD:refs/heads/x"},
		{"push", "--force", "origin", "b"},
	}
	for _, a := range yes {
		if !HasSetUpstream(a) {
			t.Errorf("HasSetUpstream(%v) = false, want true", a)
		}
	}
	for _, a := range no {
		if HasSetUpstream(a) {
			t.Errorf("HasSetUpstream(%v) = true, want false", a)
		}
	}
}

func TestValidRefName(t *testing.T) {
	ok := []string{"agent/73/ab12", "feature", "a.b_c-d", "x/y/z"}
	bad := []string{"", "-x", "/x", "x/", "a//b", "a..b", "a b", "a\tb", "a~b", ".."}
	for _, s := range ok {
		if !ValidRefName(s) {
			t.Errorf("ValidRefName(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidRefName(s) {
			t.Errorf("ValidRefName(%q) = true, want false", s)
		}
	}
}
