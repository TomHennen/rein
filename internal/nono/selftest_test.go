package nono

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// denied and refused build ConnResults the way the probe would, so the
// classifier tests exercise the exact errno discipline.
func denied(target string) ConnResult {
	return ConnResult{Target: target, Attempted: true, Denied: true, Err: "permission denied"}
}
func refused(target string) ConnResult { // ECONNREFUSED / timeout / offline
	return ConnResult{Target: target, Attempted: true, Err: "connection refused"}
}
func reached(target string) ConnResult {
	return ConnResult{Target: target, Attempted: true, Succeeded: true}
}

// contained is a fully-clean observation set: creds hidden, all egress denied,
// proxy reachable, UDP denied, and the approval channels all isolated.
func contained() Observations {
	return Observations{
		Creds:            []CredObs{{Path: "/home/u/.ssh/id_ed25519", Existed: true, Denied: true}},
		DirectExternal:   denied("1.1.1.1:443"),
		GitHubDirect:     denied("140.82.121.3:443"),
		PlantedLoop:      denied("127.0.0.1:47999"),
		NonoProxy:        reached("127.0.0.1:33777"),
		UDPExternal:      denied("8.8.8.8:53"),
		ApprovalTTY:      TTYObs{Attempted: true, Opened: false, Err: "open /dev/tty: no such device or address"},
		ApprovalTmuxSock: denied("/tmp/rein-approvalfix-x/tmux.sock"),
		ApprovalSendKeys: ExecResult{Attempted: true, Ran: true, ExitZero: false, ExitCode: 1},
		ApprovalEnv:      EnvObs{Checked: true},
	}
}

func chanStatus(v Verdict, ch Channel) ChannelStatus {
	for _, c := range v.Channels {
		if c.Channel == ch {
			return c.Status
		}
	}
	return "MISSING"
}

func TestClassify_Contained(t *testing.T) {
	v := Classify(contained(), Policy{})
	if v.ShouldFailClosed() {
		t.Fatalf("clean containment must not fail closed; got:\n%s", v.String())
	}
	for _, ch := range []Channel{ChanCredentials, ChanDirectTCP, ChanArbitraryLoopback, ChanGitHubViaProxy, ChanProxyReachable, ChanUDPEgress, ChanApprovalTTY, ChanApprovalTmuxSocket, ChanApprovalTmuxSendKeys, ChanApprovalTmuxEnv} {
		if got := chanStatus(v, ch); got != ChannelOK {
			t.Errorf("channel %s = %s, want ok", ch, got)
		}
	}
}

// The crux: an arbitrary loopback listener REACHED must fail closed. The whole
// no-proxy-auth decision rests on this being a leak.
func TestClassify_ArbitraryLoopbackReached_IsLeak(t *testing.T) {
	obs := contained()
	obs.PlantedLoop = reached("127.0.0.1:47999")
	v := Classify(obs, Policy{})
	if got := chanStatus(v, ChanArbitraryLoopback); got != ChannelLeak {
		t.Fatalf("arbitrary loopback reached must be leak, got %s", got)
	}
	if !v.ShouldFailClosed() {
		t.Fatalf("a loopback leak must fail the gate closed")
	}
}

// Errno discipline: a connect that merely FAILED (refused/timeout/offline) is
// NOT containment — it is unknown, and must not read as ok, but also must not
// on its own fail the gate.
func TestClassify_RefusedIsUnknownNotOK(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(o *Observations)
		ch   Channel
	}{
		{"direct", func(o *Observations) { o.DirectExternal = refused("1.1.1.1:443") }, ChanDirectTCP},
		{"github", func(o *Observations) { o.GitHubDirect = refused("140.82.121.3:443") }, ChanGitHubViaProxy},
		{"loopback", func(o *Observations) { o.PlantedLoop = refused("127.0.0.1:47999") }, ChanArbitraryLoopback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := contained()
			tc.set(&obs)
			v := Classify(obs, Policy{})
			if got := chanStatus(v, tc.ch); got != ChannelUnknown {
				t.Errorf("%s refused: got %s, want unknown", tc.ch, got)
			}
			if v.ShouldFailClosed() {
				t.Errorf("a lone unknown must not fail the gate closed:\n%s", v.String())
			}
		})
	}
}

