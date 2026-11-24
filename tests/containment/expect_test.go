package containment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/srt"
)

func res(v Verdict, k Kind, target, reason string) Result {
	return Result{Observation{Kind: k, Target: target}, v, reason}
}

func TestSplit(t *testing.T) {
	n := Normalizer{Subs: []Sub{{"/tmp/fx/home", "<HOME>"}}, UID: 501}
	tol := []Tolerated{{KindFile, "/etc/passwd", "names only"}}
	golden, untriaged := Split([]Result{
		res(VerdictOK, KindFile, "/tmp/fx/home/.ssh", "unreadable"),
		res(VerdictOK, KindFile, "/tmp/fx/home/.ssh", "unreadable"), // duplicate
		res(VerdictOK, KindFile, "/run/user/501", "unreadable"),
		res(VerdictUnknown, KindFile, "/etc/passwd", "readable"),       // tolerated
		res(VerdictUnknown, KindFile, "/etc/shadow", "readable"),       // NOT tolerated
		res(VerdictOK, KindSocket, "/tmp/tmux-501/default", "visible"), // host-dependent
		res(VerdictOK, KindSocket, SocketCreate, "blocked"),
		res(VerdictLeak, KindFile, "/etc/passwd", "leak"), // a tolerated target never hides a leak
		res(VerdictRegression, KindFile, "/tmp/fx/home/go", "hidden"),
	}, n, tol)

	var got []string
	for _, r := range golden {
		got = append(got, string(r.Verdict)+" "+string(r.Kind)+" "+r.Target)
	}
	want := []string{
		"leak file /etc/passwd",
		"ok file /run/user/<UID>",
		"ok file <HOME>/.ssh",
		"ok socket AF_UNIX",
		"regression file <HOME>/go",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("golden rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(untriaged) != 1 || untriaged[0].Target != "/etc/shadow" {
		t.Errorf("untriaged = %v, want only /etc/shadow", untriaged)
	}
}

func TestParseTolerated(t *testing.T) {
	for _, bad := range []string{
		"file | /etc/passwd",        // no note
		"file | /etc/passwd | ",     // empty note
		"unknown | file | /x | why", // old 4-field shape
	} {
		if _, err := ParseTolerated(strings.NewReader(bad)); err == nil {
			t.Errorf("ParseTolerated(%q) accepted a bad line", bad)
		}
	}
	f, err := os.Open("tolerated.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tol, err := ParseTolerated(f)
	if err != nil || len(tol) == 0 {
		t.Fatalf("committed tolerated.txt: %v (%d entries)", err, len(tol))
	}
}

// The committed golden may hold only "ok" rows: nothing else can be adopted.
func TestCommittedGoldenIsAllOK(t *testing.T) {
	data, err := os.ReadFile("golden-report.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.HasPrefix(line, "[ok ") {
			t.Errorf("non-ok row in golden: %s", line)
		}
	}
}

func TestNormalizerIsPerBoxNeutral(t *testing.T) {
	for _, uid := range []int{501, 1000} {
		n := Normalizer{UID: uid}
		u := map[int]string{501: "501", 1000: "1000"}[uid]
		if got := n.Norm("/run/user/" + u + "/rein/run-aB_9-x"); got != "/run/user/<UID>/rein/run-<RUNID>" {
			t.Errorf("uid %d: got %q", uid, got)
		}
	}
	// Another user's runtime dir is not this box's noise: keep it literal.
	if got := (Normalizer{UID: 501}).Norm("/run/user/1000"); got != "/run/user/1000" {
		t.Errorf("other uid collapsed: %q", got)
	}
	if got := (Normalizer{UID: 50}).Norm("/run/user/501"); got != "/run/user/501" {
		t.Errorf("uid prefix collapsed: %q", got)
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
