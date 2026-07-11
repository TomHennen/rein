package proxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// gateHooks builds DeclarationHooks around a mutable confirmed set.
type gateState struct {
	approved atomic.Bool
	issues   map[int]bool // confirmed issue numbers (repo-agnostic for tests)
	declared []int
	declOut  DeclareOutcome
}

func (g *gateState) hooks() *DeclarationHooks {
	return &DeclarationHooks{
		WriteApproved:  func(string) bool { return g.approved.Load() },
		IssueConfirmed: func(_ string, n int) bool { return g.issues[n] },
		Declare: func(n int, repo string) DeclareOutcome {
			g.declared = append(g.declared, n)
			return g.declOut
		},
	}
}

// --- pre-declaration synthesized denies (§2 table) ---

func TestGate_PreDeclarationPushAdvertisementERR(t *testing.T) {
	g := &gateState{}
	h := newHarness(t, harnessOpts{decl: g.hooks()})
	c := h.httpClient(false)

	resp, body := doGET(t, c, "https://github.com/o/r.git/info/refs?service=git-receive-pack")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a synthesized advertisement, not an HTTP error)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-git-receive-pack-advertisement" {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(body, "ERR rein: writes are locked until you declare your issue") {
		t.Errorf("body missing the pkt-line ERR:\n%q", body)
	}
	if !strings.Contains(body, "rein declare <n>") {
		t.Errorf("deny must name the exact next step:\n%q", body)
	}
	// Zero mint, zero upstream contact.
	if h.gh.count() != 0 {
		t.Error("GitHub must NEVER be contacted for a pre-declaration push advertisement")
	}
	if atomic.LoadInt32(&h.writeCalls) != 0 {
		t.Error("no write token may be minted pre-declaration")
	}
}

func TestGate_PreDeclarationRESTWrite403JSON(t *testing.T) {
	g := &gateState{}
	h := newHarness(t, harnessOpts{decl: g.hooks()})
	c := h.httpClient(false)

	req, _ := http.NewRequest("POST", "https://api.github.com/repos/o/r/issues", strings.NewReader(`{"title":"x"}`))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json (gh surfaces the message field)", ct)
	}
	if !strings.Contains(string(body), `"message"`) || !strings.Contains(string(body), "rein declare <n>") {
		t.Errorf("403 body must be JSON naming the declare step:\n%s", body)
	}
	if h.gh.count() != 0 {
		t.Error("GitHub must not be contacted for a pre-declaration REST write")
	}
}

func TestGate_PreDeclarationGraphQLMutation403(t *testing.T) {
	g := &gateState{}
	h := newHarness(t, harnessOpts{decl: g.hooks()})
	c := h.httpClient(false)

	req, _ := http.NewRequest("POST", "https://api.github.com/graphql",
		strings.NewReader(`{"query":"mutation { addComment(input:{}) { clientMutationId } }"}`))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("graphql mutation pre-declaration: status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(string(body), "no issue declared") {
		t.Errorf("unexpected body: %s", body)
	}
	if h.gh.count() != 0 {
		t.Error("mutation must not reach GitHub")
	}
}

func TestGate_ReadsUnaffectedPreDeclaration(t *testing.T) {
	g := &gateState{}
	h := newHarness(t, harnessOpts{decl: g.hooks()})
	c := h.httpClient(false)

	resp, _ := doGET(t, c, "https://github.com/o/r.git/info/refs?service=git-upload-pack")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read status = %d, want 200 relay", resp.StatusCode)
	}
	if h.gh.count() != 1 {
		t.Error("reads must relay as today (TM-G8 untouched)")
	}
	if got := h.gh.last().Auth; !strings.Contains(got, "Basic") {
		t.Errorf("read should carry the injected read credential, got %q", got)
	}
}

func TestGate_OutOfScopeOutranksDeclareHint(t *testing.T) {
	g := &gateState{}
	h := newHarness(t, harnessOpts{repos: []string{"o/r"}, decl: g.hooks()})
	c := h.httpClient(false)

	resp, body := doGET(t, c, "https://github.com/evil/other.git/info/refs?service=git-receive-pack")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want the refused-scope 403", resp.StatusCode)
	}
	if strings.Contains(body, "declare") {
		t.Errorf("out-of-scope refusal must NOT teach the declare step (scope outranks): %q", body)
	}
	if !strings.Contains(body, "out of the session's scope") {
		t.Errorf("expected the refused-scope message, got %q", body)
	}
}

// --- declare virtual host (§3) ---

