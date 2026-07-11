package proxy

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
)

const (
	oidA    = "1111111111111111111111111111111111111111"
	oidB    = "2222222222222222222222222222222222222222"
	zeroOID = "0000000000000000000000000000000000000000"
)

func pkt(s string) string { return fmt.Sprintf("%04x", len(s)+4) + s }

// buildBody assembles a receive-pack request body: pkt-lines + flush + tail.
func buildBody(lines []string, tail string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(pkt(l))
	}
	b.WriteString("0000")
	b.WriteString(tail)
	return b.String()
}

func TestParseReceivePack_SingleCommandWithCaps(t *testing.T) {
	body := buildBody([]string{
		oidA + " " + oidB + " refs/heads/agent/73/kx3q\x00report-status side-band-64k agent=git/2.43",
	}, "PACKDATA")
	r := strings.NewReader(body)
	p, err := ParseReceivePackCommands(r)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Commands) != 1 {
		t.Fatalf("commands = %d", len(p.Commands))
	}
	c := p.Commands[0]
	if c.OldOID != oidA || c.NewOID != oidB || c.RefName != "refs/heads/agent/73/kx3q" {
		t.Errorf("bad command: %+v", c)
	}
	if !p.SideBand() {
		t.Error("side-band-64k must be detected")
	}
	// Byte-exactness: prefix + remaining reader = original body.
	rest, _ := io.ReadAll(r)
	if got := string(p.Prefix) + string(rest); got != body {
		t.Error("Prefix + rest must reproduce the body byte-identically (stream-relay invariant)")
	}
	if string(rest) != "PACKDATA" {
		t.Errorf("parser must stop AT the flush; leftover = %q", rest)
	}
}

func TestParseReceivePack_MultiRefAndNewlines(t *testing.T) {
	body := buildBody([]string{
		oidA + " " + oidB + " refs/heads/agent/73/a\x00report-status\n",
		oidA + " " + oidB + " refs/heads/agent/73/b\n",
		zeroOID + " " + oidB + " refs/heads/agent/74/c",
	}, "")
	p, err := ParseReceivePackCommands(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(p.Commands) != 3 {
		t.Fatalf("commands = %d", len(p.Commands))
	}
	if p.Commands[1].RefName != "refs/heads/agent/73/b" {
		t.Errorf("newline must be trimmed from refname: %q", p.Commands[1].RefName)
	}
	if p.SideBand() {
		t.Error("no side-band requested")
	}
}

func TestParseReceivePack_ShallowPrefix(t *testing.T) {
	body := buildBody([]string{
		"shallow " + oidA,
		"shallow " + oidB + "\n",
		oidA + " " + oidB + " refs/heads/agent/73/x\x00report-status",
	}, "")
	p, err := ParseReceivePackCommands(strings.NewReader(body))
	if err != nil {
		t.Fatalf("shallow-prefixed parse: %v", err)
	}
	if len(p.Commands) != 1 {
		t.Fatalf("commands = %d", len(p.Commands))
	}
}

func TestParseReceivePack_DeleteOnly(t *testing.T) {
	body := buildBody([]string{
		oidA + " " + zeroOID + " refs/heads/agent/73/old\x00report-status",
	}, "") // delete-only: no packfile after flush
	p, err := ParseReceivePackCommands(strings.NewReader(body))
	if err != nil {
		t.Fatalf("delete-only parse: %v", err)
	}
	if !p.Commands[0].IsDelete() {
		t.Error("zero new-oid must be detected as a delete")
	}
}

func TestParseReceivePack_FailClosed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"garbage length", "zzzz whatever"},
		{"reserved delim pkt", "0001"},
		{"truncated pkt body", pkt(oidA + " " + oidB + " refs/heads/x")[:20]},
		{"malformed oid", buildBody([]string{"nothex...... " + oidB + " refs/heads/x\x00caps"}, "")},
		{"short oid", buildBody([]string{"abc " + oidB + " refs/heads/x\x00caps"}, "")},
		{"two fields only", buildBody([]string{oidA + " " + oidB + "\x00caps"}, "")},
		{"control char in ref", buildBody([]string{oidA + " " + oidB + " refs/heads/a\x1bb\x00caps"}, "")},
		{"empty refname", buildBody([]string{oidA + " " + oidB + " \x00caps"}, "")},
		{"no commands at all", "0000"},
		{"shallow after command", buildBody([]string{oidA + " " + oidB + " refs/heads/x\x00caps", "shallow " + oidA}, "")},
		{"nul in later command", buildBody([]string{oidA + " " + oidB + " refs/heads/x\x00caps", oidA + " " + oidB + " refs/heads/y\x00more"}, "")},
		{"eof before flush", pkt(oidA + " " + oidB + " refs/heads/x\x00caps")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseReceivePackCommands(strings.NewReader(tc.body)); err == nil {
				t.Error("malformed command section must fail closed")
			}
		})
	}
}

func TestParseReceivePack_OversizedSectionCapped(t *testing.T) {
	var lines []string
	// Enough commands to blow the 64 KiB cap (~90 bytes each).
	for i := 0; i < 1200; i++ {
		lines = append(lines, fmt.Sprintf("%s %s refs/heads/agent/73/nonce%04d", oidA, oidB, i))
	}
	lines[0] += "\x00report-status"
	body := buildBody(lines, "")
	_, err := ParseReceivePackCommands(strings.NewReader(body))
	if err == nil {
		t.Fatal("oversized command section must be rejected")
	}
	if err != ErrCommandSectionTooLarge {
		t.Errorf("want ErrCommandSectionTooLarge, got %v", err)
	}
}

// --- ref convention ---

