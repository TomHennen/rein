// receivepack.go — git receive-pack wire handling for push-ref
// verification (issue #35 §5): a byte-precise parser for the pkt-line
// command section of a `POST /git-receive-pack` body, the strict
// agent/{issue}/{nonce} ref convention, and the two synthesized
// responses rein answers pushes with locally:
//
//   - a receive-pack ADVERTISEMENT carrying a pkt-line ERR (the
//     pre-declaration gate: git prints `fatal: remote error: …` and
//     exits cleanly — zero upload, zero mint, GitHub never contacted);
//   - a report-status body of `unpack ok` + `ng <ref> <reason>` lines
//     (post-approval convention/cross-check denies: git prints
//     `! [remote rejected] <ref> (<reason>)`), side-band-wrapped iff
//     the client negotiated side-band.
//
// The parser reads EXACTLY the command section (never a byte of the
// packfile): the caller relays io.MultiReader(prefix, rest) upstream so
// the pack is streamed, never buffered. Push is protocol v0/v1 only
// (v2 has no push); GitHub does not advertise push-cert; delete-only
// pushes simply have no packfile after the flush — all shapes reduce to
// "commands, then flush-pkt".
//
// Everything here fails closed: malformed pkt framing, oversized
// command sections, invalid OIDs, or control characters in a refname
// are errors the caller turns into a deny.
package proxy

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// maxCommandSection caps the bytes we are willing to buffer for the
// command section (issue #35 §5.2). Real pushes carry a handful of
// commands; 64 KiB is ~450 refs — far beyond any agent workflow.
const maxCommandSection = 64 * 1024

// ErrCommandSectionTooLarge: the command section exceeded the cap.
var ErrCommandSectionTooLarge = errors.New("receive-pack command section exceeds 64 KiB cap")

// RefCommand is one parsed update command: old-oid SP new-oid SP refname.
type RefCommand struct {
	OldOID  string
	NewOID  string
	RefName string
}

// IsDelete reports whether the command deletes its ref (new oid all-zero).
func (c RefCommand) IsDelete() bool { return isZeroOID(c.NewOID) }

// PushCommands is the parsed command section of one receive-pack POST.
type PushCommands struct {
	Commands []RefCommand
	// Caps is the capability list the client chose (after the NUL on the
	// first command line): e.g. "report-status side-band-64k agent=git/2.43".
	Caps string
	// Prefix is the raw bytes consumed (shallow lines + commands + the
	// flush-pkt), so the caller can relay MultiReader(Prefix, rest) with
	// the body byte-identical.
	Prefix []byte
}

// SideBand reports whether the client negotiated side-band framing for
// the report-status response (side-band-64k or side-band).
func (p PushCommands) SideBand() bool {
	for _, c := range strings.Fields(p.Caps) {
		if c == "side-band-64k" || c == "side-band" {
			return true
		}
	}
	return false
}

// ParseReceivePackCommands reads the pkt-line command section from r:
// optional leading `shallow <oid>` lines, then `<old> <new> <ref>` update
// commands (first carries `\0<caps>`), terminated by a flush-pkt. It
// reads EXACTLY through the flush-pkt and not a byte further.
func ParseReceivePackCommands(r io.Reader) (PushCommands, error) {
	var out PushCommands
	var prefix []byte
	sawCommand := false
	for {
		if len(prefix) > maxCommandSection {
			return PushCommands{}, ErrCommandSectionTooLarge
		}
		pkt, raw, err := readPktLine(r)
		if err != nil {
			return PushCommands{}, fmt.Errorf("receive-pack command section: %w", err)
		}
		prefix = append(prefix, raw...)
		if pkt == nil { // flush-pkt — end of command section
			break
		}
		line := strings.TrimSuffix(string(pkt), "\n")
		if strings.HasPrefix(line, "shallow ") {
			if sawCommand {
				return PushCommands{}, errors.New("receive-pack: shallow line after commands")
			}
			if !isOID(strings.TrimPrefix(line, "shallow ")) {
				return PushCommands{}, errors.New("receive-pack: malformed shallow line")
			}
			continue
		}
		// First command line may carry "\0caps".
		if !sawCommand {
			if line, out.Caps, _ = strings.Cut(line, "\x00"); out.Caps != "" && strings.ContainsAny(out.Caps, "\x00") {
				return PushCommands{}, errors.New("receive-pack: malformed capability list")
			}
		} else if strings.Contains(line, "\x00") {
			return PushCommands{}, errors.New("receive-pack: NUL outside first command line")
		}
		cmd, err := parseCommandLine(line)
		if err != nil {
			return PushCommands{}, err
		}
		out.Commands = append(out.Commands, cmd)
		sawCommand = true
	}
	if len(out.Commands) == 0 {
		// A push with no commands authorizes nothing and is not a shape
		// git produces; refuse rather than relay something unvetted.
		return PushCommands{}, errors.New("receive-pack: no update commands before flush")
	}
	out.Prefix = prefix
	return out, nil
}

// parseCommandLine parses "<old-oid> <new-oid> <refname>".
func parseCommandLine(line string) (RefCommand, error) {
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return RefCommand{}, fmt.Errorf("receive-pack: malformed command line (%d fields)", len(parts))
	}
	cmd := RefCommand{OldOID: parts[0], NewOID: parts[1], RefName: parts[2]}
	if !isOID(cmd.OldOID) || !isOID(cmd.NewOID) {
		return RefCommand{}, errors.New("receive-pack: malformed object id in command")
	}
	if cmd.RefName == "" || hasControlChars(cmd.RefName) {
		return RefCommand{}, errors.New("receive-pack: malformed refname in command")
	}
	return cmd, nil
}

