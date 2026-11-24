package containment

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The expectations file replaces a per-box golden snapshot: it says which
// report rows every machine must produce, which depend on host state, and
// which "unknown" rows a human has triaged. See README "Expectations".

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

// Class says when an expected row must appear.
type Class string

const (
	ClassRequired Class = "required" // on every machine
	ClassIfExists Class = "ifexists" // exactly when the target has content on the host (HasContent)
	ClassOptional Class = "optional" // host-dependent; present or absent
	ClassTriaged  Class = "triaged"  // a reviewed "unknown"; present or absent
	ClassOneOf    Class = "oneof"    // at least one row of the group
)

// AnyTarget matches every target of its kind. Only verified-per-run socket
// rows may use it (see ParseExpectations).
const AnyTarget = "*"

// Expectation is one line of the expectations file.
type Expectation struct {
	Class   Class
	Group   string // ClassOneOf only
	Verdict Verdict
	Kind    Kind
	Target  string
	Reason  string
	Note    string // why a triaged row is acceptable
	Line    int
}

// Normalizer collapses per-run and per-box values so rows compare across
// machines. Only THIS user's uid is collapsed: another user's /run/user/<n>
// stays literal and so shows up as an unexpected row.
type Normalizer struct {
	Home     string
	Worktree string
	UID      int
}

var (
	reProcPID = regexp.MustCompile(`/proc/[0-9]+/`)
	reSrtMux  = regexp.MustCompile(`/tmp/srt-mux-[0-9-]*\.sock`)
	reRunID   = regexp.MustCompile(`/rein/run-[A-Za-z0-9_-]+`)
	reHex16   = regexp.MustCompile(`[0-9a-f]{16}`)
	reHex8Dot = regexp.MustCompile(`[0-9a-f]{8}\.`)
)

// Norm normalizes a target or reason string.
func (n Normalizer) Norm(s string) string {
	if n.Worktree != "" {
		s = strings.ReplaceAll(s, n.Worktree, "<WORKTREE>")
	}
	if n.Home != "" {
		s = strings.ReplaceAll(s, n.Home, "<HOME>")
	}
	uid := strconv.Itoa(n.UID)
	for _, prefix := range []string{"/run/user/", "/tmp/tmux-", "/tmp/cc-daemon-"} {
		re := regexp.MustCompile(regexp.QuoteMeta(prefix+uid) + `(/|$)`)
		s = re.ReplaceAllString(s, prefix+"<UID>$1")
	}
	s = reProcPID.ReplaceAllString(s, "/proc/<PID>/")
	s = reSrtMux.ReplaceAllString(s, "/tmp/srt-mux-<N>.sock")
	s = reRunID.ReplaceAllString(s, "/rein/run-<RUNID>")
	s = reHex16.ReplaceAllString(s, "<HEX>")
	s = reHex8Dot.ReplaceAllString(s, "<HEX>.")
	return s
}

