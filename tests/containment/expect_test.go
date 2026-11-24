package containment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/srt"
)

const testExpectations = `
required | ok | env | GH_TOKEN | sensitive env var correctly scrubbed
required | ok | file | <HOME>/.ssh | sensitive path correctly unreadable
required | ok | file | /run/user/<UID> | sensitive path correctly unreadable
ifexists | ok | file | <HOME>/go | path re-exposed read-only via allowRead <HOME>/go
optional | ok | file | <HOME>/.npm-global/bin | path re-exposed read-only via allowRead <HOME>/.npm-global/bin
required | ok | socket | AF_UNIX | AF_UNIX socket creation blocked
optional | ok | socket | * | unix socket path visible but not connectable
oneof:logs | ok | write | <HOME>/.npm/_logs/p | write correctly blocked by denyWrite pin
oneof:logs | ok | write | <HOME>/.npm/_logs/p | write outside writable binds evaporated (deny tmpfs)
triaged | unknown | file | /etc/passwd | readable path not covered | account names only
`

func mustParse(t *testing.T, s string) []Expectation {
	t.Helper()
	exps, err := ParseExpectations(strings.NewReader(s))
	if err != nil {
		t.Fatalf("ParseExpectations: %v", err)
	}
	return exps
}

// baseRows is a passing report for testExpectations on a host WITHOUT ~/go.
func baseRows() []Row {
	return []Row{
		{VerdictOK, KindEnv, "GH_TOKEN", "sensitive env var correctly scrubbed"},
		{VerdictOK, KindFile, "<HOME>/.ssh", "sensitive path correctly unreadable"},
		{VerdictOK, KindFile, "/run/user/<UID>", "sensitive path correctly unreadable"},
		{VerdictOK, KindSocket, "AF_UNIX", "AF_UNIX socket creation blocked"},
		{VerdictOK, KindWrite, "<HOME>/.npm/_logs/p", "write outside writable binds evaporated (deny tmpfs)"},
	}
}

func noneExist(string) bool { return false }

func TestCheck(t *testing.T) {
	exps := mustParse(t, testExpectations)
	goExists := func(target string) bool { return target == "<HOME>/go" }
	goRow := Row{VerdictOK, KindFile, "<HOME>/go", "path re-exposed read-only via allowRead <HOME>/go"}

	tests := []struct {
		name   string
		mutate func([]Row) []Row
		exists func(string) bool
		want   string // substring of the single problem; "" = must pass
	}{
		{"baseline passes", func(r []Row) []Row { return r }, noneExist, ""},
		{"required row missing fails", func(r []Row) []Row { return r[1:] }, noneExist, "MISSING: env GH_TOKEN"},
		{"optional row absent passes", func(r []Row) []Row { return r }, noneExist, ""},
		{"optional row present passes", func(r []Row) []Row {
			return append(r, Row{VerdictOK, KindFile, "<HOME>/.npm-global/bin", "path re-exposed read-only via allowRead <HOME>/.npm-global/bin"})
		}, noneExist, ""},
		{"optional row with wrong verdict fails", func(r []Row) []Row {
			return append(r, Row{VerdictRegression, KindFile, "<HOME>/.npm-global/bin", "allowRead path <HOME>/.npm-global/bin unreadable in sandbox (agent needs it)"})
		}, noneExist, "CHANGED"},
		{"required row with a changed reason fails", func(r []Row) []Row {
			r[1].Reason = "path re-exposed read-only via allowRead <HOME>/.ssh"
			return r
		}, noneExist, "CHANGED"},
		{"leak row fails", func(r []Row) []Row {
			r[1] = Row{VerdictLeak, KindFile, "<HOME>/.ssh", "sensitive path readable in sandbox (under denyRead <HOME>/.ssh)"}
			return r
		}, noneExist, "CHANGED"},
		{"untriaged unknown fails", func(r []Row) []Row {
			return append(r, Row{VerdictUnknown, KindFile, "/etc/shadow", "readable path not covered"})
		}, noneExist, "UNTRIAGED"},
		{"triaged unknown passes", func(r []Row) []Row {
			return append(r, Row{VerdictUnknown, KindFile, "/etc/passwd", "readable path not covered"})
		}, noneExist, ""},
		{"new ok row nobody reviewed fails", func(r []Row) []Row {
			return append(r, Row{VerdictOK, KindFile, "<HOME>/.aws", "path re-exposed read-only via allowRead <HOME>/.aws"})
		}, noneExist, "UNEXPECTED"},
		{"ifexists: on host, row present passes", func(r []Row) []Row { return append(r, goRow) }, goExists, ""},
		{"ifexists: on host, row missing fails", func(r []Row) []Row { return r }, goExists, "MISSING: file <HOME>/go"},
		{"ifexists: not on host, row present fails", func(r []Row) []Row { return append(r, goRow) }, noneExist, "UNEXPECTED: file <HOME>/go"},
		{"oneof: other alternative passes", func(r []Row) []Row {
			r[4].Reason = "write correctly blocked by denyWrite pin"
			return r
		}, noneExist, ""},
		{"oneof: none present fails", func(r []Row) []Row { return r[:4] }, noneExist, `oneof group "logs"`},
		{"any non-connectable socket passes", func(r []Row) []Row {
			return append(r, Row{VerdictOK, KindSocket, "/run/whatever.sock", "unix socket path visible but not connectable"})
		}, noneExist, ""},
		{"connectable socket fails", func(r []Row) []Row {
			return append(r, Row{VerdictLeak, KindSocket, "/run/docker.sock", "unix socket connectable from the sandbox"})
		}, noneExist, "CHANGED"},
		{"socket creation allowed fails", func(r []Row) []Row {
			r[3] = Row{VerdictLeak, KindSocket, "AF_UNIX", "AF_UNIX socket creation allowed in sandbox (seccomp block missing)"}
			return r
		}, noneExist, "CHANGED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			problems := Check(tc.mutate(baseRows()), exps, tc.exists)
			if tc.want == "" {
				if len(problems) != 0 {
					t.Fatalf("want pass, got %v", problems)
				}
				return
			}
			if len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Fatalf("want a problem containing %q, got %v", tc.want, problems)
			}
		})
	}
}