// readPktLine reads one pkt-line. Returns (payload, rawBytes, err);
// payload nil (with raw "0000") means flush-pkt. Reads are exact — no
// buffering beyond the line itself.
func readPktLine(r io.Reader) (payload, raw []byte, err error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, nil, fmt.Errorf("read pkt length: %w", err)
	}
	n, err := strconv.ParseUint(string(hdr), 16, 32)
	if err != nil {
		return nil, nil, fmt.Errorf("bad pkt length %q", hdr)
	}
	switch {
	case n == 0:
		return nil, hdr, nil // flush-pkt
	case n < 4:
		// 0001-0003 are reserved (delim-pkt etc. — protocol v2 shapes that
		// have no business in a v0/v1 push). Fail closed.
		return nil, nil, fmt.Errorf("reserved pkt length %q", hdr)
	case n == 4:
		return []byte{}, hdr, nil // empty pkt (degenerate but framed)
	case n > 65520:
		return nil, nil, fmt.Errorf("oversized pkt length %q", hdr)
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, nil, fmt.Errorf("read pkt body: %w", err)
	}
	return body, append(hdr, body...), nil
}

// --- ref convention (issue #35 §5.1) ---

// agentRefPattern is the strict push-ref convention: the agent encodes
// its issue in the branch (design.md:521). Anchored; leading zeros
// rejected; issue 0 rejected (GitHub numbers issues from 1, and `rein
// declare 0` is refused everywhere — so a matching ref could never be
// confirmed anyway); nonce is agent-chosen and format-checked only
// (branch uniqueness — the security nonce is the unrelated REIN_RUN_ID).
var agentRefPattern = regexp.MustCompile(`^refs/heads/agent/([1-9][0-9]{0,9})/([A-Za-z0-9][A-Za-z0-9._-]{0,63})$`)

// IssueFromRef extracts the issue number a ref claims under the
// agent/{issue}/{nonce} convention. ok=false when the ref does not
// match the convention (including leading-zero and malformed-nonce
// forms — the caller denies the whole push).
func IssueFromRef(ref string) (issue int, ok bool) {
	m := agentRefPattern.FindStringSubmatch(ref)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil { // unreachable given the pattern; belt-and-suspenders
		return 0, false
	}
	return n, true
}

// --- synthesized responses ---

// SynthesizeAdvertisementERR builds a receive-pack advertisement body
// whose first payload pkt is a pkt-line ERR (issue #35 §5.3, the
// pre-declaration gate). git's remote-curl prints the message verbatim
// as `fatal: remote error: <msg>` and exits cleanly; nothing is
// uploaded and no retry loop is triggered.
func SynthesizeAdvertisementERR(msg string) []byte {
	var b []byte
	b = appendPkt(b, "# service=git-receive-pack\n")
	b = append(b, "0000"...)
	b = appendPkt(b, "ERR "+sanitizePktText(msg)+"\n")
	b = append(b, "0000"...)
	return b
}

// SynthesizeNgReport builds a report-status response denying every
// command: `unpack ok` + one `ng <ref> <reason>` per ref (issue #35
// §5.4). git prints each as `! [remote rejected] <ref> (<reason>)`.
// When the client negotiated side-band (sideBand), the whole report is
// wrapped in band-1 pkts as the protocol requires — an unwrapped report
// would be garbage to a side-band client.
//
// reason receives each refname and returns the deny reason; both refs
// (client-sent) and reasons (fixed strings + refs) are sanitized so the
// synthesized body can never smuggle framing or control bytes. Never a
// token: the inputs are the client's own refs and fixed strings.
func SynthesizeNgReport(cmds []RefCommand, sideBand bool, reason func(ref string) string) []byte {
	var report []byte
	report = appendPkt(report, "unpack ok\n")
	for _, c := range cmds {
		ref := sanitizePktText(c.RefName)
		report = appendPkt(report, "ng "+ref+" "+sanitizePktText(reason(c.RefName))+"\n")
	}
	report = append(report, "0000"...)

	if !sideBand {
		return report
	}
	// Side-band framing: the report travels on band 1, chunked under the
	// 64k pkt ceiling, followed by a flush.
	var out []byte
	const chunk = 65500
	for off := 0; off < len(report); off += chunk {
		end := off + chunk
		if end > len(report) {
			end = len(report)
		}
		out = appendPktBytes(out, append([]byte{1}, report[off:end]...))
	}
	out = append(out, "0000"...)
	return out
}

// appendPkt appends one pkt-line with a string payload.
func appendPkt(b []byte, s string) []byte {
	return appendPktBytes(b, []byte(s))
}

func appendPktBytes(b, payload []byte) []byte {
	return append(append(b, []byte(fmt.Sprintf("%04x", len(payload)+4))...), payload...)
}

// sanitizePktText strips control characters (incl. NUL/CR/LF/ESC) from
// text echoed into a synthesized pkt so a hostile refname can't inject
// framing, extra report lines, or terminal escapes into git's output.
func sanitizePktText(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		out = append(out, r)
	}
	return string(out)
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// isOID reports whether s looks like a git object id: 40 (sha1) or 64
// (sha256) hex chars. The all-zero id (create/delete) is a valid oid.
func isOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func isZeroOID(s string) bool {
	for _, c := range s {
		if c != '0' {
			return false
		}
	}
	return len(s) == 40 || len(s) == 64
}