// Under RequireControls (what the gate sets), an ambiguous non-denial on the
// crux (arbitrary loopback) or direct-TCP fails closed instead of passing as
// Unknown — those controls have no legitimate "couldn't determine".
func TestClassify_RequireControls_UnknownFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(o *Observations)
		ch   Channel
	}{
		{"loopback", func(o *Observations) { o.PlantedLoop = refused("127.0.0.1:47999") }, ChanArbitraryLoopback},
		{"direct", func(o *Observations) { o.DirectExternal = refused("1.1.1.1:443") }, ChanDirectTCP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := contained()
			tc.set(&obs)
			v := Classify(obs, Policy{RequireControls: true})
			if got := chanStatus(v, tc.ch); got != ChannelFail {
				t.Errorf("%s under RequireControls: got %s, want fail", tc.ch, got)
			}
			if !v.ShouldFailClosed() {
				t.Errorf("strict-control Unknown must fail the gate closed")
			}
		})
	}
}

// github-via-proxy Unknown is legitimate (no DNS) even under RequireControls.
func TestClassify_RequireControls_GitHubUnknownStillPasses(t *testing.T) {
	obs := contained()
	obs.GitHubDirect = ConnResult{} // unresolved / not attempted
	v := Classify(obs, Policy{RequireControls: true})
	if got := chanStatus(v, ChanGitHubViaProxy); got != ChannelUnknown {
		t.Errorf("github unknown under RequireControls: got %s, want unknown", got)
	}
	if v.ShouldFailClosed() {
		t.Errorf("a legitimately-unknown github channel must not fail closed")
	}
}

func TestClassify_DirectEgressReached_IsLeak(t *testing.T) {
	obs := contained()
	obs.DirectExternal = reached("1.1.1.1:443")
	v := Classify(obs, Policy{})
	if chanStatus(v, ChanDirectTCP) != ChannelLeak || !v.ShouldFailClosed() {
		t.Fatalf("direct external reach must leak + fail closed:\n%s", v.String())
	}
}

func TestClassify_GitHubDirectReached_IsLeak(t *testing.T) {
	obs := contained()
	obs.GitHubDirect = reached("140.82.121.3:443")
	v := Classify(obs, Policy{})
	if chanStatus(v, ChanGitHubViaProxy) != ChannelLeak || !v.ShouldFailClosed() {
		t.Fatalf("direct github reach must leak + fail closed:\n%s", v.String())
	}
}

func TestClassify_CredReadable_IsLeak(t *testing.T) {
	obs := contained()
	obs.Creds = []CredObs{{Path: "/home/u/.config/rein-credentials/app.pem", Existed: true, Readable: true}}
	v := Classify(obs, Policy{})
	if chanStatus(v, ChanCredentials) != ChannelLeak || !v.ShouldFailClosed() {
		t.Fatalf("readable credential must leak + fail closed:\n%s", v.String())
	}
}

func TestClassify_SentinelReadable_IsLeak(t *testing.T) {
	obs := contained()
	obs.Creds = []CredObs{{Path: "/home/u/.ssh/rein-sentinel", Sentinel: true, Existed: true, Readable: true}}
	v := Classify(obs, Policy{})
	if chanStatus(v, ChanCredentials) != ChannelLeak {
		t.Fatalf("readable sentinel must leak, got %s", chanStatus(v, ChanCredentials))
	}
}

func TestClassify_NoCredsPresent_IsUnknown(t *testing.T) {
	obs := contained()
	obs.Creds = []CredObs{{Path: "/nope", Existed: false, Denied: true}}
	v := Classify(obs, Policy{})
	if got := chanStatus(v, ChanCredentials); got != ChannelUnknown {
		t.Fatalf("no creds present must be unknown, got %s", got)
	}
	if v.ShouldFailClosed() {
		t.Fatalf("unknown creds must not fail closed on its own")
	}
}

// Positive control: nono's proxy unreachable is a gate-integrity FAIL (fails
// closed), distinct from a containment leak.
func TestClassify_ProxyUnreachable_FailsClosed(t *testing.T) {
	obs := contained()
	obs.NonoProxy = refused("127.0.0.1:33777")
	v := Classify(obs, Policy{})
	if got := chanStatus(v, ChanProxyReachable); got != ChannelFail {
		t.Fatalf("proxy unreachable must be fail, got %s", got)
	}
	if !v.ShouldFailClosed() {
		t.Fatalf("failed positive control must fail the gate closed")
	}
}