// Rows normalizes classified results into a sorted, de-duplicated row set.
func (n Normalizer) Rows(results []Result) []Row {
	seen := map[Row]bool{}
	var out []Row
	for _, r := range results {
		row := Row{r.Verdict, r.Observation.Kind, n.Norm(r.Observation.Target), n.Norm(r.Reason)}
		if !seen[row] {
			seen[row] = true
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// HasContent mirrors the in-sandbox read probe on the HOST: a non-empty
// readable file, or a dir with one within depth 3. An allow-back with no
// content cannot be judged (an empty dir reads the same hidden or exposed), so
// it is neither probed nor expected.
func HasContent(path string) bool { return hasContent(path, 3) }

func hasContent(path string, depth int) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !st.IsDir() {
		if !st.Mode().IsRegular() {
			return false
		}
		f, err := os.Open(path)
		if err != nil {
			return false
		}
		defer f.Close()
		n, _ := f.Read(make([]byte, 1))
		return n == 1
	}
	if depth == 0 {
		return false
	}
	ents, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if hasContent(filepath.Join(path, e.Name()), depth-1) {
			return true
		}
	}
	return false
}

// ParseExpectations reads the expectations file. Format, one row per line:
//
//	class | verdict | kind | target | reason [| note]
//
// It rejects anything that would let the file excuse a failure: leak and
// regression verdicts are never expectable, an "unknown" must be triaged with
// a note, and the wildcard target is limited to verified socket rows.
func ParseExpectations(r io.Reader) ([]Expectation, error) {
	var out []Expectation
	dup := map[string]int{}
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
		if len(f) != 5 && len(f) != 6 {
			return nil, fmt.Errorf("line %d: want 5 or 6 '|' fields, got %d", line, len(f))
		}
		e := Expectation{Verdict: Verdict(f[1]), Kind: Kind(f[2]), Target: f[3], Reason: f[4], Line: line}
		if len(f) == 6 {
			e.Note = f[5]
		}
		class, group, _ := strings.Cut(f[0], ":")
		e.Class, e.Group = Class(class), group
		switch e.Class {
		case ClassRequired, ClassIfExists, ClassOptional, ClassTriaged:
			if group != "" {
				return nil, fmt.Errorf("line %d: only oneof takes a :group", line)
			}
		case ClassOneOf:
			if group == "" {
				return nil, fmt.Errorf("line %d: oneof needs a group (oneof:<name>)", line)
			}
		default:
			return nil, fmt.Errorf("line %d: unknown class %q", line, f[0])
		}
		switch e.Kind {
		case KindNetwork, KindFile, KindEnv, KindWrite, KindSocket:
		default:
			return nil, fmt.Errorf("line %d: unknown kind %q", line, f[2])
		}
		if e.Target == "" || e.Reason == "" {
			return nil, fmt.Errorf("line %d: empty target or reason", line)
		}
		switch e.Verdict {
		case VerdictOK:
			if e.Class == ClassTriaged {
				return nil, fmt.Errorf("line %d: triaged is for unknown rows only", line)
			}
		case VerdictUnknown:
			if e.Class != ClassTriaged {
				return nil, fmt.Errorf("line %d: an unknown row must be class triaged", line)
			}
			if e.Note == "" {
				return nil, fmt.Errorf("line %d: a triaged row needs a note saying why it is acceptable", line)
			}
		default:
			return nil, fmt.Errorf("line %d: verdict %q can never be expected", line, f[1])
		}
		if e.Class == ClassIfExists && e.Kind != KindFile {
			return nil, fmt.Errorf("line %d: ifexists applies to file rows only", line)
		}
		if e.Target == AnyTarget && !(e.Class == ClassOptional && e.Kind == KindSocket && e.Verdict == VerdictOK) {
			return nil, fmt.Errorf("line %d: target * is only allowed as optional | ok | socket", line)
		}
		key := strings.Join([]string{string(e.Verdict), string(e.Kind), e.Target, e.Reason}, "\x00")
		if prev, ok := dup[key]; ok {
			return nil, fmt.Errorf("line %d: duplicate of line %d", line, prev)
		}
		dup[key] = line
		out = append(out, e)
	}
	return out, sc.Err()
}

// Check compares rows to the expectations and returns one message per problem
// (empty = pass). exists reports whether an ifexists target has content on the
// host; it receives the normalized target.
func Check(rows []Row, exps []Expectation, exists func(target string) bool) []string {
	byTarget := map[string][]int{}
	for i, e := range exps {
		k := string(e.Kind) + "\x00" + e.Target
		byTarget[k] = append(byTarget[k], i)
	}
	seen := make([]bool, len(exps))
	var problems []string

	for _, row := range rows {
		cands := byTarget[string(row.Kind)+"\x00"+row.Target]
		if len(cands) == 0 {
			cands = byTarget[string(row.Kind)+"\x00"+AnyTarget]
		}
		if len(cands) == 0 {
			if row.Verdict == VerdictUnknown {
				problems = append(problems, "UNTRIAGED: "+row.String()+
					"\n    decide whether rein should deny it; if it is acceptable, add a 'triaged' line with the reason to expected.txt")
			} else {
				problems = append(problems, "UNEXPECTED: "+row.String()+
					"\n    no expectation covers this row; review it and add it to expected.txt if it is intended")
			}
			continue
		}
		matched := false
		for _, i := range cands {
			if exps[i].Verdict == row.Verdict && exps[i].Reason == row.Reason {
				seen[i], matched = true, true
			}
		}
		if !matched {
			e := exps[cands[0]]
			problems = append(problems, fmt.Sprintf("CHANGED: %s\n    expected (expected.txt:%d): [%s] %s",
				row.String(), e.Line, e.Verdict, e.Reason))
		}
	}

	groups := map[string]bool{}
	var groupOrder []string
	for i, e := range exps {
		switch e.Class {
		case ClassRequired:
			if !seen[i] {
				problems = append(problems, fmt.Sprintf("MISSING: %s %s (expected.txt:%d) — the probe no longer produces this row", e.Kind, e.Target, e.Line))
			}
		case ClassIfExists:
			if on := exists(e.Target); on && !seen[i] {
				problems = append(problems, fmt.Sprintf("MISSING: %s %s exists on this host but produced no row (expected.txt:%d)", e.Kind, e.Target, e.Line))
			} else if !on && seen[i] {
				problems = append(problems, fmt.Sprintf("UNEXPECTED: %s %s has a row but does not exist on this host (expected.txt:%d)", e.Kind, e.Target, e.Line))
			}
		case ClassOneOf:
			if _, ok := groups[e.Group]; !ok {
				groupOrder = append(groupOrder, e.Group)
			}
			groups[e.Group] = groups[e.Group] || seen[i]
		}
	}
	for _, g := range groupOrder {
		if !groups[g] {
			problems = append(problems, fmt.Sprintf("MISSING: no row of the oneof group %q appeared", g))
		}
	}
	return problems
}
