package nono

// Containment prober — the nono counterpart of srt/selftest.go's
// VerifyConfigApplied. It launches a probe INSIDE a real `nono run` (under the
// rein-representative profile Build emits) and turns the empirical containment
// facts measured in docs/nono-git-push-spike-findings.md into automated,
// fail-closed checks. Two entry points:
//
//   - RunContainmentProbe: runs INSIDE the sandbox (via the hidden
//     `rein __nono-probe` subcommand). It MEASURES raw facts and writes them as
//     an Observations JSON. It classifies nothing and never exfiltrates a
//     credential's CONTENT — it reports only whether each channel was open.
//   - VerifyContainment: the host-side launch gate. It stages targets, launches
//     the probe through the real nono path, reads the Observations back,
//     Classifies them into a Verdict, and returns a non-nil error when any
//     containment channel LEAKED (so the caller fails the launch closed).
//
// HONESTY — enumeration is NOT soundness. A clean run means none of the KNOWN
// channels leaked, NOT proof of confinement. This is a regression + drift
// detector for the specific channels the spike measured, not a proof the
// sandbox holds. Covert/side channels are out of scope. It is an
// enumerator/reporter, never an escape kit (CLAUDE.md #5): it reports booleans,
// not credential contents, and does not attempt to break out. See
// docs/containment-probe-harness.md "Limits (state loudly)".

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Channel names the containment property each check verifies. Each maps to a
// measured finding in docs/nono-git-push-spike-findings.md.
type Channel string

const (
	// ChanCredentials — App key / gh token / ssh keys unreadable (deny_credentials).
	ChanCredentials Channel = "credentials"
	// ChanDirectTCP — raw connect() to a non-allowlisted external host is denied.
	ChanDirectTCP Channel = "direct_tcp_egress"
	// ChanArbitraryLoopback — raw connect() to an arbitrary 127.0.0.1:port is
	// denied. THE control: it is what lets rein's loopback listener run without
	// proxy-auth. The whole no-proxy-auth decision rests on this one.
	ChanArbitraryLoopback Channel = "arbitrary_loopback"
	// ChanGitHubViaProxy — direct connect() to github's IP is denied, so github
	// egress can only go through the proxy (the token-injection boundary holds).
	ChanGitHubViaProxy Channel = "github_via_proxy"
	// ChanProxyReachable — POSITIVE CONTROL: nono's own proxy IS reachable from
	// inside. Without it the negative checks are meaningless (a dead listener or
	// a totally-blocked network would pass every "blocked" assertion vacuously).
	ChanProxyReachable Channel = "proxy_reachable"
	// ChanUDPEgress — is UDP egress open? Open by default in nono 0.68.0, an
	// accepted residual (spike §3d). Reported loudly, not failed unless policy
	// opts in.
	ChanUDPEgress Channel = "udp_egress"

	// Approval-channel isolation (issue #146, design §3e — the crux). nono has no
	// namespaces, so the agent runs at rein's uid in the same terminal session;
	// these four assert it still cannot reach or drive the human approval surface
	// to self-approve a push. Each is HARD (fail-closed): a leak refuses the launch.

	// ChanApprovalTTY — /dev/tty is unopenable in the sandbox. Setsid gives the run
	// no controlling terminal, so the agent cannot answer an inline approval prompt.
	ChanApprovalTTY Channel = "approval_tty"
	// ChanApprovalTmuxSocket — connect() to the host tmux control socket is denied
	// by af_unix_mediation:"pathname", so the agent cannot reach the approval popup.
	// The runtime proof that nono ENFORCES the mediation the profile only declares.
	ChanApprovalTmuxSocket Channel = "approval_tmux_socket"
	// ChanApprovalTmuxSendKeys — `tmux send-keys` against that socket fails: a
	// consequence of the connect denial, proving the actual tool an attacker would
	// use cannot drive the popup.
	ChanApprovalTmuxSendKeys Channel = "approval_tmux_send_keys"
	// ChanApprovalTmuxEnv — $TMUX/$TMUX_PANE are absent in the sandbox env, so the
	// agent is not even told the popup socket path (defense-in-depth; the mediation
	// is the real control).
	ChanApprovalTmuxEnv Channel = "approval_tmux_env"

	// Co-located-broker self-hardening (design §8). nono has NO PID namespace and
	// runs the agent at rein's own uid, so rein's broker process is /proc-visible to
	// the agent. These assert the credential-brokering boundary survives co-location.

	// ChanPtrace — HARD (fail-closed): the agent cannot ptrace-ATTACH (or read
	// /proc/<pid>/mem of) a co-located same-uid process. If it could, a prompt-
	// injected agent could dump rein's broker heap for the minted token. Blocked by
	// yama ptrace_scope (ancestor/non-descendant attach) + nono's seccomp; a
	// reachable attach is a leak that refuses the launch.
	ChanPtrace Channel = "ptrace_isolation"
	// ChanProcVisibility — RESIDUAL (reported, not failed): /proc/<pid>/cmdline
	// (argv) of a co-located process is world-readable under nono (no PID ns) — the
	// documented residual, and WHY rein keeps every minted secret out of its own
	// argv (AssertNoSecretInLaunch). /proc/<pid>/environ is additionally blocked by
	// nono (measured EACCES — better than the host default), so env is not exposed.
	ChanProcVisibility Channel = "proc_visibility"
)

// ChannelStatus is the per-channel containment verdict (distinct from the
// doctor-facing sandboxutil.Status re-exported as nono.Status).
type ChannelStatus string