func TestClassify_ProxyNotAttempted_FailsClosed(t *testing.T) {
	obs := contained()
	obs.NonoProxy = ConnResult{}
	v := Classify(obs, Policy{})
	if !v.ShouldFailClosed() {
		t.Fatalf("missing positive control must fail closed")
	}
}

// UDP open is a warning by default, a leak only under FailOnUDP.
func TestClassify_UDPOpen_WarnsByDefault(t *testing.T) {
	obs := contained()
	obs.UDPExternal = reached("8.8.8.8:53")
	v := Classify(obs, Policy{})
	if got := chanStatus(v, ChanUDPEgress); got != ChannelWarn {
		t.Fatalf("UDP open must warn by default, got %s", got)
	}
	if v.ShouldFailClosed() {
		t.Fatalf("UDP-open must NOT fail closed by default")
	}
	if len(v.Warnings()) == 0 {
		t.Fatalf("UDP-open must surface a warning")
	}
}

func TestClassify_UDPOpen_FailsUnderPolicy(t *testing.T) {
	obs := contained()
	obs.UDPExternal = reached("8.8.8.8:53")
	v := Classify(obs, Policy{FailOnUDP: true})
	if got := chanStatus(v, ChanUDPEgress); got != ChannelLeak {
		t.Fatalf("UDP open under FailOnUDP must leak, got %s", got)
	}
	if !v.ShouldFailClosed() {
		t.Fatalf("UDP leak under policy must fail closed")
	}
}

// ---- Approval-channel isolation (§3e, issue #146) ----

// /dev/tty OPENED inside the sandbox is a leak (the agent could answer an inline
// approval); unopenable is contained; not-probed fails closed (mandatory control).
func TestClassify_ApprovalTTY(t *testing.T) {
	for _, tc := range []struct {
		name string
		obs  TTYObs
		want ChannelStatus
	}{
		{"unopenable", TTYObs{Attempted: true, Opened: false, Err: "no such device or address"}, ChannelOK},
		{"opened", TTYObs{Attempted: true, Opened: true}, ChannelLeak},
		{"not-probed", TTYObs{Attempted: false}, ChannelFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := contained()
			obs.ApprovalTTY = tc.obs
			v := Classify(obs, Policy{})
			if got := chanStatus(v, ChanApprovalTTY); got != tc.want {
				t.Fatalf("tty %s: got %s, want %s", tc.name, got, tc.want)
			}
			if (tc.want == ChannelLeak || tc.want == ChannelFail) != v.ShouldFailClosed() {
				t.Fatalf("tty %s: ShouldFailClosed mismatch", tc.name)
			}
		})
	}
}

// $TMUX/$TMUX_PANE present in the sandbox leaks the popup socket path; absent is
// contained; not-checked fails closed.
func TestClassify_ApprovalEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		obs  EnvObs
		want ChannelStatus
	}{
		{"absent", EnvObs{Checked: true}, ChannelOK},
		{"tmux-present", EnvObs{Checked: true, TMUX: "/tmp/tmux-1000/default,123,0"}, ChannelLeak},
		{"pane-present", EnvObs{Checked: true, TMUXPane: "%3"}, ChannelLeak},
		{"not-checked", EnvObs{Checked: false}, ChannelFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := contained()
			obs.ApprovalEnv = tc.obs
			v := Classify(obs, Policy{})
			if got := chanStatus(v, ChanApprovalTmuxEnv); got != tc.want {
				t.Fatalf("env %s: got %s, want %s", tc.name, got, tc.want)
			}
		})
	}
}