func TestIssueFromRef(t *testing.T) {
	cases := []struct {
		ref  string
		want int
		ok   bool
	}{
		{"refs/heads/agent/73/kx3q", 73, true},
		{"refs/heads/agent/0/x", 0, false}, // issue 0 does not exist; declare rejects it too
		{"refs/heads/agent/9999999999/n", 9999999999 % (1 << 62), true},
		{"refs/heads/agent/73/a.b-c_d", 73, true},
		{"refs/heads/agent/73/" + strings.Repeat("n", 64), 73, true},

		{"refs/heads/agent/073/x", 0, false},                         // leading zero
		{"refs/heads/agent/73/", 0, false},                           // empty nonce
		{"refs/heads/agent/73", 0, false},                            // no nonce segment
		{"refs/heads/agent/73/x/y", 0, false},                        // extra segment
		{"refs/heads/agent/73/" + strings.Repeat("n", 65), 0, false}, // nonce too long
		{"refs/heads/agent/73/.hidden", 0, false},                    // nonce must start alnum
		{"refs/heads/agent/-73/x", 0, false},
		{"refs/heads/agent/12345678901/x", 0, false}, // >10 digits
		{"refs/heads/main", 0, false},
		{"refs/heads/feature/agent/73/x", 0, false}, // not anchored at agent/
		{"refs/tags/agent/73/x", 0, false},          // tags never match
		{"refs/heads/agent/73/x\n", 0, false},
		{"agent/73/x", 0, false}, // must be fully qualified
	}
	for _, tc := range cases {
		got, ok := IssueFromRef(tc.ref)
		if ok != tc.ok {
			t.Errorf("IssueFromRef(%q) ok = %v, want %v", tc.ref, ok, tc.ok)
			continue
		}
		if ok && tc.ref == "refs/heads/agent/73/kx3q" && got != 73 {
			t.Errorf("IssueFromRef(%q) = %d, want 73", tc.ref, got)
		}
	}
}

// --- synthesized responses ---

func TestSynthesizeAdvertisementERR(t *testing.T) {
	body := SynthesizeAdvertisementERR("rein: writes are locked until you declare your issue. Run: rein declare <n>")
	s := string(body)
	if !strings.HasPrefix(s, pkt("# service=git-receive-pack\n")+"0000") {
		t.Errorf("advertisement must open with the service pkt + flush:\n%q", s)
	}
	if !strings.Contains(s, "ERR rein: writes are locked") {
		t.Errorf("ERR pkt missing:\n%q", s)
	}
	if !strings.HasSuffix(s, "0000") {
		t.Errorf("advertisement must end with a flush:\n%q", s)
	}
	// Framing must be valid pkt-lines end to end.
	r := strings.NewReader(s)
	for i := 0; i < 4; i++ {
		if _, _, err := readPktLine(r); err != nil {
			t.Fatalf("pkt %d unreadable: %v", i, err)
		}
	}
}

func TestSynthesizeAdvertisementERR_SanitizesMessage(t *testing.T) {
	body := SynthesizeAdvertisementERR("evil\nERR forged\x1b[31m")
	if bytes.Contains(body, []byte("\nERR forged")) {
		t.Error("newline injection into the ERR pkt must be neutralized")
	}
	if bytes.Contains(body, []byte{0x1b}) {
		t.Error("terminal escapes must be stripped from the ERR text")
	}
}

func TestSynthesizeNgReport_Plain(t *testing.T) {
	cmds := []RefCommand{
		{OldOID: oidA, NewOID: oidB, RefName: "refs/heads/main"},
		{OldOID: oidA, NewOID: oidB, RefName: "refs/heads/agent/74/x"},
	}
	body := SynthesizeNgReport(cmds, false, func(ref string) string { return "rein: denied (" + ref + ")" })
	s := string(body)
	if !strings.HasPrefix(s, pkt("unpack ok\n")) {
		t.Errorf("report must start with unpack ok:\n%q", s)
	}
	for _, want := range []string{"ng refs/heads/main ", "ng refs/heads/agent/74/x "} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in report:\n%q", want, s)
		}
	}
	if !strings.HasSuffix(s, "0000") {
		t.Error("report must end with a flush")
	}
}

func TestSynthesizeNgReport_SideBandWrapped(t *testing.T) {
	cmds := []RefCommand{{OldOID: oidA, NewOID: oidB, RefName: "refs/heads/main"}}
	body := SynthesizeNgReport(cmds, true, func(string) string { return "no" })
	// First pkt must be band 1 carrying the inner report.
	r := bytes.NewReader(body)
	payload, _, err := readPktLine(r)
	if err != nil {
		t.Fatalf("outer pkt: %v", err)
	}
	if len(payload) == 0 || payload[0] != 1 {
		t.Fatalf("side-band report must travel on band 1, got %v", payload[:1])
	}
	inner := string(payload[1:])
	if !strings.HasPrefix(inner, pkt("unpack ok\n")) {
		t.Errorf("band-1 payload must be the report body:\n%q", inner)
	}
	// Outer stream ends with a flush.
	if !bytes.HasSuffix(body, []byte("0000")) {
		t.Error("side-band stream must end with a flush")
	}
}

func TestSynthesizeNgReport_SanitizesHostileRef(t *testing.T) {
	cmds := []RefCommand{{OldOID: oidA, NewOID: oidB, RefName: "refs/heads/x"}}
	// A hostile "reason" (should be fixed strings, but belt-and-suspenders)
	// and a ref echoed through the reason func must both be sanitized.
	body := SynthesizeNgReport(cmds, false, func(string) string { return "a\nng refs/heads/forged ok\x1b" })
	if bytes.Contains(body, []byte("\nng refs/heads/forged")) {
		t.Error("newline injection into the report must be neutralized")
	}
	if bytes.Contains(body, []byte{0x1b}) {
		t.Error("escapes must be stripped")
	}
}
