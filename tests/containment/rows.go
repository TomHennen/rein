package containment

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// run.sh probes a fixture $HOME, so the report is the same on every machine
// and is compared to an exact golden. The exceptions live outside the fixture
// and vary by host: "unknown" rows a human triaged (tolerated.txt), and
// visible unix-socket paths. See README "Golden and tolerated rows".

// Row is one normalized report line.
type Row struct {
	Verdict Verdict
	Kind    Kind
	Target  string
	Reason  string
}

func (r Row) String() string {
	return fmt.Sprintf("[%-10s] %-7s %s — %s", r.Verdict, r.Kind, r.Target, r.Reason)
}

// Sub replaces a per-run or per-box path with a stable label.
type Sub struct{ Path, Label string }

// Normalizer collapses per-run and per-box values. Only THIS user's uid is
// collapsed: another user's /run/user/<n> stays literal and so shows up.
type Normalizer struct {
	subs    []Sub
	runUser *regexp.Regexp
}

// NewNormalizer applies subs in order (list longer paths first).
func NewNormalizer(subs []Sub, uid int) Normalizer {
	return Normalizer{subs, regexp.MustCompile(`/run/user/` + strconv.Itoa(uid) + `(/|$)`)}
}

var (
	reProcPID = regexp.MustCompile(`/proc/[0-9]+/`)
	reRunID   = regexp.MustCompile(`/rein/run-[A-Za-z0-9_-]+`)
)

// Norm normalizes a target or reason string.
func (n Normalizer) Norm(s string) string {
	for _, sub := range n.subs {
		if sub.Path != "" {
			s = strings.ReplaceAll(s, sub.Path, sub.Label)
		}
	}
	s = n.runUser.ReplaceAllString(s, "/run/user/<UID>$1")
	s = reProcPID.ReplaceAllString(s, "/proc/<PID>/")
	return reRunID.ReplaceAllString(s, "/rein/run-<RUNID>")
}

// Tolerated is a reviewed "unknown" row that may or may not appear on a host.
type Tolerated struct {
	Kind   Kind
	Target string
	Note   string // why it is acceptable
}

// ParseTolerated reads tolerated.txt: `kind | target | note` per line. The
// file can only excuse "unknown" rows, and every entry must say why.
func ParseTolerated(r io.Reader) ([]Tolerated, error) {
	var out []Tolerated
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		f := strings.Split(text, "|")
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		if len(f) != 3 || f[0] == "" || f[1] == "" || f[2] == "" {
			return nil, fmt.Errorf("line %d: want `kind | target | note`, all non-empty", line)
		}
		out = append(out, Tolerated{Kind(f[0]), f[1], f[2]})
	}
	return out, sc.Err()
}

// Split normalizes results and separates them into the rows the golden must
// match exactly and the problems that fail the run regardless of the golden:
// any "unknown" row tolerated.txt does not list. Tolerated unknowns and visible
// unix-socket paths are host-dependent and dropped from the golden set.
//
// Tolerated targets match the RAW target, so /proc/1/cgroup excuses pid 1 only.
func Split(results []Result, n Normalizer, tolerated []Tolerated) (golden []Row, untriaged []Row) {
	tol := map[string]bool{}
	for _, t := range tolerated {
		tol[string(t.Kind)+"\x00"+t.Target] = true
	}
	seen := map[Row]bool{}
	for _, r := range results {
		row := Row{r.Verdict, r.Observation.Kind, n.Norm(r.Observation.Target), n.Norm(r.Reason)}
		switch {
		case row.Verdict == VerdictUnknown && tol[string(row.Kind)+"\x00"+r.Observation.Target]:
		case row.Verdict == VerdictOK && row.Kind == KindSocket && r.Observation.Target != SocketCreate:
		case seen[row]:
		case row.Verdict == VerdictUnknown:
			seen[row] = true
			untriaged = append(untriaged, row)
		default:
			seen[row] = true
			golden = append(golden, row)
		}
	}
	for _, s := range [][]Row{golden, untriaged} {
		sort.Slice(s, func(i, j int) bool { return s[i].String() < s[j].String() })
	}
	return golden, untriaged
}