// tmux socket connect: denied ⇒ ok, reached ⇒ leak. A refused connect against a
// LIVE fixture is Unknown by default but fails closed under RequireControls. NO
// fixture with tmux ABSENT is always a plain skip Unknown, even under strict. But
// tmux PRESENT + unstageable fixture must not silently pass: Warn (lax) / Fail
// (strict).
func TestClassify_ApprovalSocket(t *testing.T) {
	sock := "/tmp/rein-approvalfix-x/tmux.sock"
	const stageErr = "tmux present but fixture unstageable"
	for _, tc := range []struct {
		name   string
		r      ConnResult
		fixErr string
		strict bool
		want   ChannelStatus
	}{
		{"denied", denied(sock), "", false, ChannelOK},
		{"reached", reached(sock), "", false, ChannelLeak},
		{"reached-strict", reached(sock), "", true, ChannelLeak},
		{"ambiguous-lax", refused(sock), "", false, ChannelUnknown},
		{"ambiguous-strict", refused(sock), "", true, ChannelFail},
		{"tmux-absent-lax", ConnResult{}, "", false, ChannelUnknown},
		{"tmux-absent-strict", ConnResult{}, "", true, ChannelUnknown}, // clean skip, NOT fail
		{"unstageable-lax", ConnResult{}, stageErr, false, ChannelWarn},
		{"unstageable-strict", ConnResult{}, stageErr, true, ChannelFail}, // fail-open closed
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := contained()
			obs.ApprovalTmuxSock = tc.r
			obs.ApprovalFixtureErr = tc.fixErr
			v := Classify(obs, Policy{RequireControls: tc.strict})
			if got := chanStatus(v, ChanApprovalTmuxSocket); got != tc.want {
				t.Fatalf("socket %s: got %s, want %s", tc.name, got, tc.want)
			}
			if tc.want == ChannelFail && !v.ShouldFailClosed() {
				t.Fatalf("socket %s: a Fail must fail the gate closed", tc.name)
			}
		})
	}
}

// send-keys: exit-0 ⇒ leak (the agent drove the popup); non-zero exit ⇒ ok
// (refused); could-not-run / no-fixture ⇒ Unknown skip (never fails the gate,
// even under strict, so a tmux-less host still passes).
func TestClassify_ApprovalSendKeys(t *testing.T) {
	const stageErr = "tmux present but fixture unstageable"
	for _, tc := range []struct {
		name   string
		e      ExecResult
		fixErr string
		strict bool
		want   ChannelStatus
	}{
		{"refused", ExecResult{Attempted: true, Ran: true, ExitZero: false, ExitCode: 1}, "", true, ChannelOK},
		{"landed", ExecResult{Attempted: true, Ran: true, ExitZero: true}, "", true, ChannelLeak},
		{"could-not-exec", ExecResult{Attempted: true, Ran: false, Err: "exec tmux: not found"}, "", true, ChannelUnknown},
		{"tmux-absent", ExecResult{}, "", true, ChannelUnknown},
		{"unstageable-lax", ExecResult{}, stageErr, false, ChannelWarn},
		{"unstageable-strict", ExecResult{}, stageErr, true, ChannelFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := contained()
			obs.ApprovalSendKeys = tc.e
			obs.ApprovalFixtureErr = tc.fixErr
			v := Classify(obs, Policy{RequireControls: tc.strict})
			if got := chanStatus(v, ChanApprovalTmuxSendKeys); got != tc.want {
				t.Fatalf("sendkeys %s: got %s, want %s", tc.name, got, tc.want)
			}
			if tc.want == ChannelUnknown && v.ShouldFailClosed() {
				t.Fatalf("sendkeys %s: a clean skip must not fail the gate closed", tc.name)
			}
		})
	}
}

// A tmux-ABSENT host (no fixture, no stage error) must still pass the gate: the
// two tmux channels go Unknown but /dev/tty + $TMUX stay hard, and nothing fails
// closed. (Distinct from tmux-present-but-unstageable, which fails closed.)
func TestClassify_TmuxAbsent_GatePasses(t *testing.T) {
	obs := contained()
	obs.ApprovalTmuxSock = ConnResult{}
	obs.ApprovalSendKeys = ExecResult{}
	obs.ApprovalFixtureErr = "" // tmux genuinely absent
	v := Classify(obs, Policy{RequireControls: true})
	if v.ShouldFailClosed() {
		t.Fatalf("tmux-absent must not fail the gate:\n%s", v.String())
	}
	if got := chanStatus(v, ChanApprovalTTY); got != ChannelOK {
		t.Fatalf("tty must stay ok without a tmux fixture, got %s", got)
	}
}

