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
// proxy reachable, UDP denied.
func contained() Observations {
	return Observations{
		Creds:          []CredObs{{Path: "/home/u/.ssh/id_ed25519", Existed: true, Denied: true}},
		DirectExternal: denied("1.1.1.1:443"),
		GitHubDirect:   denied("140.82.121.3:443"),
		PlantedLoop:    denied("127.0.0.1:47999"),
		NonoProxy:      reached("127.0.0.1:33777"),
		UDPExternal:    denied("8.8.8.8:53"),
	}
}

func chanStatus(v Verdict, ch Channel) Status {
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
	for _, ch := range []Channel{ChanCredentials, ChanDirectTCP, ChanArbitraryLoopback, ChanGitHubViaProxy, ChanProxyReachable, ChanUDPEgress} {
		if got := chanStatus(v, ch); got != StatusOK {
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
	if got := chanStatus(v, ChanArbitraryLoopback); got != StatusLeak {
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
			if got := chanStatus(v, tc.ch); got != StatusUnknown {
				t.Errorf("%s refused: got %s, want unknown", tc.ch, got)
			}
			if v.ShouldFailClosed() {
				t.Errorf("a lone unknown must not fail the gate closed:\n%s", v.String())
			}
		})
	}
}

func TestClassify_DirectEgressReached_IsLeak(t *testing.T) {
	obs := contained()
	obs.DirectExternal = reached("1.1.1.1:443")
	v := Classify(obs, Policy{})
	if chanStatus(v, ChanDirectTCP) != StatusLeak || !v.ShouldFailClosed() {
		t.Fatalf("direct external reach must leak + fail closed:\n%s", v.String())
	}
}

func TestClassify_GitHubDirectReached_IsLeak(t *testing.T) {
	obs := contained()
	obs.GitHubDirect = reached("140.82.121.3:443")
	v := Classify(obs, Policy{})
	if chanStatus(v, ChanGitHubViaProxy) != StatusLeak || !v.ShouldFailClosed() {
		t.Fatalf("direct github reach must leak + fail closed:\n%s", v.String())
	}
}

func TestClassify_CredReadable_IsLeak(t *testing.T) {
	obs := contained()
	obs.Creds = []CredObs{{Path: "/home/u/.config/rein-credentials/app.pem", Existed: true, Readable: true}}
	v := Classify(obs, Policy{})
	if chanStatus(v, ChanCredentials) != StatusLeak || !v.ShouldFailClosed() {
		t.Fatalf("readable credential must leak + fail closed:\n%s", v.String())
	}
}

func TestClassify_SentinelReadable_IsLeak(t *testing.T) {
	obs := contained()
	obs.Creds = []CredObs{{Path: "/home/u/.ssh/rein-sentinel", Sentinel: true, Existed: true, Readable: true}}
	v := Classify(obs, Policy{})
	if chanStatus(v, ChanCredentials) != StatusLeak {
		t.Fatalf("readable sentinel must leak, got %s", chanStatus(v, ChanCredentials))
	}
}

func TestClassify_NoCredsPresent_IsUnknown(t *testing.T) {
	obs := contained()
	obs.Creds = []CredObs{{Path: "/nope", Existed: false, Denied: true}}
	v := Classify(obs, Policy{})
	if got := chanStatus(v, ChanCredentials); got != StatusUnknown {
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
	if got := chanStatus(v, ChanProxyReachable); got != StatusFail {
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
	if got := chanStatus(v, ChanUDPEgress); got != StatusWarn {
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
	if got := chanStatus(v, ChanUDPEgress); got != StatusLeak {
		t.Fatalf("UDP open under FailOnUDP must leak, got %s", got)
	}
	if !v.ShouldFailClosed() {
		t.Fatalf("UDP leak under policy must fail closed")
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
	if got := chanStatus(verdict, ChanArbitraryLoopback); got != StatusOK {
		t.Errorf("arbitrary-loopback control = %s, want ok (explicit denial)", got)
	}
	if got := chanStatus(verdict, ChanProxyReachable); got != StatusOK {
		t.Errorf("positive control (proxy reachable) = %s, want ok", got)
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
