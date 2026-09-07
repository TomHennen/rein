package runbroker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExpiredPure(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	cases := []struct {
		name        string
		last, now   time.Time
		idle        time.Duration
		wantExpired bool
	}{
		{"fresh", base, base, time.Minute, false},
		{"idle trips", base, base.Add(2 * time.Minute), time.Minute, true},
		{"recent activity defers idle", base.Add(90 * time.Second), base.Add(2 * time.Minute), time.Minute, false},
		{"zero idle never expires", base, base.Add(10 * time.Hour), 0, false},
		{"exactly at idle boundary trips", base, base.Add(time.Minute), time.Minute, true},
		// #190: there is no wall-clock cap — a run that has been alive for
		// hours but is still busy must NOT expire.
		{"long-lived but active never expires", base.Add(10 * time.Hour), base.Add(10 * time.Hour), 30 * time.Minute, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := expired(tc.last, tc.now, tc.idle); got != tc.wantExpired {
				t.Errorf("expired = %v, want %v", got, tc.wantExpired)
			}
		})
	}
}

func TestDeriveCheckInterval(t *testing.T) {
	// Capped at 30s for a large bound; a quarter of the bound otherwise;
	// floored at 1s.
	if got := deriveCheckInterval(30 * time.Minute); got != 30*time.Second {
		t.Errorf("30m bound interval = %s, want 30s", got)
	}
	if got := deriveCheckInterval(40 * time.Second); got != 10*time.Second {
		t.Errorf("40s idle interval = %s, want 10s", got)
	}
	if got := deriveCheckInterval(2 * time.Second); got != time.Second {
		t.Errorf("tiny bound interval = %s, want the 1s floor", got)
	}
}

// TestMarkActivityFeedsExpiry deterministically verifies the wiring the monitor
// depends on: markActivity updates lastActivity, and a recent activity defers
// the idle bound (no timers involved).
func TestMarkActivityFeedsExpiry(t *testing.T) {
	h := &Host{}
	start := time.Unix(1000, 0)
	h.lastActivity.Store(start.UnixNano())
	now := start.Add(10 * time.Minute)

	if !expired(time.Unix(0, h.lastActivity.Load()), now, 5*time.Minute) {
		t.Error("no activity since start under a 5m idle bound should expire")
	}
	h.markActivity(now.Add(-time.Minute)) // activity 1m ago
	if expired(time.Unix(0, h.lastActivity.Load()), now, 5*time.Minute) {
		t.Error("activity 1m ago must defer a 5m idle bound")
	}
}

// TestHostIdleExpiryKeepsServing is the #190 regression guard: on an idle trip
// the host fires OnExpire (where the caller withdraws the write approval) but
// KEEPS SERVING — the agent's reads must still flow through the proxy. Before
// #190 this same moment tore the proxy down.
func TestHostIdleExpiryKeepsServing(t *testing.T) {
	fired := make(chan struct{}, 4)
	var audit syncBuffer
	h, _ := startHost(t, Config{
		SessionID:      "s",
		EmptyPathScope: "allow",
		IdleTimeout:    40 * time.Millisecond,
		checkInterval:  5 * time.Millisecond,
		Audit:          &audit,
		OnExpire:       func() error { fired <- struct{}{}; return nil },
	})
	waitFired(t, fired, "idle expiry")

	// The read path must still work: this is the whole point of re-attesting
	// in place rather than ending the session.
	c := clientThrough(t, h)
	resp, err := c.Get("https://api.github.com/repos/o/r")
	if err != nil {
		t.Fatalf("read through the proxy failed after the idle lock; the host must keep serving: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got := audit.String(); !strings.Contains(got, "decision=expired-idle") {
		t.Errorf("audit log missing the expired-idle entry; got:\n%s", got)
	}
}

// TestHostActivityDefersIdleThenExpires is the end-to-end guard on the idle-
// clock WIRING (proxy OnActivity -> markActivity -> idle deferral): while
// requests flow faster than the idle bound, OnExpire must NOT fire; once traffic
// stops, it must. This fails if the p.onActivity() hook is removed from the
// proxy request path (lastActivity would stay pinned at launch and expiry would
// fire mid-traffic).
func TestHostActivityDefersIdleThenExpires(t *testing.T) {
	fired := make(chan struct{}, 4)
	h, _ := startHost(t, Config{
		SessionID:      "s",
		EmptyPathScope: "allow",
		IdleTimeout:    150 * time.Millisecond,
		checkInterval:  10 * time.Millisecond,
		OnExpire:       func() error { fired <- struct{}{}; return nil },
	})
	c := clientThrough(t, h)

	// Phase 1: drive traffic every 25ms for 350ms (> 2x the idle bound). Each
	// request must reset the idle clock, so NO expiry may fire in this window.
	// Without the OnActivity wiring, expiry would fire at ~150ms — mid-window.
	deadline := time.Now().Add(350 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case <-fired:
			t.Fatal("idle expiry fired WHILE traffic was flowing — proxy activity is not resetting the idle clock")
		default:
		}
		getOK(t, c, "https://api.github.com/repos/o/r")
		time.Sleep(25 * time.Millisecond)
	}

	// Phase 2: go quiet — expiry must now fire.
	waitFired(t, fired, "idle expiry after traffic stopped")
}