// tmux PRESENT but the fixture could not be staged must fail closed under the
// live gate's RequireControls — the enforcement proof was skipped while a real
// approval surface exists.
func TestClassify_TmuxUnstageable_FailsClosed(t *testing.T) {
	obs := contained()
	obs.ApprovalTmuxSock = ConnResult{}
	obs.ApprovalSendKeys = ExecResult{}
	obs.ApprovalFixtureErr = "tmux present but fixture unstageable"
	v := Classify(obs, Policy{RequireControls: true})
	if !v.ShouldFailClosed() {
		t.Fatalf("tmux-present-but-unstageable must fail the gate closed:\n%s", v.String())
	}
}

func TestIsDenied(t *testing.T) {
	if !isDenied(syscall.EPERM) || !isDenied(syscall.EACCES) {
		t.Fatalf("EPERM/EACCES must be denials")
	}
	if isDenied(syscall.ECONNREFUSED) || isDenied(errors.New("timeout")) {
		t.Fatalf("ECONNREFUSED/timeout must NOT be denials")
	}
}

// VerifyContainment must return ErrNonoUnavailable (skip) when nono is absent —
// a skip is never a pass.
func TestVerifyContainment_SkipsWhenNonoAbsent(t *testing.T) {
	_, err := VerifyContainment(VerifyParams{ReinBin: "/bin/true", NonoBin: "/nonexistent/nono"})
	if !errors.Is(err, ErrNonoUnavailable) {
		t.Fatalf("want ErrNonoUnavailable, got %v", err)
	}
}

// Live end-to-end containment check. Skips cleanly when nono is not installed so
// CI without nono passes. When nono IS present it launches the real probe and
// asserts the gate does not falsely fail on a genuinely-contained sandbox.
func TestLiveContainment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live nono containment probe in -short")
	}
	if _, err := exec.LookPath("nono"); err != nil {
		t.Skip("nono not installed; skipping live containment probe")
	}
	reinBin := buildReinForProbe(t)

	verdict, err := VerifyContainment(VerifyParams{
		ReinBin: reinBin,
		Timeout: 90 * time.Second,
	})
	t.Logf("live containment verdict:\n%s", verdict.String())
	for _, w := range verdict.Warnings() {
		t.Logf("WARNING: %s", w)
	}
	if err != nil {
		// A real leak is a genuine finding, not a flaky test — surface it.
		t.Fatalf("live containment gate failed closed: %v", err)
	}
	// The crux must be an explicit denial, not merely unknown.
	if got := chanStatus(verdict, ChanArbitraryLoopback); got != ChannelOK {
		t.Errorf("arbitrary-loopback control = %s, want ok (explicit denial)", got)
	}
	if got := chanStatus(verdict, ChanProxyReachable); got != ChannelOK {
		t.Errorf("positive control (proxy reachable) = %s, want ok", got)
	}
	// Approval-channel isolation (§3e): /dev/tty and $TMUX are always measurable
	// and must be isolated.
	if got := chanStatus(verdict, ChanApprovalTTY); got != ChannelOK {
		t.Errorf("approval /dev/tty = %s, want ok (unopenable)", got)
	}
	if got := chanStatus(verdict, ChanApprovalTmuxEnv); got != ChannelOK {
		t.Errorf("approval $TMUX env = %s, want ok (absent)", got)
	}
	// The tmux socket/send-keys channels are only asserted when a fixture could be
	// staged (tmux present); otherwise they legitimately skip (Unknown).
	if _, err := exec.LookPath("tmux"); err == nil {
		if got := chanStatus(verdict, ChanApprovalTmuxSocket); got != ChannelOK {
			t.Errorf("approval tmux socket = %s, want ok (connect denied)", got)
		}
		if got := chanStatus(verdict, ChanApprovalTmuxSendKeys); got != ChannelOK {
			t.Errorf("approval tmux send-keys = %s, want ok (refused)", got)
		}
	}
}

// buildReinForProbe compiles the rein binary so the live probe can invoke
// `rein __nono-probe`.
func buildReinForProbe(t *testing.T) string {
	t.Helper()
	bin := t.TempDir() + "/rein"
	cmd := exec.Command("go", "build", "-o", bin, "github.com/TomHennen/rein/cmd/rein")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build rein for probe: %v\n%s", err, out)
	}
	if strings.TrimSpace(bin) == "" {
		t.Fatal("empty bin path")
	}
	return bin
}