func TestParseExpectationsRejects(t *testing.T) {
	bad := map[string]string{
		"leak can never be expected":       "optional | leak | file | /x | r",
		"regression can never be expected": "optional | regression | file | /x | r",
		"unknown must be triaged":          "optional | unknown | file | /x | r | note",
		"triaged needs a note":             "triaged | unknown | file | /x | r",
		"triaged is unknown-only":          "triaged | ok | file | /x | r | note",
		"wildcard on a file row":           "optional | ok | file | * | r",
		"wildcard on a required row":       "required | ok | socket | * | r",
		"wildcard triage":                  "triaged | unknown | socket | * | r | note",
		"ifexists on a non-file":           "ifexists | ok | env | X | r",
		"unknown class":                    "maybe | ok | file | /x | r",
		"unknown kind":                     "required | ok | port | 80 | r",
		"oneof without a group":            "oneof | ok | file | /x | r",
		"too few fields":                   "required | ok | file | /x",
		"duplicate line":                   "required | ok | file | /x | r\nrequired | ok | file | /x | r",
	}
	for name, text := range bad {
		if _, err := ParseExpectations(strings.NewReader(text)); err == nil {
			t.Errorf("%s: accepted %q", name, text)
		}
	}
}

// The committed file must parse; a typo there would otherwise only surface on
// a live run.
func TestCommittedExpectationsParse(t *testing.T) {
	f, err := os.Open("expected.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	exps, err := ParseExpectations(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(exps) == 0 {
		t.Fatal("expected.txt has no rows")
	}
}

