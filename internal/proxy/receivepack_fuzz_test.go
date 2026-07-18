package proxy

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// Fuzz coverage (issue #136A; `go test -run . -coverprofile`, seed corpus +
// unit tests). Every parser function reached by this target is at 100% stmt
// coverage: ParseReceivePackCommands, parseCommandLine, readPktLine, isOID,
// isZeroOID, hasControlChars, sanitizePktText. All fail-closed branches are
// hit — malformed pkt length (reserved/oversized/short-body), oversize command
// section, malformed shallow line, NUL-in-caps and NUL-outside-first-line,
// non-hex OID of OID length, control-char refname, empty/zero-command bodies,
// and the delete (zero-OID) shape. The one deliberately unreached branch is
// IssueFromRef's `strconv.Atoi` error return (l.204): unreachable belt-and-
// suspenders, the regexp already guarantees the capture is 1-10 digits.
// A 30s -fuzz run (~3.7M execs) found no crash. Verdict: sufficient for the
// parser's fail-closed surface — 100% stmt coverage from the committed seeds.
//
// FuzzParseReceivePackCommands fuzzes the untrusted pkt-line command-section
// parser (issue #136A): the input is git receive-pack wire bytes from a
// possibly-prompt-injected agent, feeding the declare gate's issue/nonce
// anchor. The parser MUST never panic and MUST fail closed on malformed input.
//
// Properties enforced:
//   - never panics (harness catches it);
//   - on error: the returned struct is the zero value (no partial command leaks
//     out to a caller that ignores err);
//   - on success: every fail-closed invariant the caller relies on holds —
//     valid OIDs, control-char-free refnames, no NUL in Caps — and the parse
//     ROUND-TRIPS: re-feeding Prefix reparses to the identical commands and
//     consumes exactly Prefix (it ends at the flush, byte-for-byte).
func FuzzParseReceivePackCommands(f *testing.F) {
	// Seed corpus: well-formed shapes + every malformed case the table test
	// pins, so the corpus starts adversarial rather than growing into it.
	seeds := []string{
		buildBody([]string{oidA + " " + oidB + " refs/heads/agent/73/kx3q\x00report-status side-band-64k agent=git/2.43"}, "PACKDATA"),
		buildBody([]string{
			oidA + " " + oidB + " refs/heads/agent/73/a\x00report-status\n",
			oidA + " " + oidB + " refs/heads/agent/73/b\n",
			zeroOID + " " + oidB + " refs/heads/agent/74/c",
		}, ""),
		buildBody([]string{"shallow " + oidA, "shallow " + oidB + "\n", oidA + " " + oidB + " refs/heads/agent/73/x\x00report-status"}, ""),
		buildBody([]string{oidA + " " + zeroOID + " refs/heads/agent/73/old\x00report-status"}, ""),
		// Malformed / fail-closed seeds.
		"zzzz whatever",
		"0001",
		"0000",
		pkt(oidA + " " + oidB + " refs/heads/x")[:20],
		buildBody([]string{"nothex...... " + oidB + " refs/heads/x\x00caps"}, ""),
		buildBody([]string{oidA + " " + oidB + "\x00caps"}, ""),
		buildBody([]string{oidA + " " + oidB + " refs/heads/a\x1bb\x00caps"}, ""),
		buildBody([]string{oidA + " " + oidB + " \x00caps"}, ""),
		buildBody([]string{oidA + " " + oidB + " refs/heads/x\x00caps", "shallow " + oidA}, ""),
		buildBody([]string{oidA + " " + oidB + " refs/heads/x\x00caps", oidA + " " + oidB + " refs/heads/y\x00more"}, ""),
		pkt(oidA + " " + oidB + " refs/heads/x\x00caps"),
		"0004",           // empty pkt then EOF
		"ffff",           // oversized length
		"00ff" + "short", // body shorter than declared length
		// Coverage-directed adversarial seeds (issue #136A): each reaches a
		// fail-closed branch the well-formed seeds miss.
		buildBody([]string{"shallow notanoid", oidA + " " + oidB + " refs/heads/x\x00caps"}, ""),      // malformed shallow line
		buildBody([]string{oidA + " " + oidB + " refs/heads/x\x00cap\x00extra"}, ""),                  // second NUL inside caps
		buildBody([]string{"z" + strings.Repeat("1", 39) + " " + oidB + " refs/heads/x\x00caps"}, ""), // OID-length field, non-hex char
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ParseReceivePackCommands(bytes.NewReader(data))
		if err != nil {
			// Fail closed: nothing partial escapes on error.
			if len(p.Commands) != 0 || p.Caps != "" || p.Prefix != nil {
				t.Fatalf("error return must be zero-value, got %+v (err=%v)", p, err)
			}
			return
		}

		// Success invariants: the caller trusts these downstream.
		if len(p.Commands) == 0 {
			t.Fatalf("successful parse with zero commands must be impossible")
		}
		for _, c := range p.Commands {
			if !isOID(c.OldOID) || !isOID(c.NewOID) {
				t.Fatalf("accepted command has non-OID field: %+v", c)
			}
			if c.RefName == "" || hasControlChars(c.RefName) {
				t.Fatalf("accepted command has empty/control-char refname: %q", c.RefName)
			}
		}
		if strings.ContainsRune(p.Caps, '\x00') {
			t.Fatalf("Caps must not contain NUL: %q", p.Caps)
		}

		// Round-trip: Prefix is exactly the consumed command section. Re-feeding
		// it must reparse identically and consume every byte (end at the flush).
		rr := bytes.NewReader(p.Prefix)
		p2, err2 := ParseReceivePackCommands(rr)
		if err2 != nil {
			t.Fatalf("re-parsing Prefix failed: %v", err2)
		}
		if rest, _ := io.ReadAll(rr); len(rest) != 0 {
			t.Fatalf("Prefix must end at the flush-pkt; %d bytes left over", len(rest))
		}
		if !bytes.Equal(p.Prefix, p2.Prefix) {
			t.Fatalf("Prefix not stable across re-parse")
		}
		if len(p.Commands) != len(p2.Commands) {
			t.Fatalf("command count changed on re-parse: %d vs %d", len(p.Commands), len(p2.Commands))
		}
		for i := range p.Commands {
			if p.Commands[i] != p2.Commands[i] {
				t.Fatalf("command %d changed on re-parse: %+v vs %+v", i, p.Commands[i], p2.Commands[i])
			}
		}

		// IssueFromRef and IsDelete must also never panic on accepted commands.
		for _, c := range p.Commands {
			_, _ = IssueFromRef(c.RefName)
			_ = c.IsDelete()
		}
	})
}