const (
	// ChannelOK — contained as expected (a positive denial was observed).
	ChannelOK ChannelStatus = "ok"
	// ChannelLeak — containment FAILED: the channel was open when it must be shut.
	ChannelLeak ChannelStatus = "leak"
	// ChannelFail — the gate cannot trust its own result (e.g. the positive
	// control failed, or the probe could not run). Fails closed, distinct from a
	// containment leak.
	ChannelFail ChannelStatus = "fail"
	// ChannelWarn — an accepted residual is open (UDP), surfaced loudly.
	ChannelWarn ChannelStatus = "warn"
	// ChannelUnknown — could not determine (target absent, ambiguous errno, no
	// network). Never counted as contained; never fails the gate on its own.
	ChannelUnknown ChannelStatus = "unknown"
)

// Policy tunes classification.
type Policy struct {
	// FailOnUDP promotes an open UDP channel from a warning to a leak. Default
	// false: UDP-open is the documented residual.
	FailOnUDP bool

	// RequireControls fails the gate closed when a control channel that has NO
	// legitimate "couldn't determine" excuse comes back Unknown — the arbitrary-
	// loopback crux (target is a guaranteed-live host listener) and direct-TCP
	// egress (seccomp denies the connect pre-network, so even offline it is a
	// clean EPERM). An Unknown there means an errno drift the gate must not pass
	// silently. The host-side gate (VerifyContainment) sets this; synthetic unit
	// tests leave it off. Does NOT touch github-via-proxy or UDP, which can be
	// legitimately Unknown (no DNS / not attempted).
	RequireControls bool
}

// ConnResult is one connect/send attempt's raw outcome, measured in-sandbox.
type ConnResult struct {
	Target    string `json:"target"`
	Attempted bool   `json:"attempted"`
	Succeeded bool   `json:"succeeded"` // the syscall (and any round-trip) succeeded
	Denied    bool   `json:"denied"`    // explicit EPERM/EACCES — a POSITIVE denial
	Err       string `json:"err,omitempty"`
}

// CredObs is one credential path's read outcome. Existed is host truth (filled
// by the harness from an unconfined stat); Readable/Denied are measured
// in-sandbox. Content is NEVER recorded.
type CredObs struct {
	Path     string `json:"path"`
	Existed  bool   `json:"existed"`  // set by the harness (host-side), not the probe
	Readable bool   `json:"readable"` // the probe read real bytes back (a LEAK)
	Denied   bool   `json:"denied"`   // open failed with EPERM/EACCES
	Sentinel bool   `json:"sentinel"` // a harness-planted canary (Readable ⟺ marker matched)
	Err      string `json:"err,omitempty"`
}

// TTYObs is the /dev/tty open outcome, measured in-sandbox. Opened ⇒ the agent
// has a controlling terminal it could prompt on (a LEAK); any open failure
// (ENXIO from no controlling tty, or fs-deny) ⇒ contained. No credential content.
type TTYObs struct {
	Attempted bool   `json:"attempted"`
	Opened    bool   `json:"opened"` // got a usable fd ⇒ leak
	Err       string `json:"err,omitempty"`
}

// ExecResult is one in-sandbox subprocess (tmux send-keys) outcome. Ran
// distinguishes "the tool started and exited non-zero" (a real refusal) from
// "the tool could not be exec'd at all" (skip, not a pass).
type ExecResult struct {
	Attempted bool   `json:"attempted"` // a target was staged
	Ran       bool   `json:"ran"`       // exec started (vs binary not found / unstageable)
	ExitZero  bool   `json:"exit_zero"` // ran AND exited 0 ⇒ send-keys landed ⇒ leak
	ExitCode  int    `json:"exit_code,omitempty"`
	Err       string `json:"err,omitempty"`
}

// EnvObs records the approval-related env the sandbox was handed. The values are
// socket paths, not secrets; non-empty ⇒ the popup socket path leaked in.
type EnvObs struct {
	Checked  bool   `json:"checked"`
	TMUX     string `json:"tmux,omitempty"`
	TMUXPane string `json:"tmux_pane,omitempty"`
}

// PtraceObs is the co-located-broker ptrace hardening measurement against a planted
// host sentinel (a same-uid process). Attached/MemReadable ⇒ the agent could dump a
// neighbor's memory (rein's broker heap holds the token) — a LEAK. Denied ⇒ blocked.
// No credential content is recorded, only the outcome.
type PtraceObs struct {
	Attempted   bool   `json:"attempted"`
	Attached    bool   `json:"attached"`     // PTRACE_ATTACH succeeded ⇒ LEAK
	Denied      bool   `json:"denied"`       // EPERM/EACCES or seccomp-kill ⇒ blocked (ok)
	MemReadable bool   `json:"mem_readable"` // /proc/<pid>/mem opened ⇒ LEAK
	Target      string `json:"target,omitempty"`
	Err         string `json:"err,omitempty"`
}

// ProcVisObs is the co-located-broker /proc self-hardening measurement against the
// same host sentinel, which holds a fake token marker in its argv + env. cmdline
// (argv) is world-readable under nono (no PID ns) — the documented residual; environ
// is nono-blocked (EACCES). Only booleans are recorded — the marker is a planted
// canary, never a real credential.
type ProcVisObs struct {
	Attempted       bool   `json:"attempted"`
	CmdlineReadable bool   `json:"cmdline_readable"` // argv of the co-located process readable
	CmdlineMarker   bool   `json:"cmdline_marker"`   // the planted marker was seen in cmdline
	EnvironReadable bool   `json:"environ_readable"` // environ readable ⇒ env exposed (nono blocks it)
	EnvironMarker   bool   `json:"environ_marker"`
	Target          string `json:"target,omitempty"`
	Err             string `json:"err,omitempty"`
}

