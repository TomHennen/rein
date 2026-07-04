package proxy

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// AuditLog is a plain, append-only, token-REDACTED record of proxy decisions.
//
// PLAN-1 CP2 originally called for a hash-chained log; the team simplified to
// plain append-only for v1 (hash-chaining without an external anchor adds
// little — a local attacker who can rewrite the file can also recompute the
// chain; recorded in PLAN-1 Notes). The file is in the sandbox deny-read set
// (design §4.1), and NO token value is ever written here (design §4.1
// response-path hygiene) — only metadata: ts, session, host, method, path,
// tier, decision, status.
type AuditLog struct {
	mu sync.Mutex
	w  io.Writer
}

// NewAuditLog writes entries to w (typically an append-mode file). A nil w
// yields a no-op log — auditing is best-effort and must never block a request.
func NewAuditLog(w io.Writer) *AuditLog {
	return &AuditLog{w: w}
}

// AuditEntry is one recorded proxy decision. It deliberately has no token
// field: a token must never reach the audit path.
type AuditEntry struct {
	Session  string
	Host     string
	Method   string
	Path     string
	Tier     string // "read" | "write" | "" (never-inject / refused)
	Decision string // e.g. "inject", "passthrough", "refused-scope", "refused-host"
	Status   int    // upstream HTTP status, or 0 when no upstream request was made
}

// Record appends one entry. Best-effort: a write error is swallowed (a broken
// audit sink must not break the relay). Safe for concurrent use.
func (a *AuditLog) Record(e AuditEntry) {
	if a == nil || a.w == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	status := "-"
	if e.Status != 0 {
		status = fmt.Sprintf("%d", e.Status)
	}
	fmt.Fprintf(a.w, "%s session=%s host=%s method=%s path=%s tier=%s decision=%s status=%s\n",
		time.Now().UTC().Format(time.RFC3339), redactField(e.Session), redactField(e.Host),
		redactField(e.Method), redactField(e.Path), fieldOrDash(e.Tier),
		fieldOrDash(e.Decision), status)
}

func fieldOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// redactField sanitizes an audit field so an agent-controlled value (SNI, the
// decoded URL path) can't forge a second log line or corrupt an operator's
// terminal. It does NOT redact secrets — no secret is ever passed to Record —
// but it replaces ALL C0 control characters (incl. CR/LF, ESC, NUL, backspace)
// and DEL with a space, keeping every entry on one clean line.
func redactField(s string) string {
	if s == "" {
		return "-"
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		out = append(out, r)
	}
	return string(out)
}
