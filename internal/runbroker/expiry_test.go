package runbroker

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExpiredPure(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	cases := []struct {
		name             string
		last, start, now time.Time
		idle, hard       time.Duration
		wantReason       string
		wantExpired      bool
	}{
		{"fresh", base, base, base, time.Minute, time.Hour, "", false},
		{"idle trips", base, base, base.Add(2 * time.Minute), time.Minute, time.Hour, "idle", true},
		{"recent activity defers idle", base.Add(90 * time.Second), base, base.Add(2 * time.Minute), time.Minute, time.Hour, "", false},
		{"hard trips (idle disabled)", base.Add(119 * time.Minute), base, base.Add(2 * time.Hour), 0, time.Hour, "hard-ttl", true},
		{"hard beats idle when both trip", base, base, base.Add(2 * time.Hour), time.Minute, time.Hour, "hard-ttl", true},
		{"both disabled never expires", base, base, base.Add(10 * time.Hour), 0, 0, "", false},
		{"exactly at idle boundary trips", base, base, base.Add(time.Minute), time.Minute, 0, "idle", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, exp := expired(tc.last, tc.start, tc.now, tc.idle, tc.hard)
			if exp != tc.wantExpired || reason != tc.wantReason {
				t.Errorf("expired = (%q,%v), want (%q,%v)", reason, exp, tc.wantReason, tc.wantExpired)
			}
		})
	}
}

func TestDeriveCheckInterval(t *testing.T) {
	// Capped at 30s for large bounds; a quarter of the tighter bound otherwise;
	// floored at 1s.
	if got := deriveCheckInterval(30*time.Minute, 4*time.Hour); got != 30*time.Second {
		t.Errorf("large bounds interval = %s, want 30s", got)
	}
	if got := deriveCheckInterval(40*time.Second, 0); got != 10*time.Second {
		t.Errorf("40s idle interval = %s, want 10s", got)
	}
	if got := deriveCheckInterval(2*time.Second, 0); got != time.Second {
		t.Errorf("tiny bound interval = %s, want the 1s floor", got)
	}
}

// TestMarkActivityFeedsExpiry deterministically verifies the wiring the monitor
// depends on: markActivity updates lastActivity, and a recent activity defers
// the idle bound (no timers involved).
func TestMarkActivityFeedsExpiry(t *testing.T) {
	h := &Host{start: time.Unix(1000, 0)}
	h.lastActivity.Store(h.start.UnixNano())
	now := h.start.Add(10 * time.Minute)

	if _, exp := expired(time.Unix(0, h.lastActivity.Load()), h.start, now, 5*time.Minute, time.Hour); !exp {
		t.Error("no activity since start under a 5m idle bound should expire")
	}
	h.markActivity(now.Add(-time.Minute)) // activity 1m ago
	if _, exp := expired(time.Unix(0, h.lastActivity.Load()), h.start, now, 5*time.Minute, time.Hour); exp {
		t.Error("activity 1m ago must defer a 5m idle bound")
	}
}

// TestHostIdleExpiry drives the real monitor goroutine with a tiny idle bound
// and asserts OnExpire fires with reason "idle" and the proxy is torn down.
func TestHostIdleExpiry(t *testing.T) {
	fired := make(chan string, 1)
	h, _ := startHost(t, Config{
		SessionID:      "s",
		EmptyPathScope: "allow",
		IdleTimeout:    40 * time.Millisecond,
		checkInterval:  5 * time.Millisecond,
		OnExpire:       func(reason string) { fired <- reason },
	})
	select {
	case r := <-fired:
		if r != "idle" {
			t.Errorf("expiry reason = %q, want idle", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle expiry did not fire")
	}
	// Proxy is torn down: a fresh request must now fail (fail closed).
	c := clientThrough(t, h)
	if resp, err := c.Get("https://api.github.com/repos/o/r"); err == nil {
		resp.Body.Close()
		t.Error("request succeeded after idle expiry; proxy was not stopped")
	}
}

// TestHostActivityDefersIdleThenExpires is the end-to-end guard on the idle-
// clock WIRING (proxy OnActivity -> markActivity -> idle deferral): while
// requests flow faster than the idle bound, OnExpire must NOT fire; once traffic
// stops, it must. This fails if the p.onActivity() hook is removed from the
// proxy request path (lastActivity would stay pinned at launch and expiry would
// fire mid-traffic).
func TestHostActivityDefersIdleThenExpires(t *testing.T) {
	fired := make(chan string, 1)
	h, _ := startHost(t, Config{
		SessionID:      "s",
		EmptyPathScope: "allow",
		IdleTimeout:    150 * time.Millisecond,
		checkInterval:  10 * time.Millisecond,
		OnExpire:       func(reason string) { fired <- reason },
	})
	c := clientThrough(t, h)

	// Phase 1: drive traffic every 25ms for 350ms (> 2x the idle bound). Each
	// request must reset the idle clock, so NO expiry may fire in this window.
	// Without the OnActivity wiring, expiry would fire at ~150ms — mid-window.
	deadline := time.Now().Add(350 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case r := <-fired:
			t.Fatalf("idle expiry fired (%s) WHILE traffic was flowing — proxy activity is not resetting the idle clock", r)
		default:
		}
		getOK(t, c, "https://api.github.com/repos/o/r")
		time.Sleep(25 * time.Millisecond)
	}

	// Phase 2: go quiet — expiry must now fire.
	select {
	case r := <-fired:
		if r != "idle" {
			t.Errorf("expiry reason = %q, want idle", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle expiry did not fire after traffic stopped")
	}
}

// TestHostHardTTLExpiry asserts the hard cap trips regardless of activity.
func TestHostHardTTLExpiry(t *testing.T) {
	fired := make(chan string, 1)
	startHost(t, Config{
		SessionID:      "s",
		EmptyPathScope: "allow",
		HardTTL:        40 * time.Millisecond,
		checkInterval:  5 * time.Millisecond,
		OnExpire:       func(reason string) { fired <- reason },
	})
	select {
	case r := <-fired:
		if r != "hard-ttl" {
			t.Errorf("expiry reason = %q, want hard-ttl", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hard-ttl expiry did not fire")
	}
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
