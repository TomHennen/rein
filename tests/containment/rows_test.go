package containment

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomHennen/rein/internal/srt"
)

func res(v Verdict, k Kind, target, reason string) Result {
	return Result{Observation{Kind: k, Target: target}, v, reason}
}

func TestSplit(t *testing.T) {
	n := NewNormalizer([]Sub{{"/tmp/fx/home", "<HOME>"}}, 501)
	tol := []Tolerated{{KindFile, "/etc/passwd", "names only"}, {KindFile, "/proc/1/cgroup", "sandbox pid 1"}}
	golden, untriaged := Split([]Result{
		res(VerdictOK, KindFile, "/tmp/fx/home/.ssh", "unreadable"),
		res(VerdictOK, KindFile, "/tmp/fx/home/.ssh", "unreadable"), // duplicate
		res(VerdictOK, KindFile, "/run/user/501", "unreadable"),
		res(VerdictUnknown, KindFile, "/etc/passwd", "readable"),       // tolerated
		res(VerdictUnknown, KindFile, "/etc/shadow", "readable"),       // NOT tolerated
		res(VerdictUnknown, KindFile, "/proc/1/cgroup", "readable"),    // tolerated: literal pid 1
		res(VerdictUnknown, KindFile, "/proc/4242/cgroup", "readable"), // NOT: a host pid
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
	// Untriaged rows keep the raw target: it is what tolerated.txt matches.
	if len(untriaged) != 2 || untriaged[0].Target != "/etc/shadow" || untriaged[1].Target != "/proc/4242/cgroup" {
		t.Errorf("untriaged = %v, want /etc/shadow and the host-pid cgroup", untriaged)
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
}

func TestNormalizerIsPerBoxNeutral(t *testing.T) {
	for _, uid := range []int{501, 1000} {
		n := NewNormalizer(nil, uid)
		u := map[int]string{501: "501", 1000: "1000"}[uid]
		if got := n.Norm("/run/user/" + u + "/rein"); got != "/run/user/<UID>/rein" {
			t.Errorf("uid %d: got %q", uid, got)
		}
	}
	// Another user's runtime dir is not this box's noise: keep it literal.
	if got := NewNormalizer(nil, 501).Norm("/run/user/1000"); got != "/run/user/1000" {
		t.Errorf("other uid collapsed: %q", got)
	}
	if got := NewNormalizer(nil, 50).Norm("/run/user/501"); got != "/run/user/501" {
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
		{"visible", Observation{Kind: KindSocket, Target: "/run/docker.sock", Reachable: true}, VerdictOK},
		{"visible under a read deny is a LEAK", Observation{Kind: KindSocket, Target: filepath.Join(home, ".gnupg", "S.gpg-agent")}, VerdictLeak},
	}
	for _, tc := range tests {
		if got := o.Classify(tc.obs); got.Verdict != tc.want {
			t.Errorf("%s: got %s (%s), want %s", tc.name, got.Verdict, got.Reason, tc.want)
		}
	}
}