// Observations are the raw facts the in-sandbox probe measures. Classification
// is a separate, pure step (Classify) so it is unit-testable with synthetic
// Observations and never needs nono.
type Observations struct {
	Creds          []CredObs  `json:"creds"`
	DirectExternal ConnResult `json:"direct_external"`
	GitHubDirect   ConnResult `json:"github_direct"`
	PlantedLoop    ConnResult `json:"planted_loopback"`
	NonoProxy      ConnResult `json:"nono_proxy"` // positive control
	UDPExternal    ConnResult `json:"udp_external"`
	// Approval-channel isolation (§3e).
	ApprovalTTY      TTYObs     `json:"approval_tty"`
	ApprovalTmuxSock ConnResult `json:"approval_tmux_socket"`
	ApprovalSendKeys ExecResult `json:"approval_tmux_send_keys"`
	ApprovalEnv      EnvObs     `json:"approval_tmux_env"`
	// ApprovalFixtureErr is HOST-overlaid (like Creds[].Existed): non-empty when
	// tmux IS present on the host but the isolation fixture could not be staged, so
	// the socket/send-keys enforcement proof was skipped despite a real approval
	// surface existing. Distinguishes that fail-open from a clean tmux-absent skip.
	ApprovalFixtureErr string `json:"approval_fixture_err,omitempty"`
	// Co-located-broker self-hardening (§8).
	Ptrace  PtraceObs  `json:"ptrace"`
	ProcVis ProcVisObs `json:"proc_visibility"`
	Errors  []string   `json:"errors,omitempty"`
}

// ChannelVerdict is one channel's classified result.
type ChannelVerdict struct {
	Channel Channel
	Status  ChannelStatus
	Detail  string
}

// Verdict is the classified outcome across all channels.
type Verdict struct {
	Channels []ChannelVerdict
}

// ShouldFailClosed reports whether the launch must be refused: any containment
// leak, or any gate-integrity failure (positive control failed / probe error).
func (v Verdict) ShouldFailClosed() bool {
	for _, c := range v.Channels {
		if c.Status == ChannelLeak || c.Status == ChannelFail {
			return true
		}
	}
	return false
}

// Warnings returns the human-readable warnings (accepted residuals surfaced
// loudly, e.g. UDP open).
func (v Verdict) Warnings() []string {
	var w []string
	for _, c := range v.Channels {
		if c.Status == ChannelWarn {
			w = append(w, fmt.Sprintf("%s: %s", c.Channel, c.Detail))
		}
	}
	return w
}

// Leaks returns the channels that failed closed (leak or gate-integrity fail).
func (v Verdict) Leaks() []ChannelVerdict {
	var out []ChannelVerdict
	for _, c := range v.Channels {
		if c.Status == ChannelLeak || c.Status == ChannelFail {
			out = append(out, c)
		}
	}
	return out
}