func TestGate_DeclareVirtualHost(t *testing.T) {
	g := &gateState{declOut: DeclareOutcome{OK: true, Issue: 73, Message: "issue #73 in o/r confirmed", Audit: "confirmed-issue"}}
	audit := &syncBuffer{}
	h := newHarnessWithAudit(t, harnessOpts{decl: g.hooks()}, audit)
	c := h.httpClient(false)

	resp, body := doGET(t, c, "https://declare.rein.internal/v1/declare?issue=73")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("declare status = %d body=%q", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"confirmed":73`) {
		t.Errorf("200 body must carry {\"confirmed\":73}: %q", body)
	}
	if len(g.declared) != 1 || g.declared[0] != 73 {
		t.Errorf("declare hook calls = %v, want [73]", g.declared)
	}
	if h.gh.count() != 0 {
		t.Error("the declare host must NEVER be relayed upstream")
	}
	for _, want := range []string{"decision=declared", "decision=confirmed-issue", "issue=73"} {
		if !strings.Contains(audit.String(), want) {
			t.Errorf("audit missing %q:\n%s", want, audit.String())
		}
	}
}

func TestGate_DeclareDenied403(t *testing.T) {
	g := &gateState{declOut: DeclareOutcome{OK: false, Issue: 74, Message: "rein: issue #74 not found in o/r", Audit: "refused-issue-unverified"}}
	h := newHarness(t, harnessOpts{decl: g.hooks()})
	c := h.httpClient(false)

	resp, body := doGET(t, c, "https://declare.rein.internal/v1/declare?issue=74")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(body, "not found") {
		t.Errorf("deny body must surface the reason: %q", body)
	}
}

func TestGate_DeclareRejectsBadInputs(t *testing.T) {
	g := &gateState{declOut: DeclareOutcome{OK: true, Issue: 1}}
	h := newHarness(t, harnessOpts{decl: g.hooks()})
	c := h.httpClient(false)

	for _, u := range []string{
		"https://declare.rein.internal/v1/declare?issue=073",
		"https://declare.rein.internal/v1/declare?issue=0",
		"https://declare.rein.internal/v1/declare?issue=abc",
		"https://declare.rein.internal/v1/declare",
		"https://declare.rein.internal/other/path?issue=73",
	} {
		resp, _ := doGET(t, c, u)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s: got 200, want a refusal", u)
		}
	}
	if len(g.declared) != 0 {
		t.Errorf("no malformed declare may reach the hook, got %v", g.declared)
	}
}

func TestGate_DeclareUnavailableFailsClosed(t *testing.T) {
	g := &gateState{}
	hooks := g.hooks()
	hooks.Declare = nil
	h := newHarness(t, harnessOpts{decl: hooks})
	c := h.httpClient(false)
	resp, _ := doGET(t, c, "https://declare.rein.internal/v1/declare?issue=73")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("nil Declare hook must 403, got %d", resp.StatusCode)
	}
}

// --- post-approval receive-pack cross-check (§5.3) ---

// pushBody builds a receive-pack POST body with the given refs + a fake pack.
func pushBody(caps string, refs ...string) string {
	lines := make([]string, len(refs))
	for i, ref := range refs {
		lines[i] = oidA + " " + oidB + " " + ref
	}
	if len(lines) > 0 && caps != "" {
		lines[0] += "\x00" + caps
	}
	return buildBody(lines, "FAKEPACKDATA")
}

func postPush(t *testing.T, c *http.Client, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("POST", "https://github.com/o/r.git/git-receive-pack", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST receive-pack: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func approvedGate(issues ...int) *gateState {
	g := &gateState{issues: map[int]bool{}}
	g.approved.Store(true)
	for _, n := range issues {
		g.issues[n] = true
	}
	return g
}

func TestGate_ConfirmedPushRelaysWithPackIntact(t *testing.T) {
	g := approvedGate(73)
	audit := &syncBuffer{}
	h := newHarnessWithAudit(t, harnessOpts{decl: g.hooks()}, audit)
	c := h.httpClient(false)

	body := pushBody("report-status", "refs/heads/agent/73/kx3q")
	resp, _ := postPush(t, c, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirmed push: status = %d, want 200 relay", resp.StatusCode)
	}
	if h.gh.count() != 1 {
		t.Fatal("confirmed push must relay upstream")
	}
	last := h.gh.last()
	if last.BodyN != len(body) {
		t.Errorf("upstream body = %d bytes, want %d (prefix + pack must be byte-identical)", last.BodyN, len(body))
	}
	if !strings.Contains(last.Auth, "Basic ") {
		t.Errorf("relayed push must carry the injected write credential, got %q", last.Auth)
	}
	for _, want := range []string{"decision=inject", "issue=73", "refs=refs/heads/agent/73/kx3q"} {
		if !strings.Contains(audit.String(), want) {
			t.Errorf("audit missing %q:\n%s", want, audit.String())
		}
	}
}

func TestGate_NonConventionRefDenied(t *testing.T) {
	g := approvedGate(73)
	audit := &syncBuffer{}
	h := newHarnessWithAudit(t, harnessOpts{decl: g.hooks()}, audit)
	c := h.httpClient(false)

	for _, ref := range []string{"refs/heads/main", "refs/tags/v1.0", "refs/heads/agent/073/x", "refs/heads/feature/foo"} {
		resp, body := postPush(t, c, pushBody("report-status", ref))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("ref %s: status = %d, want 200 (a report-status the client can read)", ref, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/x-git-receive-pack-result" {
			t.Errorf("ref %s: content-type = %q", ref, ct)
		}
		if !strings.Contains(body, "unpack ok") || !strings.Contains(body, "ng "+ref) {
			t.Errorf("ref %s: expected ng report, got %q", ref, body)
		}
		if !strings.Contains(body, "agent/<issue>/<nonce>") {
			t.Errorf("ref %s: deny must teach the convention: %q", ref, body)
		}
	}
	if h.gh.count() != 0 {
		t.Error("denied pushes must never reach GitHub — the git transport cannot touch main/tags")
	}
	if !strings.Contains(audit.String(), "decision=refused-ref-convention") {
		t.Errorf("audit missing refused-ref-convention:\n%s", audit.String())
	}
}

func TestGate_UnconfirmedIssueDenied(t *testing.T) {
	g := approvedGate(73) // 73 confirmed, 74 NOT
	audit := &syncBuffer{}
	h := newHarnessWithAudit(t, harnessOpts{decl: g.hooks()}, audit)
	c := h.httpClient(false)

	resp, body := postPush(t, c, pushBody("report-status", "refs/heads/agent/74/xy"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 report-status", resp.StatusCode)
	}
	if !strings.Contains(body, "ng refs/heads/agent/74/xy") || !strings.Contains(body, "rein declare 74") {
		t.Errorf("deny must name the expansion step (rein declare 74): %q", body)
	}
	if h.gh.count() != 0 {
		t.Error("unconfirmed-issue push must not reach GitHub")
	}
	if !strings.Contains(audit.String(), "decision=refused-issue-unconfirmed") {
		t.Errorf("audit missing refused-issue-unconfirmed:\n%s", audit.String())
	}
}

func TestGate_MultiIssuePushDenied(t *testing.T) {
	g := approvedGate(73, 74) // BOTH confirmed — still one issue per push (§9)
	h := newHarness(t, harnessOpts{decl: g.hooks()})
	c := h.httpClient(false)

	resp, body := postPush(t, c, pushBody("report-status",
		"refs/heads/agent/73/a", "refs/heads/agent/74/b"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(body, "one issue per push") {
		t.Errorf("expected the split-your-push deny, got %q", body)
	}
	if h.gh.count() != 0 {
		t.Error("multi-issue push must not relay")
	}
}

func TestGate_DeleteOfMatchingRefAllowed(t *testing.T) {
	g := approvedGate(73)
	h := newHarness(t, harnessOpts{decl: g.hooks()})
	c := h.httpClient(false)

	// Delete of agent/73/old (zero new-oid), no packfile after flush.
	line := oidA + " " + zeroOID + " refs/heads/agent/73/old\x00report-status"
	resp, _ := postPush(t, c, buildBody([]string{line}, ""))
	if resp.StatusCode != http.StatusOK || h.gh.count() != 1 {
		t.Errorf("delete of a confirmed agent ref should relay (status=%d upstream=%d)", resp.StatusCode, h.gh.count())
	}
}

func TestGate_DeleteOfMainDenied(t *testing.T) {
	g := approvedGate(73)
	h := newHarness(t, harnessOpts{decl: g.hooks()})
	c := h.httpClient(false)

	line := oidA + " " + zeroOID + " refs/heads/main\x00report-status"
	resp, body := postPush(t, c, buildBody([]string{line}, ""))
	if resp.StatusCode != http.StatusOK || h.gh.count() != 0 {
		t.Fatalf("delete of main must be denied locally (status=%d upstream=%d)", resp.StatusCode, h.gh.count())
	}
	if !strings.Contains(body, "ng refs/heads/main") {
		t.Errorf("expected ng for main delete: %q", body)
	}
}

func TestGate_SideBandDenyWrapped(t *testing.T) {
	g := approvedGate(73)
	h := newHarness(t, harnessOpts{decl: g.hooks()})
	c := h.httpClient(false)

	resp, body := postPush(t, c, pushBody("report-status side-band-64k", "refs/heads/main"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Band-1 framing: first pkt payload starts with \x01.
	if len(body) < 5 || body[4] != 1 {
		t.Errorf("side-band client must get a band-1-wrapped report, got %q", body[:min(20, len(body))])
	}
}

func TestGate_MalformedCommandSection403(t *testing.T) {
	g := approvedGate(73)
	h := newHarness(t, harnessOpts{decl: g.hooks()})
	c := h.httpClient(false)

	resp, _ := postPush(t, c, "zzzzgarbage")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unparseable command section: status = %d, want 403 (deny, close)", resp.StatusCode)
	}
	if h.gh.count() != 0 {
		t.Error("malformed push must not relay")
	}
}

func TestGate_AdvertisementAfterApprovalRelays(t *testing.T) {
	g := approvedGate(73)
	h := newHarness(t, harnessOpts{decl: g.hooks()})
	c := h.httpClient(false)

	resp, _ := doGET(t, c, "https://github.com/o/r.git/info/refs?service=git-receive-pack")
	if resp.StatusCode != http.StatusOK || h.gh.count() != 1 {
		t.Errorf("post-approval advertisement should mint + relay (status=%d upstream=%d)", resp.StatusCode, h.gh.count())
	}
	if atomic.LoadInt32(&h.writeCalls) != 1 {
		t.Errorf("write mints = %d, want 1 (post-approval — clean)", atomic.LoadInt32(&h.writeCalls))
	}
}

func TestGate_AuditLinesForDeniesCarryRefs(t *testing.T) {
	g := approvedGate(73)
	audit := &syncBuffer{}
	h := newHarnessWithAudit(t, harnessOpts{decl: g.hooks()}, audit)
	c := h.httpClient(false)
	postPush(t, c, pushBody("report-status", "refs/heads/agent/74/x"))
	if !strings.Contains(audit.String(), "refs=refs/heads/agent/74/x") {
		t.Errorf("deny audit row must carry the refs:\n%s", audit.String())
	}
	// And never a token value anywhere in the audit stream.
	for _, secret := range []string{h.readTok, h.writeTok} {
		if strings.Contains(audit.String(), secret) {
			t.Fatal("token value leaked into the audit log")
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// fmt is used by pushBody helpers indirectly; keep the import stable.
var _ = fmt.Sprintf

// TestGate_MintFailedWithEmptiedSetTellsAgentToReDeclare pins the review's
// S1: when the TM-G6 re-check invalidates this run's confirmation mid-push
// (the mint fails AND the confirmed set is now empty), the in-sandbox error
// must name the RIGHT remedy — re-declare — not "run `rein doctor`".
func TestGate_MintFailedWithEmptiedSetTellsAgentToReDeclare(t *testing.T) {
	// The gate passes when the request arrives (the set is still confirmed),
	// then the TM-G6 re-check inside the mint invalidates it: the mint fails
	// AND every later read of the gate reports EMPTY. That is exactly the
	// live sequence InvalidateTransferred produces.
	g := approvedGate(73)
	hooks := g.hooks()
	var gateCalls atomic.Int32
	hooks.WriteApproved = func(string) bool { return gateCalls.Add(1) == 1 }
	h := newHarness(t, harnessOpts{decl: hooks, mintWErr: errors.New("all confirmed issues were transferred")})
	c := h.httpClient(false)

	resp, body := doGET(t, c, "https://github.com/o/r.git/info/refs?service=git-receive-pack")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (a declaration problem, not a mint outage)", resp.StatusCode)
	}
	if !strings.Contains(body, "rein declare") {
		t.Errorf("the agent must be told to RE-DECLARE:\n%s", body)
	}
	if strings.Contains(body, "rein doctor") {
		t.Errorf("must NOT point at rein doctor (wrong remedy):\n%s", body)
	}
}

// TestGate_GenuineMintFailureStillPointsAtDoctor: the ordinary mint failure
// (App/network/config) keeps its 502 + doctor pointer.
func TestGate_GenuineMintFailureStillPointsAtDoctor(t *testing.T) {
	g := approvedGate(73) // set stays non-empty
	h := newHarness(t, harnessOpts{decl: g.hooks(), mintWErr: errors.New("github 500")})
	c := h.httpClient(false)

	resp, body := doGET(t, c, "https://github.com/o/r.git/info/refs?service=git-receive-pack")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for a genuine mint failure", resp.StatusCode)
	}
	if !strings.Contains(body, "rein doctor") {
		t.Errorf("a genuine mint failure must keep the doctor pointer:\n%s", body)
	}
	_ = h
}