// TestHostIdleRelocksAfterActivity pins the RE-ARM half of #190: the monitor is
// no longer one-shot. Lock, then work, then go quiet again ⇒ lock again.
func TestHostIdleRelocksAfterActivity(t *testing.T) {
	fired := make(chan struct{}, 4)
	h, _ := startHost(t, Config{
		SessionID:      "s",
		EmptyPathScope: "allow",
		IdleTimeout:    40 * time.Millisecond,
		checkInterval:  5 * time.Millisecond,
		OnExpire:       func() error { fired <- struct{}{}; return nil },
	})
	waitFired(t, fired, "first idle lock")

	// One request re-arms the monitor; going quiet must lock a SECOND time.
	getOK(t, clientThrough(t, h), "https://api.github.com/repos/o/r")
	waitFired(t, fired, "second idle lock after activity")
}

// TestHostIdleDoesNotRelockWithoutActivity is the other half of the re-arm
// rule, and the guard against banner-spam: a run that stays quiet after its
// lock must NOT be locked again every idle period.
func TestHostIdleDoesNotRelockWithoutActivity(t *testing.T) {
	var count atomic.Int32
	fired := make(chan struct{}, 8)
	startHost(t, Config{
		SessionID:      "s",
		EmptyPathScope: "allow",
		IdleTimeout:    30 * time.Millisecond,
		checkInterval:  5 * time.Millisecond,
		OnExpire:       func() error { count.Add(1); fired <- struct{}{}; return nil },
	})
	waitFired(t, fired, "idle lock")

	// Stay quiet for several more idle periods. Exactly one lock must have
	// happened: no activity, nothing new to withdraw.
	time.Sleep(200 * time.Millisecond)
	if got := count.Load(); got != 1 {
		t.Errorf("OnExpire fired %d times while the run stayed quiet, want 1", got)
	}
}

