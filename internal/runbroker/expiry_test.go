package runbroker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	if got := deriveCheckInterval(30*time.Minute, 4*time.Hour); got != 30*time.Second {
		t.Errorf("30m bound interval = %s, want 30s", got)
	}
	if got := deriveCheckInterval(40*time.Second, 0); got != 10*time.Second {
		t.Errorf("40s idle interval = %s, want 10s", got)
	}
	if got := deriveCheckInterval(2*time.Second, 0); got != time.Second {
		t.Errorf("tiny bound interval = %s, want the 1s floor", got)
	}
}

// TestApprovalExpiredPure pins the age bound's policy: measured from the
// human's last confirmation, unaffected by anything the agent does.
func TestApprovalExpiredPure(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	cases := []struct {
		name        string
		confirmedAt time.Time
		now         time.Time
		ttl         time.Duration
		want        bool
	}{
		{"fresh confirmation", base, base.Add(time.Hour), 4 * time.Hour, false},
		{"aged out", base, base.Add(4 * time.Hour), 4 * time.Hour, true},
		{"exactly at the bound trips", base, base.Add(4 * time.Hour), 4 * time.Hour, true},
		{"zero ttl disables", base, base.Add(100 * time.Hour), 0, false},
		{"no confirmation never expires", time.Time{}, base.Add(100 * time.Hour), 4 * time.Hour, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := approvalExpired(tc.confirmedAt, tc.now, tc.ttl); got != tc.want {
				t.Errorf("approvalExpired = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDueExpiry covers the whole policy without timers: which bound wins, and
// the two re-arm gates.
func TestDueExpiry(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	approvedAt := func(at time.Time, ok bool) func() (time.Time, bool) {
		return func() (time.Time, bool) { return at, ok }
	}
	const idle = 30 * time.Minute
	const ttl = 4 * time.Hour

	// Busy run, fresh approval: nothing is due.
	if _, _, ok := dueExpiry(dueInput{
		now: base.Add(time.Hour), lastActivity: base.Add(time.Hour), idle: idle, approvalTTL: ttl,
		lastApproval: approvedAt(base.Add(time.Hour), true),
	}); ok {
		t.Error("a busy run with a fresh approval must not expire")
	}

	// Busy run, OLD approval: the age bound trips even though activity never
	// stopped. This is the whole point of the bound.
	reason, _, ok := dueExpiry(dueInput{
		now: base.Add(5 * time.Hour), lastActivity: base.Add(5 * time.Hour), idle: idle, approvalTTL: ttl,
		lastApproval: approvedAt(base, true),
	})
	if !ok || reason != ExpireApprovalAge {
		t.Errorf("busy run with a 5h-old approval = (%q,%v), want approval-age", reason, ok)
	}

	// Nothing confirmed: the age bound has no clock, so only idle can fire.
	reason, _, ok = dueExpiry(dueInput{
		now: base.Add(5 * time.Hour), lastActivity: base.Add(5 * time.Hour), idle: idle, approvalTTL: ttl,
		lastApproval: approvedAt(time.Time{}, false),
	})
	if ok {
		t.Errorf("a run with no confirmed issue has nothing to expire; got %q", reason)
	}

	// Age re-arm: the confirmation already acted on must not trip again, but a
	// FRESHER one (the human re-confirmed) must.
	old := base
	if _, _, ok := dueExpiry(dueInput{
		now: base.Add(5 * time.Hour), lastActivity: base.Add(5 * time.Hour), idle: idle, approvalTTL: ttl,
		lastApproval: approvedAt(old, true), actedApproval: old,
	}); ok {
		t.Error("the same confirmation must not trip the age bound twice")
	}
	renewed := base.Add(30 * time.Minute)
	if reason, _, ok := dueExpiry(dueInput{
		now: renewed.Add(5 * time.Hour), lastActivity: renewed.Add(5 * time.Hour), idle: idle, approvalTTL: ttl,
		lastApproval: approvedAt(renewed, true), actedApproval: old,
	}); !ok || reason != ExpireApprovalAge {
		t.Errorf("a re-confirmation restarts the clock and can trip again; got (%q,%v)", reason, ok)
	}

	// Idle re-arm is unchanged, and a lock of EITHER kind arms it.
	lock := base.Add(time.Hour)
	if _, _, ok := dueExpiry(dueInput{
		now: lock.Add(2 * time.Hour), lastActivity: base, idle: idle, approvalTTL: 0,
		lastLock: lock,
	}); ok {
		t.Error("no activity since the last lock: idle must not re-fire")
	}
	if reason, _, ok := dueExpiry(dueInput{
		now: lock.Add(2 * time.Hour), lastActivity: lock.Add(time.Minute), idle: idle, approvalTTL: 0,
		lastLock: lock,
	}); !ok || reason != ExpireIdle {
		t.Errorf("activity since the last lock re-arms idle; got (%q,%v)", reason, ok)
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
		OnExpire:       func(ExpireReason) (bool, error) { fired <- struct{}{}; return true, nil },
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

	// The audit records the OUTCOME, not just that the monitor fired: a real
	// withdrawal must be distinguishable from a trip that found nothing.
	if got := audit.String(); !strings.Contains(got, "decision=expired-idle-withdrawn") {
		t.Errorf("audit log missing the expired-idle-withdrawn entry; got:\n%s", got)
	}
}

// TestHostIdleAuditsNoopOutcome: a trip where the caller withdrew nothing is
// recorded as such, so the audit trail never implies an approval was revoked
// when none existed.
func TestHostIdleAuditsNoopOutcome(t *testing.T) {
	fired := make(chan struct{}, 4)
	var audit syncBuffer
	startHost(t, Config{
		SessionID:      "s",
		EmptyPathScope: "allow",
		IdleTimeout:    40 * time.Millisecond,
		checkInterval:  5 * time.Millisecond,
		Audit:          &audit,
		OnExpire:       func(ExpireReason) (bool, error) { fired <- struct{}{}; return false, nil },
	})
	waitFired(t, fired, "idle expiry")

	// The record is written after OnExpire returns; poll briefly for it.
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(audit.String(), "decision=expired-idle-noop") {
		if time.Now().After(deadline) {
			t.Fatalf("audit log missing the expired-idle-noop entry; got:\n%s", audit.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if strings.Contains(audit.String(), "expired-idle-withdrawn") {
		t.Error("a no-op trip must not be audited as a withdrawal")
	}
}

// TestHostApprovalAgeExpiryWhileBusy is the age bound's reason for existing: a
// run that never idles still has its approval withdrawn once the confirmation
// is old enough. Traffic flows throughout, so the idle bound can never be what
// fires — and the host keeps serving, exactly like the idle path.
func TestHostApprovalAgeExpiryWhileBusy(t *testing.T) {
	fired := make(chan ExpireReason, 4)
	var audit syncBuffer
	var mu sync.Mutex
	confirmedAt := time.Now()
	approved := true

	h, _ := startHost(t, Config{
		SessionID:      "s",
		EmptyPathScope: "allow",
		IdleTimeout:    time.Hour, // far away: only the age bound can fire
		ApprovalTTL:    60 * time.Millisecond,
		checkInterval:  5 * time.Millisecond,
		Audit:          &audit,
		LastApproval: func() (time.Time, bool) {
			mu.Lock()
			defer mu.Unlock()
			return confirmedAt, approved
		},
		OnExpire: func(r ExpireReason) (bool, error) {
			// The real withdrawal clears the confirmed set; mirror that so the
			// hook stops reporting an approval, as production does.
			mu.Lock()
			approved = false
			mu.Unlock()
			fired <- r
			return true, nil
		},
	})
	c := clientThrough(t, h)

	// Keep the run BUSY the whole time: activity must not defer the age bound.
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			getOK(t, c, "https://api.github.com/repos/o/r")
			time.Sleep(5 * time.Millisecond)
		}
	}()

	var got ExpireReason
	select {
	case got = <-fired:
	case <-time.After(3 * time.Second):
		close(stop)
		<-done
		t.Fatal("the approval-age bound did not fire on a busy run")
	}
	close(stop)
	<-done
	if got != ExpireApprovalAge {
		t.Errorf("expiry reason = %q, want approval-age (the run was never idle)", got)
	}

	// Same in-place semantics as the idle path: reads keep working.
	resp, err := c.Get("https://api.github.com/repos/o/r")
	if err != nil {
		t.Fatalf("read through the proxy failed after the approval-age lock: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got := audit.String(); !strings.Contains(got, "decision=expired-approval-withdrawn") {
		t.Errorf("audit log missing the expired-approval-withdrawn entry; got:\n%s", got)
	}
}

// TestHostApprovalAgeQuietWhenNothingConfirmed: with no confirmed issue there
// is no clock, so the age bound must never fire no matter how long the run
// lives. Without the ok=false guard, a zero ConfirmedAt would read as
// infinitely old and lock a run that never had an approval.
func TestHostApprovalAgeQuietWhenNothingConfirmed(t *testing.T) {
	fired := make(chan ExpireReason, 4)
	startHost(t, Config{
		SessionID:      "s",
		EmptyPathScope: "allow",
		IdleTimeout:    time.Hour,
		ApprovalTTL:    20 * time.Millisecond,
		checkInterval:  5 * time.Millisecond,
		LastApproval:   func() (time.Time, bool) { return time.Time{}, false },
		OnExpire:       func(r ExpireReason) (bool, error) { fired <- r; return true, nil },
	})
	select {
	case r := <-fired:
		t.Fatalf("expiry fired (%q) for a run with nothing confirmed", r)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestHostApprovalAgeResetsOnReconfirmation: a fresh confirmation restarts the
// clock, so the run gets another full TTL rather than being re-locked at once.
//
// OnExpire clears the approval exactly as the real withdrawal does, so what is
// under test is the CLOCK RESET — not the same-timestamp re-arm gate, which is
// covered in TestDueExpiry.
func TestHostApprovalAgeResetsOnReconfirmation(t *testing.T) {
	fired := make(chan ExpireReason, 8)
	var mu sync.Mutex
	confirmedAt := time.Now()
	approved := true

	startHost(t, Config{
		SessionID:      "s",
		EmptyPathScope: "allow",
		IdleTimeout:    time.Hour,
		ApprovalTTL:    60 * time.Millisecond,
		checkInterval:  5 * time.Millisecond,
		LastApproval: func() (time.Time, bool) {
			mu.Lock()
			defer mu.Unlock()
			return confirmedAt, approved
		},
		OnExpire: func(r ExpireReason) (bool, error) {
			mu.Lock()
			approved = false // the withdrawal clears the confirmed set
			mu.Unlock()
			fired <- r
			return true, nil
		},
	})

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("the approval-age bound did not fire")
	}

	// The human re-declares and re-confirms: a NEW confirmation, clock from now.
	mu.Lock()
	confirmedAt = time.Now()
	approved = true
	mu.Unlock()

	// Well inside the new TTL, nothing may fire again...
	select {
	case r := <-fired:
		t.Fatalf("expiry fired (%q) immediately after a fresh confirmation; the clock did not reset", r)
	case <-time.After(30 * time.Millisecond):
	}
	// ...and once the NEW confirmation ages out, it locks again.
	select {
	case r := <-fired:
		if r != ExpireApprovalAge {
			t.Errorf("second expiry reason = %q, want approval-age", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the age bound did not re-fire after the re-confirmation aged out")
	}
}

// TestHostIdleDropsMemoizedWriteToken is the #190 blocker regression, caught by
// the idle_reattest journey: the withdrawal REVOKES this run's write tokens,
// but the proxy memoizes one write token in memory for the whole run. Without
// an explicit drop, the human re-confirms, the agent's next push replays the
// just-revoked token, and GitHub answers 401 ("could not read Username") — a
// working approval that cannot write. The next write after an idle trip must
// therefore mint a FRESH token.
func TestHostIdleDropsMemoizedWriteToken(t *testing.T) {
	fired := make(chan struct{}, 4)
	var mints atomic.Int32
	h, up := startHost(t, Config{
		SessionID:      "s",
		EmptyPathScope: "allow",
		IdleTimeout:    60 * time.Millisecond,
		checkInterval:  5 * time.Millisecond,
		Approve:        func(string) bool { return true },
		MintWrite: func(context.Context) (string, time.Time, error) {
			// Long-lived on purpose: the memo would happily serve this token
			// for the rest of the run, which is the bug.
			return fmt.Sprintf("WRITE-%d", mints.Add(1)), time.Now().Add(time.Hour), nil
		},
		OnExpire: func(ExpireReason) (bool, error) { fired <- struct{}{}; return true, nil },
	})
	c := clientThrough(t, h)
	const pushURL = "https://github.com/o/r.git/info/refs?service=git-receive-pack"

	getStatus(t, c, pushURL)
	first := up.lastAuth()
	if got := mints.Load(); got != 1 {
		t.Fatalf("write mints before the idle trip = %d, want 1", got)
	}
	if first == "" {
		t.Fatal("setup: the first write did not reach upstream with an injected credential")
	}

	waitFired(t, fired, "idle lock")

	// The human re-confirms and the agent pushes again.
	getStatus(t, c, pushURL)
	if got := mints.Load(); got != 2 {
		t.Errorf("write mints after the idle trip = %d, want 2 — the revoked token was served from the memo", got)
	}
	if second := up.lastAuth(); second == first {
		t.Errorf("the write after the idle trip injected the SAME credential as before it (%q); it was revoked and GitHub will 401", second)
	}
}

// TestHostIdleDropsWriteTokenMintedDuringWithdrawal closes the re-entrant half
// of the same bug. The gate does not close until partway through OnExpire, so a
// write that races the withdrawal still mints — re-populating the memo with a
// token the SAME withdrawal then revokes. One drop before OnExpire would leave
// that dead token cached until the next idle trip, so the host drops again
// after. Here OnExpire issues that racing write itself, making the window
// deterministic.
func TestHostIdleDropsWriteTokenMintedDuringWithdrawal(t *testing.T) {
	const pushURL = "https://github.com/o/r.git/info/refs?service=git-receive-pack"
	fired := make(chan struct{}, 4)
	ready := make(chan struct{})
	var mints atomic.Int32
	var c *http.Client

	h, _ := startHost(t, Config{
		SessionID:      "s",
		EmptyPathScope: "allow",
		IdleTimeout:    60 * time.Millisecond,
		checkInterval:  5 * time.Millisecond,
		Approve:        func(string) bool { return true },
		MintWrite: func(context.Context) (string, time.Time, error) {
			return fmt.Sprintf("WRITE-%d", mints.Add(1)), time.Now().Add(time.Hour), nil
		},
		OnExpire: func(ExpireReason) (bool, error) {
			<-ready // c is published; the channel gives the happens-before
			getStatus(t, c, pushURL)
			fired <- struct{}{}
			return true, nil
		},
	})
	c = clientThrough(t, h)
	close(ready)

	getStatus(t, c, pushURL)
	if got := mints.Load(); got != 1 {
		t.Fatalf("mints after the first write = %d, want 1", got)
	}

	waitFired(t, fired, "idle lock")
	if got := mints.Load(); got != 2 {
		t.Fatalf("the write racing the withdrawal did not mint (mints = %d, want 2); the test no longer exercises the window", got)
	}

	// That raced token was revoked by the same withdrawal. The next write must
	// not serve it from the memo.
	getStatus(t, c, pushURL)
	if got := mints.Load(); got != 3 {
		t.Errorf("mints after the post-withdrawal write = %d, want 3 — the memo kept the token minted during the withdrawal, which was revoked", got)
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
		OnExpire:       func(ExpireReason) (bool, error) { fired <- struct{}{}; return true, nil },
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
		OnExpire:       func(ExpireReason) (bool, error) { fired <- struct{}{}; return true, nil },
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
		OnExpire:       func(ExpireReason) (bool, error) { count.Add(1); fired <- struct{}{}; return true, nil },
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
		OnExpire: func(ExpireReason) (bool, error) {
			fired <- struct{}{}
			return false, errors.New("cannot rewrite the approval record")
		},
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