// String renders the verdict as a one-line-per-channel report.
func (v Verdict) String() string {
	var b strings.Builder
	for _, c := range v.Channels {
		fmt.Fprintf(&b, "  [%-4s] %-20s %s\n", c.Status, c.Channel, c.Detail)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Classify turns raw Observations into a Verdict. PURE — no I/O, no nono. This
// is the trust-critical logic, and the errno discipline is load-bearing:
//
//	a negative channel is "ok" ONLY on an explicit denial (EPERM/EACCES).
//	Succeeded ⇒ leak. Anything else (refused, timeout, offline, no target) ⇒
//	unknown — NEVER "ok". Mapping absence-of-success to "contained" would let a
//	down network read as containment, the worst failure for a fail-closed gate.
func Classify(obs Observations, policy Policy) Verdict {
	var v Verdict
	v.Channels = append(v.Channels,
		classifyCreds(obs.Creds),
		classifyNegative(ChanDirectTCP, obs.DirectExternal,
			"non-allowlisted external host reachable directly (seccomp egress block failed)",
			"direct external connect denied", policy.RequireControls),
		classifyNegative(ChanArbitraryLoopback, obs.PlantedLoop,
			"arbitrary loopback listener REACHED — rein's no-proxy-auth assumption is BROKEN",
			"arbitrary loopback connect denied (rein's listener is safe without proxy-auth)", policy.RequireControls),
		classifyNegative(ChanGitHubViaProxy, obs.GitHubDirect,
			"github reachable directly, bypassing the proxy (token-injection boundary lost)",
			"direct github connect denied — github egress must route through the proxy", false),
		classifyProxyControl(obs.NonoProxy),
		classifyUDP(obs.UDPExternal, policy),
		classifyApprovalTTY(obs.ApprovalTTY),
		classifyApprovalEnv(obs.ApprovalEnv),
		classifyApprovalSocket(obs.ApprovalTmuxSock, obs.ApprovalFixtureErr, policy.RequireControls),
		classifyApprovalSendKeys(obs.ApprovalSendKeys, obs.ApprovalFixtureErr, policy.RequireControls),
		classifyPtrace(obs.Ptrace, policy.RequireControls),
		classifyProcVisibility(obs.ProcVis),
	)
	return v
}

// classifyPtrace: the agent must NOT be able to ptrace-attach or read the memory of
// a co-located same-uid process (rein's broker). Attached/MemReadable ⇒ leak; a
// denial (EPERM/EACCES, or a seccomp kill of the forked attempt) ⇒ ok. Like the
// other hard controls, a not-attempted / ambiguous outcome fails closed under the
// live gate's RequireControls (the gate stages the sentinel) but is Unknown for
// synthetic unit tests.
func classifyPtrace(o PtraceObs, strict bool) ChannelVerdict {
	unknown := func(detail string) ChannelVerdict {
		if strict {
			return ChannelVerdict{ChanPtrace, ChannelFail, detail + " — no legitimate Unknown for this control; failing closed"}
		}
		return ChannelVerdict{ChanPtrace, ChannelUnknown, detail}
	}
	// The sentinel pid (o.Target) is deterministically volatile — kept in the
	// Observations JSON for debugging, but NOT in the surfaced detail (it would
	// break golden determinism when this verdict reaches the transcript).
	switch {
	case !o.Attempted:
		return unknown("ptrace not attempted (no co-located sentinel staged)")
	case o.Attached || o.MemReadable:
		return ChannelVerdict{ChanPtrace, ChannelLeak,
			"co-located process ptrace-ATTACH / /proc/<pid>/mem read SUCCEEDED — a prompt-injected agent could dump rein's broker heap for the token"}
	case o.Denied:
		return ChannelVerdict{ChanPtrace, ChannelOK,
			"ptrace-attach + /proc/<pid>/mem of a co-located process denied — broker heap not dumpable"}
	default:
		return unknown(fmt.Sprintf("ptrace failed but not with a denial errno (%s); cannot conclude isolation", o.Err))
	}
}

// classifyProcVisibility: /proc/<pid>/cmdline (argv) of a co-located process is
// world-readable under nono (no PID namespace) — a documented RESIDUAL surfaced
// loudly (like UDP), never a launch failure, because rein keeps every minted secret
// out of its own argv (AssertNoSecretInLaunch). environ readable is worse but nono
// blocks it (measured); if it ever became readable it is still only a residual here
// (rein carries no minted secret in env either) — reported, not failed.
func classifyProcVisibility(o ProcVisObs) ChannelVerdict {
	// The sentinel pid (o.Target) stays in the Observations JSON but is kept OUT of
	// the surfaced detail: this Warn reaches the user transcript, and a live pid
	// would make the golden non-deterministic.
	switch {
	case !o.Attempted:
		return ChannelVerdict{ChanProcVisibility, ChannelUnknown, "not attempted (no co-located sentinel staged)"}
	case o.EnvironReadable:
		return ChannelVerdict{ChanProcVisibility, ChannelWarn,
			"co-located /proc/<pid>/environ readable — a neighbor process's env is exposed (nono usually blocks this); rein places no minted secret in env"}
	case o.CmdlineReadable:
		return ChannelVerdict{ChanProcVisibility, ChannelWarn,
			"co-located /proc/<pid>/cmdline (argv) readable — documented residual (no PID namespace); environ is nono-blocked. rein places no minted secret in argv"}
	default:
		return ChannelVerdict{ChanProcVisibility, ChannelOK,
			"co-located /proc cmdline + environ both unreadable"}
	}
}

// approvalSkip resolves a not-attempted approval check: a clean skip (tmux
// absent, stageErr empty) is Unknown; but if tmux WAS present and only the
// fixture failed to stage, the enforcement proof was skipped while a real
// approval surface exists — that must not silently pass, so it is a loud Warn,
// or a fail-closed Fail under the live gate's RequireControls.
func approvalSkip(ch Channel, stageErr string, strict bool, absentDetail string) ChannelVerdict {
	if stageErr == "" {
		return ChannelVerdict{ch, ChannelUnknown, absentDetail}
	}
	if strict {
		return ChannelVerdict{ch, ChannelFail, stageErr + " — failing closed (a tmux approval surface exists but the control could not be verified)"}
	}
	return ChannelVerdict{ch, ChannelWarn, stageErr}
}

// classifyApprovalTTY: /dev/tty is a HARD control with no external dependency —
// it is always measurable, so a probe that did not run it is a gate-integrity
// FAIL, and an OPENED tty is a leak (the agent could answer an inline approval).
func classifyApprovalTTY(o TTYObs) ChannelVerdict {
	switch {
	case !o.Attempted:
		return ChannelVerdict{ChanApprovalTTY, ChannelFail,
			"/dev/tty was not probed — cannot confirm the approval tty is unreachable; failing closed"}
	case o.Opened:
		return ChannelVerdict{ChanApprovalTTY, ChannelLeak,
			"/dev/tty OPENED inside the sandbox — the agent has a controlling terminal and could drive an inline approval prompt"}
	default:
		return ChannelVerdict{ChanApprovalTTY, ChannelOK,
			"/dev/tty unopenable inside the sandbox (" + o.Err + ")"}
	}
}

// classifyApprovalEnv: $TMUX/$TMUX_PANE must be absent so the agent is never even
// told the popup socket path. Always measurable ⇒ not-checked is a gate FAIL.
func classifyApprovalEnv(o EnvObs) ChannelVerdict {
	switch {
	case !o.Checked:
		return ChannelVerdict{ChanApprovalTmuxEnv, ChannelFail,
			"approval env was not probed — failing closed"}
	case o.TMUX != "" || o.TMUXPane != "":
		return ChannelVerdict{ChanApprovalTmuxEnv, ChannelLeak,
			fmt.Sprintf("$TMUX/$TMUX_PANE present in the sandbox — the approval socket path leaked to the agent (TMUX=%q TMUX_PANE=%q)", o.TMUX, o.TMUXPane)}
	default:
		return ChannelVerdict{ChanApprovalTmuxEnv, ChannelOK,
			"$TMUX and $TMUX_PANE absent in the sandbox env"}
	}
}

// classifyApprovalSocket: connect() to the host tmux socket must be DENIED. Same
// errno discipline as the other negatives (Succeeded ⇒ leak, EPERM/EACCES ⇒ ok),
// EXCEPT a not-attempted result (no tmux fixture could be staged — tmux absent)
// is always a plain skip Unknown, never a strict Fail: a machine without tmux
// must not fail the launch gate. Only an ambiguous errno against a LIVE fixture
// fails closed under strict.
func classifyApprovalSocket(r ConnResult, stageErr string, strict bool) ChannelVerdict {
	switch {
	case !r.Attempted:
		return approvalSkip(ChanApprovalTmuxSocket, stageErr, strict,
			"no tmux fixture staged (tmux absent) — approval-socket connect not tested")
	case r.Succeeded:
		return ChannelVerdict{ChanApprovalTmuxSocket, ChannelLeak,
			"tmux approval socket REACHED — af_unix_mediation not enforced; the agent can drive the approval popup [" + r.Target + "]"}
	case r.Denied:
		return ChannelVerdict{ChanApprovalTmuxSocket, ChannelOK,
			"tmux approval socket connect denied [" + r.Target + "]"}
	default:
		detail := fmt.Sprintf("connect to a LIVE tmux fixture %s failed but not with a denial errno (%s); cannot conclude the mediation held", r.Target, r.Err)
		if strict {
			return ChannelVerdict{ChanApprovalTmuxSocket, ChannelFail, detail + " — no legitimate Unknown for this control; failing closed"}
		}
		return ChannelVerdict{ChanApprovalTmuxSocket, ChannelUnknown, detail}
	}
}

// classifyApprovalSendKeys: `tmux send-keys` against the fixture socket must fail.
// ExitZero ⇒ the keys landed ⇒ leak. A non-zero exit ⇒ refused ⇒ ok. A tool that
// could not be staged or exec'd (no fixture / tmux not readable in-sandbox) is a
// skip Unknown, not a pass — but never a Fail, so a tmux-less host still passes.
func classifyApprovalSendKeys(e ExecResult, stageErr string, strict bool) ChannelVerdict {
	switch {
	case !e.Attempted:
		return approvalSkip(ChanApprovalTmuxSendKeys, stageErr, strict,
			"no tmux fixture staged (tmux absent) — send-keys not tested")
	case !e.Ran:
		return ChannelVerdict{ChanApprovalTmuxSendKeys, ChannelUnknown,
			"tmux could not be exec'd inside the sandbox — send-keys not tested (" + e.Err + ")"}
	case e.ExitZero:
		return ChannelVerdict{ChanApprovalTmuxSendKeys, ChannelLeak,
			"tmux send-keys SUCCEEDED inside the sandbox — the agent drove the approval popup and can self-approve"}
	default:
		return ChannelVerdict{ChanApprovalTmuxSendKeys, ChannelOK,
			fmt.Sprintf("tmux send-keys refused inside the sandbox (exit %d)", e.ExitCode)}
	}
}

func classifyCreds(creds []CredObs) ChannelVerdict {
	var existed int
	var leaked []string
	for _, c := range creds {
		if c.Readable {
			leaked = append(leaked, c.Path)
		}
		if c.Existed || c.Sentinel {
			existed++
		}
	}
	switch {
	case len(leaked) > 0:
		return ChannelVerdict{ChanCredentials, ChannelLeak,
			fmt.Sprintf("credential(s) READABLE inside the sandbox: %s (deny_credentials failed)", strings.Join(leaked, ", "))}
	case existed == 0:
		return ChannelVerdict{ChanCredentials, ChannelUnknown,
			"no credential files present to test (cannot prove deny_credentials)"}
	default:
		return ChannelVerdict{ChanCredentials, ChannelOK,
			fmt.Sprintf("%d credential file(s) present, all unreadable inside the sandbox", existed)}
	}
}

// classifyNegative classifies a channel that must be BLOCKED. If strictUnknown
// is set, an ambiguous non-denial (the channel has no legitimate Unknown) fails
// the gate closed instead of passing as Unknown.
func classifyNegative(ch Channel, r ConnResult, leakDetail, okDetail string, strictUnknown bool) ChannelVerdict {
	unknown := func(detail string) ChannelVerdict {
		if strictUnknown {
			return ChannelVerdict{ch, ChannelFail, detail + " — no legitimate Unknown for this control; failing closed"}
		}
		return ChannelVerdict{ch, ChannelUnknown, detail}
	}
	switch {
	case !r.Attempted:
		return unknown("not attempted (no target)")
	case r.Succeeded:
		return ChannelVerdict{ch, ChannelLeak, leakDetail + " [" + r.Target + "]"}
	case r.Denied:
		return ChannelVerdict{ch, ChannelOK, okDetail + " [" + r.Target + "]"}
	default:
		return unknown(fmt.Sprintf("connect to %s failed but not with a denial errno (%s); cannot conclude containment", r.Target, r.Err))
	}
}

// classifyProxyControl is the positive control: nono's proxy MUST be reachable.
func classifyProxyControl(r ConnResult) ChannelVerdict {
	switch {
	case !r.Attempted:
		return ChannelVerdict{ChanProxyReachable, ChannelFail,
			"positive control not run (HTTPS_PROXY absent?) — cannot trust the negative checks"}
	case r.Succeeded:
		return ChannelVerdict{ChanProxyReachable, ChannelOK,
			"nono's proxy is reachable from inside [" + r.Target + "]"}
	default:
		return ChannelVerdict{ChanProxyReachable, ChannelFail,
			fmt.Sprintf("nono's proxy UNREACHABLE [%s] (%s) — misconfig; the whole egress path is dead, failing closed", r.Target, r.Err)}
	}
}

func classifyUDP(r ConnResult, policy Policy) ChannelVerdict {
	switch {
	case !r.Attempted:
		return ChannelVerdict{ChanUDPEgress, ChannelUnknown, "not attempted"}
	case r.Succeeded:
		if policy.FailOnUDP {
			return ChannelVerdict{ChanUDPEgress, ChannelLeak,
				"UDP egress OPEN and policy requires it closed [" + r.Target + "]"}
		}
		return ChannelVerdict{ChanUDPEgress, ChannelWarn,
			"UDP egress OPEN (accepted residual — a prompt-injected agent has a UDP exfil channel) [" + r.Target + "]"}
	case r.Denied:
		return ChannelVerdict{ChanUDPEgress, ChannelOK, "UDP egress denied [" + r.Target + "]"}
	default:
		return ChannelVerdict{ChanUDPEgress, ChannelUnknown,
			fmt.Sprintf("UDP send to %s failed but not with a denial errno (%s)", r.Target, r.Err)}
	}
}

// isDenied reports whether err is an explicit permission denial (EPERM/EACCES),
// which is how nono's seccomp-notify supervisor refuses a connect/bind/sendto.
// Distinguishing this from ECONNREFUSED/timeout/offline is the crux of the
// errno discipline in Classify.
func isDenied(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

// ---- In-sandbox probe ----

// probeConfig is the target set the harness stages for the in-sandbox probe. It
// is JSON in a read-granted file; the probe reads it and measures each target.
type probeConfig struct {
	Creds           []CredTarget `json:"creds"`
	ExternalTarget  string       `json:"external_target"`  // e.g. "1.1.1.1:443" (no DNS)
	GitHubTarget    string       `json:"github_target"`    // resolved github IP:443 (may be empty)
	PlantedLoopback string       `json:"planted_loopback"` // 127.0.0.1:PORT host listener
	UDPTarget       string       `json:"udp_target"`       // e.g. "8.8.8.8:53"
	// Approval-channel isolation fixture (§3e). Empty ⇒ that check is skipped.
	TmuxSocket string `json:"tmux_socket,omitempty"` // dedicated fixture socket path (NON-granted dir)
	TmuxBin    string `json:"tmux_bin,omitempty"`    // tmux path for the send-keys probe
	TmuxTarget string `json:"tmux_target,omitempty"` // fixture session name for send-keys -t
	// Co-located-broker self-hardening (§8): a planted host sentinel (same-uid
	// process) holding the marker in its argv + env. Empty pid ⇒ those checks skip.
	ProcSentinelPID    int    `json:"proc_sentinel_pid,omitempty"`
	ProcSentinelMarker string `json:"proc_sentinel_marker,omitempty"`
	DialTimeoutMS      int    `json:"dial_timeout_ms"`
}

// CredTarget is one credential path for the probe to attempt to read.
type CredTarget struct {
	Path     string `json:"path"`
	Sentinel bool   `json:"sentinel"`
	Marker   string `json:"marker,omitempty"` // for a sentinel: the exact bytes that prove a read-back
}

// probeExit codes: the probe MEASURES and writes Observations JSON; its exit
// code only signals whether it ran, not the verdict (that is in the JSON).
const (
	probeOK    = 0 // observations written
	probeError = 12
)

// RunContainmentProbe is the body of the hidden `rein __nono-probe` subcommand.
// It runs INSIDE the nono sandbox. args: [outPath, configPath]. It reads the
// config, measures every channel, and writes an Observations JSON to outPath
// (in a write-granted dir). Returns probeOK on write, probeError otherwise. It
// records only booleans/errnos — never credential contents.
func RunContainmentProbe(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "nono probe: usage: __nono-probe <outPath> <configPath>")
		return probeError
	}
	outPath, cfgPath := args[0], args[1]

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nono probe: read config: %v\n", err)
		return probeError
	}
	var cfg probeConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "nono probe: parse config: %v\n", err)
		return probeError
	}
	timeout := time.Duration(cfg.DialTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	obs := Observations{}

	// 1. Credentials: attempt a read; record readable/denied only.
	for _, c := range cfg.Creds {
		obs.Creds = append(obs.Creds, probeReadCred(c))
	}
	// 2. Direct external egress (literal IP, no DNS).
	obs.DirectExternal = probeTCP(cfg.ExternalTarget, timeout, false)
	// 3. Direct github egress (must be blocked; proxy is the only path).
	obs.GitHubDirect = probeTCP(cfg.GitHubTarget, timeout, false)
	// 4. Arbitrary loopback — THE control. Round-trip so a stray refuse can't
	//    masquerade as a reach.
	obs.PlantedLoop = probeTCP(cfg.PlantedLoopback, timeout, true)
	// 5. Positive control: nono's own proxy, discovered from HTTPS_PROXY.
	obs.NonoProxy = probeTCP(nonoProxyAddr(), timeout, false)
	// 6. UDP residual.
	obs.UDPExternal = probeUDP(cfg.UDPTarget, timeout)
	// 7. Approval-channel isolation (§3e): the agent must not reach or drive the
	//    human approval surface. /dev/tty and env are always measured; the tmux
	//    socket/send-keys checks run only when a fixture was staged.
	obs.ApprovalTTY = probeDevTTY()
	obs.ApprovalEnv = EnvObs{Checked: true, TMUX: os.Getenv("TMUX"), TMUXPane: os.Getenv("TMUX_PANE")}
	obs.ApprovalTmuxSock = probeUnixConnect(cfg.TmuxSocket, timeout)
	obs.ApprovalSendKeys = probeSendKeys(cfg.TmuxBin, cfg.TmuxSocket, cfg.TmuxTarget, timeout)
	// 8. Co-located-broker self-hardening (§8): against the planted host sentinel,
	//    the agent must not be able to ptrace/dump a neighbor's memory (HARD); its
	//    argv is a documented residual (readable), its environ nono-blocked.
	obs.Ptrace = probePtrace(cfg.ProcSentinelPID, timeout)
	obs.ProcVis = probeProcVisibility(cfg.ProcSentinelPID, cfg.ProcSentinelMarker)

	data, err := json.MarshalIndent(obs, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "nono probe: marshal: %v\n", err)
		return probeError
	}
	if err := os.WriteFile(outPath, append(data, '\n'), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "nono probe: write observations: %v\n", err)
		return probeError
	}
	return probeOK
}

