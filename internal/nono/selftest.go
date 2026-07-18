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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
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
)

// Status is the per-channel verdict.
type Status string

const (
	// StatusOK — contained as expected (a positive denial was observed).
	StatusOK Status = "ok"
	// StatusLeak — containment FAILED: the channel was open when it must be shut.
	StatusLeak Status = "leak"
	// StatusFail — the gate cannot trust its own result (e.g. the positive
	// control failed, or the probe could not run). Fails closed, distinct from a
	// containment leak.
	StatusFail Status = "fail"
	// StatusWarn — an accepted residual is open (UDP), surfaced loudly.
	StatusWarn Status = "warn"
	// StatusUnknown — could not determine (target absent, ambiguous errno, no
	// network). Never counted as contained; never fails the gate on its own.
	StatusUnknown Status = "unknown"
)

// Policy tunes classification.
type Policy struct {
	// FailOnUDP promotes an open UDP channel from a warning to a leak. Default
	// false: UDP-open is the documented residual.
	FailOnUDP bool
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
	Errors         []string   `json:"errors,omitempty"`
}

// ChannelVerdict is one channel's classified result.
type ChannelVerdict struct {
	Channel Channel
	Status  Status
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
		if c.Status == StatusLeak || c.Status == StatusFail {
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
		if c.Status == StatusWarn {
			w = append(w, fmt.Sprintf("%s: %s", c.Channel, c.Detail))
		}
	}
	return w
}

// Leaks returns the channels that failed closed (leak or gate-integrity fail).
func (v Verdict) Leaks() []ChannelVerdict {
	var out []ChannelVerdict
	for _, c := range v.Channels {
		if c.Status == StatusLeak || c.Status == StatusFail {
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
			"direct external connect denied"),
		classifyNegative(ChanArbitraryLoopback, obs.PlantedLoop,
			"arbitrary loopback listener REACHED — rein's no-proxy-auth assumption is BROKEN",
			"arbitrary loopback connect denied (rein's listener is safe without proxy-auth)"),
		classifyNegative(ChanGitHubViaProxy, obs.GitHubDirect,
			"github reachable directly, bypassing the proxy (token-injection boundary lost)",
			"direct github connect denied — github egress must route through the proxy"),
		classifyProxyControl(obs.NonoProxy),
		classifyUDP(obs.UDPExternal, policy),
	)
	return v
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
		return ChannelVerdict{ChanCredentials, StatusLeak,
			fmt.Sprintf("credential(s) READABLE inside the sandbox: %s (deny_credentials failed)", strings.Join(leaked, ", "))}
	case existed == 0:
		return ChannelVerdict{ChanCredentials, StatusUnknown,
			"no credential files present to test (cannot prove deny_credentials)"}
	default:
		return ChannelVerdict{ChanCredentials, StatusOK,
			fmt.Sprintf("%d credential file(s) present, all unreadable inside the sandbox", existed)}
	}
}

// classifyNegative classifies a channel that must be BLOCKED.
func classifyNegative(ch Channel, r ConnResult, leakDetail, okDetail string) ChannelVerdict {
	switch {
	case !r.Attempted:
		return ChannelVerdict{ch, StatusUnknown, "not attempted (no target)"}
	case r.Succeeded:
		return ChannelVerdict{ch, StatusLeak, leakDetail + " [" + r.Target + "]"}
	case r.Denied:
		return ChannelVerdict{ch, StatusOK, okDetail + " [" + r.Target + "]"}
	default:
		return ChannelVerdict{ch, StatusUnknown,
			fmt.Sprintf("connect to %s failed but not with a denial errno (%s); cannot conclude containment", r.Target, r.Err)}
	}
}

// classifyProxyControl is the positive control: nono's proxy MUST be reachable.
func classifyProxyControl(r ConnResult) ChannelVerdict {
	switch {
	case !r.Attempted:
		return ChannelVerdict{ChanProxyReachable, StatusFail,
			"positive control not run (HTTPS_PROXY absent?) — cannot trust the negative checks"}
	case r.Succeeded:
		return ChannelVerdict{ChanProxyReachable, StatusOK,
			"nono's proxy is reachable from inside [" + r.Target + "]"}
	default:
		return ChannelVerdict{ChanProxyReachable, StatusFail,
			fmt.Sprintf("nono's proxy UNREACHABLE [%s] (%s) — misconfig; the whole egress path is dead, failing closed", r.Target, r.Err)}
	}
}

func classifyUDP(r ConnResult, policy Policy) ChannelVerdict {
	switch {
	case !r.Attempted:
		return ChannelVerdict{ChanUDPEgress, StatusUnknown, "not attempted"}
	case r.Succeeded:
		if policy.FailOnUDP {
			return ChannelVerdict{ChanUDPEgress, StatusLeak,
				"UDP egress OPEN and policy requires it closed [" + r.Target + "]"}
		}
		return ChannelVerdict{ChanUDPEgress, StatusWarn,
			"UDP egress OPEN (accepted residual — a prompt-injected agent has a UDP exfil channel) [" + r.Target + "]"}
	case r.Denied:
		return ChannelVerdict{ChanUDPEgress, StatusOK, "UDP egress denied [" + r.Target + "]"}
	default:
		return ChannelVerdict{ChanUDPEgress, StatusUnknown,
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
	DialTimeoutMS   int          `json:"dial_timeout_ms"`
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