func TestNormalizerIsPerBoxNeutral(t *testing.T) {
	report := func(home, wd string, uid string) []Result {
		obs := func(k Kind, target string) Observation { return Observation{Kind: k, Target: target} }
		return []Result{
			{obs(KindFile, "/run/user/"+uid), VerdictOK, "sensitive path correctly unreadable"},
			{obs(KindFile, "/run/user/"+uid+"/rein/run-aB3_x-9"), VerdictOK, "sensitive path correctly unreadable"},
			{obs(KindFile, home+"/go"), VerdictOK, "path re-exposed read-only via allowRead " + home + "/go"},
			{obs(KindWrite, wd+"/probe"), VerdictOK, "write persisted inside a writable bind (expected)"},
			{obs(KindSocket, "/tmp/tmux-"+uid+"/default"), VerdictOK, "unix socket path visible but not connectable"},
			{obs(KindSocket, "/tmp/cc-daemon-"+uid+"/0123456789abcdef/control.sock"), VerdictOK, "unix socket path visible but not connectable"},
			{obs(KindSocket, "/tmp/srt-mux-123-4.sock"), VerdictOK, "unix socket path visible but not connectable"},
			{obs(KindSocket, "/tmp/srt-mux-987-6.sock"), VerdictOK, "unix socket path visible but not connectable"},
			{obs(KindFile, "/proc/41/cgroup"), VerdictUnknown, "readable"},
		}
	}
	a := Normalizer{Home: "/home/alice", Worktree: "/tmp/rein-containment-aaa", UID: 1000}.
		Rows(report("/home/alice", "/tmp/rein-containment-aaa", "1000"))
	b := Normalizer{Home: "/Users/bob.linux", Worktree: "/tmp/nix-shell.q/rein-containment-bbb", UID: 501}.
		Rows(report("/Users/bob.linux", "/tmp/nix-shell.q/rein-containment-bbb", "501"))
	if len(a) != len(b) {
		t.Fatalf("row counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("row %d differs across boxes:\n  %s\n  %s", i, a[i], b[i])
		}
	}
	if len(a) != 8 { // the two srt-mux sockets collapse to one row
		t.Errorf("want 8 rows after de-duplication, got %d: %v", len(a), a)
	}

	// Only this user's uid is collapsed: another user's runtime dir must stay
	// literal so it cannot hide behind the <UID> rows.
	n := Normalizer{UID: 501}
	for _, other := range []string{"/run/user/0", "/run/user/5010/x", "/tmp/tmux-0/default"} {
		if got := n.Norm(other); got != other {
			t.Errorf("Norm(%q) = %q, want it left literal", other, got)
		}
	}
}

func TestClassifySocket(t *testing.T) {
	home := t.TempDir()
	o := NewOracle(srt.Config{Filesystem: srt.Filesystem{DenyRead: []string{home}}})
	tests := []struct {
		name string
		obs  Observation
		want Verdict
	}{
		{"creation blocked", Observation{Kind: KindSocket, Target: SocketCreate}, VerdictOK},
		{"creation allowed is a LEAK", Observation{Kind: KindSocket, Target: SocketCreate, Reachable: true}, VerdictLeak},
		{"visible, not connectable", Observation{Kind: KindSocket, Target: "/run/docker.sock"}, VerdictOK},
		{"connectable is a LEAK", Observation{Kind: KindSocket, Target: "/run/docker.sock", Reachable: true}, VerdictLeak},
		{"visible under a read deny is a LEAK", Observation{Kind: KindSocket, Target: filepath.Join(home, ".gnupg", "S.gpg-agent")}, VerdictLeak},
	}
	for _, tc := range tests {
		if got := o.Classify(tc.obs); got.Verdict != tc.want {
			t.Errorf("%s: got %s (%s), want %s", tc.name, got.Verdict, got.Reason, tc.want)
		}
	}
}

func TestHasContent(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	deep := filepath.Join(dir, "deep", "a", "b")
	tooDeep := filepath.Join(dir, "toodeep", "a", "b", "c")
	for _, d := range []string{empty, deep, tooDeep} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(deep, "f"), filepath.Join(tooDeep, "f")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	zero := filepath.Join(dir, "zero")
	if err := os.WriteFile(zero, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		filepath.Join(dir, "absent"):  false,
		empty:                         false,
		zero:                          false,
		filepath.Join(dir, "deep"):    true,
		filepath.Join(dir, "toodeep"): false, // file is past the probe's depth 3
		filepath.Join(deep, "f"):      true,
	} {
		if got := HasContent(path); got != want {
			t.Errorf("HasContent(%s) = %v, want %v", path, got, want)
		}
	}
}