// TestHostExpireErrorStopsProxy is the fail-closed fallback: when the caller
// cannot withdraw the write approval, the host reverts to the pre-#190
// behavior and tears the proxy down so the next request fails closed.
func TestHostExpireErrorStopsProxy(t *testing.T) {
	fired := make(chan struct{}, 4)
	h, _ := startHost(t, Config{
		SessionID:      "s",
		EmptyPathScope: "allow",
		IdleTimeout:    40 * time.Millisecond,
		checkInterval:  5 * time.Millisecond,
		OnExpire:       func() error { fired <- struct{}{}; return errors.New("cannot rewrite the approval record") },
	})
	waitFired(t, fired, "idle expiry")

	// The teardown races the callback's return; poll briefly for the socket to
	// stop serving rather than assume it is already gone.
	c := clientThrough(t, h)
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := c.Get("https://api.github.com/repos/o/r")
		if err != nil {
			return // proxy is down: fail closed as intended
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatal("proxy still serving after OnExpire reported it could not withdraw the approval")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitFired waits for one OnExpire firing, failing with what was expected.
func waitFired(t *testing.T, fired <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not fire", what)
	}
}

// syncBuffer is a mutex-guarded bytes.Buffer: the audit log is written from the
// monitor goroutine and read by the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestConcurrentRunsIsolated is the CP4 concurrent-isolation invariant: two
// runbroker Hosts share no approval, token, or scope state — approving (or
// denying) one has no effect on the other, and their injected tokens never
// cross. This is the in-process analogue of "concurrent runs isolated".
func TestConcurrentRunsIsolated(t *testing.T) {
	var aApprovals, bApprovals atomic.Int32

	hA, upA := startHost(t, Config{
		SessionID:      "A",
		EmptyPathScope: "allow",
		Approve:        func(string) bool { aApprovals.Add(1); return true }, // A approves
		MintRead:       func(context.Context) (string, time.Time, error) { return "A-READ", time.Now().Add(time.Hour), nil },
		MintWrite:      func(context.Context) (string, time.Time, error) { return "A-WRITE", time.Now().Add(time.Hour), nil },
	})
	hB, upB := startHost(t, Config{
		SessionID:      "B",
		EmptyPathScope: "allow",
		Approve:        func(string) bool { bApprovals.Add(1); return false }, // B denies
		MintRead:       func(context.Context) (string, time.Time, error) { return "B-READ", time.Now().Add(time.Hour), nil },
		MintWrite:      func(context.Context) (string, time.Time, error) { return "B-WRITE", time.Now().Add(time.Hour), nil },
	})

	cA, cB := clientThrough(t, hA), clientThrough(t, hB)

	// Reads: each host injects its OWN token — no shared cache bleed.
	getOK(t, cA, "https://api.github.com/repos/o/r")
	getOK(t, cB, "https://api.github.com/repos/o/r")
	if got := upA.lastAuth(); got != "Bearer A-READ" {
		t.Errorf("host A read auth = %q, want Bearer A-READ", got)
	}
	if got := upB.lastAuth(); got != "Bearer B-READ" {
		t.Errorf("host B read auth = %q, want Bearer B-READ", got)
	}

	// Writes: A's approval lets A's write through; B's denial blocks B's write.
	// Crucially B is prompted independently — A's granted approval does NOT
	// pre-approve B (no shared approval state).
	getStatus(t, cA, "https://github.com/o/r.git/info/refs?service=git-receive-pack")
	getStatus(t, cB, "https://github.com/o/r.git/info/refs?service=git-receive-pack")

	if aApprovals.Load() != 1 {
		t.Errorf("host A approvals = %d, want 1", aApprovals.Load())
	}
	if bApprovals.Load() != 1 {
		t.Errorf("host B approvals = %d, want 1 (B must be prompted independently, not pre-approved by A)", bApprovals.Load())
	}
	// A's write reached upstream with an injected (Basic) write credential.
	if got := upA.lastAuth(); !strings.HasPrefix(got, "Basic ") {
		t.Errorf("host A write auth = %q, want an injected Basic write credential", got)
	}
	// B's denied write never reached upstream: its last-seen auth is still the
	// earlier read token, proving the write was blocked AND no A state leaked in.
	if got := upB.lastAuth(); got != "Bearer B-READ" {
		t.Errorf("host B upstream auth = %q; a denied write must not reach upstream", got)
	}
}

// getOK issues a GET and drains/closes the body, failing on a transport error.
func getOK(t *testing.T, c *http.Client, url string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// getStatus issues a GET, drains/closes the body, and ignores a transport error
// (a denied write may surface as a dropped connection). It exists to exercise
// the write path for its side effects (approval prompt + injection decision).
func getStatus(t *testing.T, c *http.Client, url string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}
