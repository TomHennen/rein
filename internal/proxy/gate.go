// gate.go — the issue-declaration gate (issue #35 §2/§3/§5): the
// declare.rein.internal virtual-host handler, the pre-declaration
// synthesized denies (advertisement ERR + 403 JSON), and the
// post-approval receive-pack arm (parse → convention → confirmed-set
// cross-check → stream-relay or report-status `ng`).
//
// Prompts NEVER fire inside a relayed request: the proxy decides every
// request from recorded state (the run's confirmed-issue set, read via
// the hooks below); the only place a human is consulted is the declare
// handler — and that request is local-only, never relayed.
package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/brokercore"
	"github.com/TomHennen/rein/internal/classify"
)

// DeclareOutcome is what the broker-side declare hook reports back for
// one declaration attempt. Message crosses back into the sandbox and
// must never carry a token (internal/declare builds it from fixed
// strings + issue number + repo only).
type DeclareOutcome struct {
	OK      bool
	Issue   int
	Message string
	Audit   string // audit decision tag (declare.Audit* constants)
}

// DeclarationHooks is the proxy's window onto the run's declaration
// state. All three hooks read/act on the per-run approval record OUTSIDE
// the sandbox; the proxy itself holds no issue state.
//
// Fail-closed contract: a nil hook inside a non-nil DeclarationHooks
// means "deny" (WriteApproved/IssueConfirmed) or "declare unavailable"
// (Declare). A nil DeclarationHooks on Config disables the #35 gate
// entirely — permitted only for bare-relay unit tests; runbroker.Start
// refuses to start a production run without it.
type DeclarationHooks struct {
	// WriteApproved reports whether the run's confirmed-issue set is
	// non-empty (and the record signature still matches the live
	// session). Consulted for every write-tier request.
	WriteApproved func(repo string) bool

	// IssueConfirmed reports whether issue n is confirmed for repo —
	// the push-ref cross-check (§5.3).
	IssueConfirmed func(repo string, n int) bool

	// Declare performs one declaration (fetch + Form A prompt + record)
	// and BLOCKS until the human decides. Runs out-of-sandbox.
	Declare func(issue int, repo string) DeclareOutcome
}

// Deny-path bounds (§5.4): before closing a connection on a
// post-approval push deny, drain the client's in-flight upload so the
// synthesized report-status isn't clobbered by a RST — up to a hard cap.
const (
	denyDrainByteCap = 8 << 20 // 8 MiB
	denyDrainTime    = 10 * time.Second
)

// declareIssueParam is the strict grammar for the ?issue= query param —
// the same number shape the ref convention accepts (§5.1).
var declareIssueParam = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

// undeclaredWriteMsg is the instructive deny for non-git write channels
// (REST/GraphQL/uploads/raw clients). gh surfaces the message field.
const undeclaredWriteMsg = "rein: no issue declared for this run. Run: rein declare <n> (the issue this work is for), approve on your terminal, then retry."

// undeclaredPushMsg is the pkt-line ERR text for a pre-declaration push
// advertisement; git prints it verbatim after "fatal: remote error: ".
const undeclaredPushMsg = "rein: writes are locked until you declare your issue. Run: rein declare <n> (then push to agent/<n>/<nonce>)"

// serveDeclare answers the declare.rein.internal virtual host. Local
// only — nothing here ever touches the upstream transport, and no
// response carries a token.
func (p *Proxy) serveDeclare(conn net.Conn, req *http.Request) bool {
	// Discard any request body, bounded (the endpoint takes query params
	// only; a GET normally has none).
	if req.Body != nil {
		_, _ = io.CopyN(io.Discard, req.Body, 4096)
		req.Body.Close()
	}

	if p.decl == nil || p.decl.Declare == nil {
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: DeclareHost, Method: req.Method, Path: req.URL.Path, Decision: "refused-declare-unavailable"})
		p.writeLocalJSON(conn, http.StatusForbidden, "rein: declare is not available in this run")
		return false
	}
	if req.Method != http.MethodGet || req.URL.Path != "/v1/declare" {
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: DeclareHost, Method: req.Method, Path: req.URL.Path, Decision: "refused-declare-invalid"})
		p.writeLocalJSON(conn, http.StatusNotFound, "rein: unknown declare endpoint (want GET /v1/declare?issue=<n>[&repo=owner/name])")
		return false
	}
	q := req.URL.Query()
	issueStr := q.Get("issue")
	repo := q.Get("repo")
	if !declareIssueParam.MatchString(issueStr) || len(repo) > 200 {
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: DeclareHost, Method: req.Method, Path: req.URL.Path, Decision: "refused-declare-invalid"})
		p.writeLocalJSON(conn, http.StatusForbidden, "rein: issue must be a positive integer with no leading zeros (rein declare <n>)")
		return false
	}
	issue, err := strconv.Atoi(issueStr)
	if err != nil {
		p.writeLocalJSON(conn, http.StatusForbidden, "rein: unparseable issue number")
		return false
	}

	p.logger.Printf("declare: agent declared issue #%d (repo=%q); prompting", issue, repo)
	p.audit.Record(AuditEntry{Session: p.sessionID, Host: DeclareHost, Method: req.Method, Path: req.URL.Path, Issue: issue, Decision: "declared"})

	// BLOCKS while the human decides (unbounded approval-pause
	// discipline; the prompt has its own timeout).
	out := p.decl.Declare(issue, repo)

	decision := out.Audit
	if decision == "" {
		decision = "refused-declare-denied"
	}
	p.audit.Record(AuditEntry{Session: p.sessionID, Host: DeclareHost, Method: req.Method, Path: req.URL.Path, Issue: issue, Decision: decision})

	if out.OK {
		body := marshalNoEscape(struct {
			Confirmed int    `json:"confirmed"`
			Message   string `json:"message"`
		}{out.Issue, out.Message})
		p.writeLocalRaw(conn, http.StatusOK, "application/json", body)
		return false
	}
	msg := out.Message
	if msg == "" {
		msg = "rein: declaration denied"
	}
	p.writeLocalJSON(conn, http.StatusForbidden, msg)
	return false
}