// probeReadCred attempts to read a credential path. It reads at most a small
// prefix and, for a sentinel, checks ONLY whether the known marker came back —
// it never stores or emits real credential content.
func probeReadCred(c CredTarget) CredObs {
	o := CredObs{Path: c.Path, Sentinel: c.Sentinel}
	f, err := os.Open(c.Path)
	if err != nil {
		o.Denied = isDenied(err)
		o.Err = err.Error()
		return o
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, rerr := f.Read(buf)
	if rerr != nil && n == 0 {
		o.Denied = isDenied(rerr)
		o.Err = rerr.Error()
		return o
	}
	if c.Sentinel {
		// A sentinel is a canary we planted; readable ⟺ the marker matched.
		o.Readable = strings.HasPrefix(string(buf[:n]), c.Marker)
	} else {
		// A real credential: readable ⟺ we got any bytes. We do NOT keep them.
		o.Readable = n > 0
	}
	return o
}

// probeTCP attempts a raw TCP connect (bypassing any proxy). If roundTrip, it
// writes a byte and reads one, so "succeeded" means the peer really answered
// (guards against a half-open that a naive connect might report).
func probeTCP(target string, timeout time.Duration, roundTrip bool) ConnResult {
	r := ConnResult{Target: target}
	if strings.TrimSpace(target) == "" {
		return r
	}
	r.Attempted = true
	d := net.Dialer{Timeout: timeout}
	conn, err := d.Dial("tcp", target)
	if err != nil {
		r.Denied = isDenied(err)
		r.Err = err.Error()
		return r
	}
	defer conn.Close()
	if roundTrip {
		_ = conn.SetDeadline(time.Now().Add(timeout))
		if _, werr := conn.Write([]byte("rein-probe\n")); werr != nil {
			// TCP connected but write failed — still a reach at connect level.
			r.Succeeded = true
			r.Err = "connected; write failed: " + werr.Error()
			return r
		}
		one := make([]byte, 1)
		_, _ = conn.Read(one) // read outcome is not required; connect proved reach
	}
	r.Succeeded = true
	return r
}

// probeUDP attempts a UDP send. Succeeded ⇒ UDP egress is open (the sendto
// syscall was permitted); Denied ⇒ nono blocked it.
func probeUDP(target string, timeout time.Duration) ConnResult {
	r := ConnResult{Target: target}
	if strings.TrimSpace(target) == "" {
		return r
	}
	r.Attempted = true
	conn, err := net.DialTimeout("udp", target, timeout)
	if err != nil {
		r.Denied = isDenied(err)
		r.Err = err.Error()
		return r
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, werr := conn.Write([]byte("rein-probe")); werr != nil {
		r.Denied = isDenied(werr)
		r.Err = werr.Error()
		return r
	}
	r.Succeeded = true
	return r
}

// probeDevTTY attempts to open /dev/tty read-write. Under Setsid the run has no
// controlling terminal, so this fails with ENXIO (and fs-deny would also refuse).
// A success means the agent has a tty it could prompt on — a leak. No content.
func probeDevTTY() TTYObs {
	o := TTYObs{Attempted: true}
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		o.Err = err.Error()
		return o
	}
	o.Opened = true
	_ = f.Close()
	return o
}

// probeUnixConnect attempts an AF_UNIX connect to a pathname socket (the host
// tmux control socket). Succeeded ⇒ the socket was reachable (a leak); Denied ⇒
// af_unix_mediation refused it (EPERM/EACCES). Empty target ⇒ not attempted.
func probeUnixConnect(sock string, timeout time.Duration) ConnResult {
	r := ConnResult{Target: sock}
	if strings.TrimSpace(sock) == "" {
		return r
	}
	r.Attempted = true
	conn, err := net.DialTimeout("unix", sock, timeout)
	if err != nil {
		r.Denied = isDenied(err)
		r.Err = err.Error()
		return r
	}
	_ = conn.Close()
	r.Succeeded = true
	return r
}

// probeSendKeys runs `tmux -S <sock> send-keys` against the fixture. A zero exit
// means the keystrokes landed (the agent drove the popup — a leak); a non-zero
// exit means it was refused. Ran=false distinguishes "tmux not exec'able here"
// (a skip) from a real refusal. It never targets anything but the passed fixture
// socket, so it cannot touch the operator's tmux server.
func probeSendKeys(tmuxBin, sock, target string, timeout time.Duration) ExecResult {
	e := ExecResult{}
	if strings.TrimSpace(tmuxBin) == "" || strings.TrimSpace(sock) == "" {
		return e
	}
	e.Attempted = true
	if strings.TrimSpace(target) == "" {
		target = "probe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tmuxBin, "-S", sock, "send-keys", "-t", target, "echo rein-probe", "Enter")
	err := cmd.Run()
	var execErr *exec.Error
	if errors.As(err, &execErr) || cmd.ProcessState == nil {
		// tmux could not be started (not readable/exec'able in the sandbox).
		e.Err = "exec tmux: " + errString(err)
		return e
	}
	e.Ran = true
	e.ExitCode = cmd.ProcessState.ExitCode()
	e.ExitZero = err == nil
	if err != nil {
		e.Err = err.Error()
	}
	return e
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---- Co-located-broker self-hardening probes (§8) ----

// ptrace child exit codes: the __nono-ptrace-attach subcommand encodes ONLY the
// attach outcome in its exit code (so a seccomp RET_KILL of the attempt cannot take
// the parent probe down before it writes observations).
const (
	ptraceChildAttached = 0  // PTRACE_ATTACH succeeded ⇒ LEAK
	ptraceChildDenied   = 20 // EPERM/EACCES ⇒ blocked (ok)
	ptraceChildOther    = 21 // some other error ⇒ unknown
	ptraceChildUsage    = 22
)

// RunPtraceAttachProbe is the body of the hidden `rein __nono-ptrace-attach <pid>`
// subcommand. It runs the ptrace attempt in ISOLATION (its own process, so a
// seccomp kill can't cascade) and reports the outcome via exit code only. It never
// reads or emits any memory content — a successful attach is immediately detached.
func RunPtraceAttachProbe(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "nono ptrace: usage: __nono-ptrace-attach <pid>")
		return ptraceChildUsage
	}
	pid, err := strconv.Atoi(strings.TrimSpace(args[0]))
	if err != nil || pid <= 0 {
		return ptraceChildUsage
	}
	runtime.LockOSThread() // PTRACE_ATTACH is per-thread
	if err := syscall.PtraceAttach(pid); err != nil {
		if isDenied(err) {
			return ptraceChildDenied
		}
		return ptraceChildOther
	}
	_ = syscall.PtraceDetach(pid)
	return ptraceChildAttached
}

// probePtrace forks the fork-safe __nono-ptrace-attach child against the sentinel
// pid and interprets its exit: attached ⇒ leak, denied (errno OR the child was
// killed by a seccomp RET_KILL) ⇒ blocked, other ⇒ unknown. It also directly opens
// /proc/<pid>/mem (needs PTRACE_MODE_ATTACH — a clean read/open outcome, no kill).
func probePtrace(pid int, timeout time.Duration) PtraceObs {
	o := PtraceObs{}
	if pid <= 0 {
		return o
	}
	o.Attempted = true
	o.Target = "pid " + strconv.Itoa(pid)

	self, err := os.Executable()
	if err != nil {
		o.Err = "resolve self: " + err.Error()
		return o
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "__nono-ptrace-attach", strconv.Itoa(pid))
	runErr := cmd.Run()
	switch {
	case cmd.ProcessState == nil:
		o.Err = "ptrace child did not run: " + errString(runErr)
	case cmd.ProcessState.Exited():
		switch cmd.ProcessState.ExitCode() {
		case ptraceChildAttached:
			o.Attached = true
		case ptraceChildDenied:
			o.Denied = true
		default:
			o.Err = fmt.Sprintf("ptrace child exit %d", cmd.ProcessState.ExitCode())
		}
	default:
		// Not a normal exit ⇒ the child was signaled. A seccomp RET_KILL of the
		// ptrace syscall is a BLOCK, so treat a kill as denied.
		o.Denied = true
		o.Err = "ptrace child killed by signal (seccomp RET_KILL treated as blocked): " + errString(runErr)
	}

	// /proc/<pid>/mem: opening for read requires PTRACE_MODE_ATTACH. A successful
	// open is a leak; EACCES/EPERM is the expected block.
	if f, e := os.Open(fmt.Sprintf("/proc/%d/mem", pid)); e == nil {
		o.MemReadable = true
		_ = f.Close()
	} else if o.Err == "" && !isDenied(e) {
		o.Err = "procmem: " + e.Error()
	}
	return o
}

// probeProcVisibility reads /proc/<pid>/cmdline and /proc/<pid>/environ of the
// co-located sentinel and records only whether each was readable and whether the
// planted (non-secret) marker appeared. Under nono cmdline is readable (residual)
// and environ is blocked (EACCES). It never records real credential content.
func probeProcVisibility(pid int, marker string) ProcVisObs {
	o := ProcVisObs{}
	if pid <= 0 {
		return o
	}
	o.Attempted = true
	o.Target = "pid " + strconv.Itoa(pid)

	if b, e := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); e == nil {
		o.CmdlineReadable = true
		o.CmdlineMarker = marker != "" && bytes.Contains(b, []byte(marker))
	} else {
		o.Err = "cmdline: " + e.Error()
	}
	if b, e := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); e == nil {
		o.EnvironReadable = true
		o.EnvironMarker = marker != "" && bytes.Contains(b, []byte(marker))
	} else if o.Err == "" {
		o.Err = "environ: " + e.Error()
	}
	return o
}