// serveUndeclaredWrite answers a write-tier request while the run's
// confirmed-issue set is empty (§2's per-channel table). Local answers
// only: zero upload, zero mint, GitHub never contacted.
func (p *Proxy) serveUndeclaredWrite(conn net.Conn, req *http.Request, sni, tier string) bool {
	// git push advertisement (GET info/refs?service=git-receive-pack):
	// synthesize a local advertisement carrying a pkt-line ERR — git
	// prints it verbatim and exits cleanly, and retries fine after a
	// declare (§5.3 row 1).
	if sni == "github.com" && req.Method == http.MethodGet &&
		strings.HasSuffix(req.URL.Path, "/info/refs") &&
		req.URL.Query().Get("service") == "git-receive-pack" {
		p.logger.Printf("undeclared write: synthesizing ERR advertisement for %s", req.URL.Path)
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Tier: tier, Decision: "refused-undeclared"})
		p.writeLocalRaw(conn, http.StatusOK, "application/x-git-receive-pack-advertisement",
			SynthesizeAdvertisementERR(undeclaredPushMsg))
		return false
	}
	// Everything else (REST writes, GraphQL mutations, uploads, raw
	// clients, and the degenerate direct receive-pack POST): local 403
	// with the JSON message gh/raw clients surface. For a non-graphql
	// request no 100-continue has been sent, so an Expect client gets
	// this instead of an upload invitation (C2 preserved).
	p.logger.Printf("undeclared write: local 403 for %s %s%s", req.Method, sni, req.URL.Path)
	p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Tier: tier, Decision: "refused-undeclared"})
	p.writeLocalJSON(conn, http.StatusForbidden, undeclaredWriteMsg)
	return false
}

// serveReceivePack is the post-approval push arm (§5.3): parse the
// command section, enforce the agent/{issue}/{nonce} convention, require
// exactly one issue per push, cross-check it against the confirmed set
// for the push-target repo, then mint + stream-relay — or deny with a
// synthesized report-status the client can actually read.
func (p *Proxy) serveReceivePack(conn net.Conn, req *http.Request, sni, repo string, class hostClass, tier string) bool {
	// The refs live in the body, so we must invite it before deciding
	// (the one place C2's decide-before-invite cannot hold). The §5.4
	// drain cap bounds what a denied push can make us read.
	if !p.handleExpectContinue(conn, req) {
		return false
	}
	cmds, err := ParseReceivePackCommands(req.Body)
	if err != nil {
		p.logger.Printf("receive-pack: unparseable command section: %v", err)
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Tier: tier, Decision: "refused-receivepack-malformed"})
		p.writeLocalError(conn, http.StatusForbidden, "rein: malformed git-receive-pack request; refusing")
		return false
	}

	refs := make([]string, 0, len(cmds.Commands))
	issues := map[int]bool{}
	badConvention := false
	for _, c := range cmds.Commands {
		refs = append(refs, c.RefName)
		if n, ok := IssueFromRef(c.RefName); ok {
			issues[n] = true
		} else {
			badConvention = true
		}
	}

	switch {
	case badConvention:
		// Whole push denied (atomic): the git transport can never touch
		// main, tags, or any non-convention ref — including deletes.
		return p.denyPush(conn, req, sni, tier, cmds, refs, 0, "refused-ref-convention", func(ref string) string {
			if _, ok := IssueFromRef(ref); ok {
				return "rein: push denied: another ref in this push does not match agent/<issue>/<nonce>"
			}
			return "rein: refs must match agent/<issue>/<nonce> (e.g. agent/73/kx3q)"
		})
	case len(issues) > 1:
		return p.denyPush(conn, req, sni, tier, cmds, refs, 0, "refused-multi-issue", func(string) string {
			return "rein: one issue per push; split your push (one issue per push, several pushes per run)"
		})
	}
	var issue int
	for n := range issues {
		issue = n
	}
	confirmed := p.decl != nil && p.decl.IssueConfirmed != nil && p.decl.IssueConfirmed(repo, issue)
	if !confirmed {
		msg := fmt.Sprintf("rein: issue #%d is not confirmed for this run; run: rein declare %d", issue, issue)
		return p.denyPush(conn, req, sni, tier, cmds, refs, issue, "refused-issue-unconfirmed", func(string) string { return msg })
	}

	// Verified. Mint via the shared core (scope + confirm + mint — the
	// confirm hook re-reads the same record; cheap and consistent).
	cred := p.core.Serve(context.Background(), brokercore.Request{Repo: repo, WriteIntent: true})
	switch cred.Password {
	case brokercore.PlaceholderRefused:
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Tier: tier, Issue: issue, Refs: refs, Decision: "refused-scope"})
		p.writeLocalError(conn, http.StatusForbidden,
			"rein: this repository is out of the session's scope, or a write was not approved. Run `rein doctor`.")
		return false
	case brokercore.PlaceholderMintFailed:
		p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Tier: tier, Issue: issue, Refs: refs, Decision: "mint-failed"})
		p.writeLocalError(conn, http.StatusBadGateway,
			"rein: could not mint a GitHub token for this request. Run `rein doctor`.")
		return false
	}

	// Re-assemble the body byte-identically: the parsed command section
	// prefix + the untouched packfile stream (never buffered).
	req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(cmds.Prefix), req.Body))

	authValue := "Bearer " + cred.Password
	if class == classInjectBasic {
		authValue = "Basic " + basicAuth(brokercore.CredentialUsername, cred.Password)
	}
	return p.relay(conn, req, sni, authValue, "inject", tier, issue, refs)
}

// denyPush delivers a §5.4 deny: drain the in-flight upload (bounded),
// then answer with a synthesized report-status (`unpack ok` + `ng` per
// ref, side-band-wrapped iff negotiated) that git prints as
// `! [remote rejected] <ref> (<reason>)`. The connection closes after.
func (p *Proxy) denyPush(conn net.Conn, req *http.Request, sni, tier string, cmds PushCommands, refs []string, issue int, decision string, reason func(string) string) bool {
	deadline := time.Now().Add(denyDrainTime)
	_ = conn.SetReadDeadline(deadline)
	_, _ = io.CopyN(io.Discard, req.Body, denyDrainByteCap)
	_ = conn.SetReadDeadline(time.Time{})

	p.logger.Printf("receive-pack DENY (%s): refs=%v issue=%d", decision, refs, issue)
	p.audit.Record(AuditEntry{Session: p.sessionID, Host: sni, Method: req.Method, Path: req.URL.Path, Tier: tier, Issue: issue, Refs: refs, Decision: decision})
	p.writeLocalRaw(conn, http.StatusOK, "application/x-git-receive-pack-result",
		SynthesizeNgReport(cmds.Commands, cmds.SideBand(), reason))
	return false
}

// writeLocalJSON writes a local JSON error/answer body {"message": …}
// toward the client and closes the connection. Fixed strings + caller
// message only — never a token (response-path hygiene).
func (p *Proxy) writeLocalJSON(conn net.Conn, status int, message string) {
	p.writeLocalRaw(conn, status, "application/json", marshalNoEscape(struct {
		Message string `json:"message"`
	}{message}))
}

// marshalNoEscape marshals without HTML escaping so instructive text
// like `rein declare <n>` stays readable to raw clients (gh decodes
// either way; curl users see the literal).
func marshalNoEscape(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// writeLocalRaw writes a complete local HTTP/1.1 response with the given
// body and closes the keep-alive (Connection: close). Mirrors
// writeLocalError's deadline discipline.
func (p *Proxy) writeLocalRaw(conn net.Conn, status int, contentType string, body []byte) {
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	fmt.Fprintf(conn,
		"HTTP/1.1 %d %s\r\nContent-Type: %s\r\nCache-Control: no-cache\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
		status, http.StatusText(status), contentType, len(body))
	_, _ = conn.Write(body)
}

// repoAllowedByScope reports whether the declaration gate should treat
// this repo as in-scope for gate purposes: unknown repos ("" — e.g.
// /graphql) are gate-checked, out-of-scope repos fall through to the
// core's existing refused-scope answer (scope refusal outranks the
// declare hint — §2: out-of-scope repos are refused before any token is
// served, and before rein teaches the declare step).
func (p *Proxy) repoAllowedByScope(repo string) bool {
	if p.inScope == nil || repo == "" {
		return true
	}
	return p.inScope(repo)
}

// isReceivePackPOST reports whether the request is the push service POST.
func isReceivePackPOST(sni string, req *http.Request) bool {
	return sni == "github.com" && req.Method == http.MethodPost &&
		strings.HasSuffix(req.URL.Path, "/git-receive-pack")
}

// tierWrite is a tiny helper for gate call sites.
func tierWrite(tier classify.Tier) bool { return tier == classify.Write }

// basicAuth encodes a Basic authorization value.
func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}