// AssertNoSecretInLaunch is the co-located-broker DISCIPLINE check (§8): because the
// agent can read /proc/<rein-pid>/cmdline (argv) of a co-located process, rein's
// broker/proxy launch must place NO real credential in its own (or a child's) argv
// or env. It fails closed if any secret value appears in argv or an env value. Pure
// and content-free in its error (it names the vector, never the secret).
func AssertNoSecretInLaunch(argv, env, secrets []string) error {
	for _, s := range secrets {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		for _, a := range argv {
			if strings.Contains(a, s) {
				return fmt.Errorf("nono: broker launch argv exposes a credential (agent-readable via /proc/<pid>/cmdline under no-PID-ns nono)")
			}
		}
		for _, e := range env {
			if i := strings.IndexByte(e, '='); i >= 0 && strings.Contains(e[i+1:], s) {
				return fmt.Errorf("nono: broker launch env exposes a credential (co-located process env must carry no minted secret)")
			}
		}
	}
	return nil
}

// nonoProxyAddr extracts nono's proxy host:port from the HTTP(S)_PROXY env nono
// injects into the sandbox (e.g. "http://nono:<token>@127.0.0.1:33777").
func nonoProxyAddr() string {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		v := os.Getenv(k)
		if v == "" {
			continue
		}
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return ""
}
