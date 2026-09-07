package runbroker

import (
	"context"
	"time"

	"github.com/TomHennen/rein/internal/proxy"
)

// The two bounds on a live write approval (#190). Neither ends the run: both
// withdraw the APPROVAL in place, leaving the proxy, the read path and the
// expose tunnel serving, so the agent keeps working and re-declares for its
// next write.
//
//   - DefaultIdleTimeout: no proxy traffic at all for this long ⇒ the agent is
//     wedged, waiting on a human, or done. 30m is comfortably above git's own
//     pauses and any realistic think-time between GitHub calls.
//   - DefaultApprovalTTL: the age of the approval itself, measured from the
//     human's MOST RECENT confirmation. Activity does NOT extend it — that is
//     the point: it bounds how long one ceremony can authorize writes, so a
//     busy agent cannot hold a granted approval forever by staying busy. A
//     declare AFTER the withdrawal starts a fresh clock (a new confirmation);
//     re-declaring an already-confirmed issue is a no-op and extends nothing.
const (
	DefaultIdleTimeout = 30 * time.Minute
	DefaultApprovalTTL = 4 * time.Hour
)

// ExpireReason names which bound tripped. Both lead to the same in-place
// withdrawal; the reason only shapes the banner and the audit tag.
type ExpireReason string

const (
	ExpireIdle        ExpireReason = "idle"
	ExpireApprovalAge ExpireReason = "approval-age"
)

// expired is the pure idle decision, split out so the policy is unit-testable
// without any timers. A zero idle disables expiry entirely.
func expired(last, now time.Time, idle time.Duration) bool {
	return idle > 0 && now.Sub(last) >= idle
}

// approvalExpired is the pure approval-age decision: how long since the human
// last confirmed, regardless of what the agent has been doing since. A zero ttl
// disables the bound.
func approvalExpired(confirmedAt, now time.Time, ttl time.Duration) bool {
	return ttl > 0 && !confirmedAt.IsZero() && now.Sub(confirmedAt) >= ttl
}

// deriveCheckInterval picks the expiry poll cadence: a quarter of the tighter
// bound, capped at 30s so the 4h TTL doesn't poll only every hour, floored at
// 1s so a tiny test bound doesn't spin.
func deriveCheckInterval(idle, approvalTTL time.Duration) time.Duration {
	interval := 30 * time.Second
	for _, b := range []time.Duration{idle, approvalTTL} {
		if b > 0 && b/4 < interval {
			interval = b / 4
		}
	}
	if interval < time.Second {
		interval = time.Second
	}
	return interval
}

// monitor polls both bounds until ctx is cancelled. On a trip it fires onExpire
// (the caller's revoke + withdraw-approval + banner) and KEEPS RUNNING: the
// proxy, the read path and the expose tunnel stay up, so the agent's next write
// re-declares while everything else continues.
//
// Re-arm rules, one per bound, both there to stop a wedged run being locked (and
// re-bannered) every poll:
//
//   - idle: a second lock needs ACTIVITY since the last lock of either kind, so
//     "work, then go quiet again" locks again while a run that stays quiet locks
//     once.
//   - approval age: a second lock needs a NEWER confirmation than the one
//     already acted on — i.e. the human re-confirmed, restarting the clock.
//
// Fail closed: if onExpire reports it could NOT withdraw the approval, we fall
// back to the pre-#190 behavior — tear the host down so the next in-sandbox
// request fails closed rather than run on with an approval we cannot revoke.
func (h *Host) monitor(ctx context.Context, idle, approvalTTL, interval time.Duration, now func() time.Time, lastApproval func() (time.Time, bool), onExpire func(ExpireReason) (bool, error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastLock, actedApproval time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n := now()
			reason, actedAt, ok := dueExpiry(dueInput{
				now:           n,
				lastActivity:  time.Unix(0, h.lastActivity.Load()),
				idle:          idle,
				approvalTTL:   approvalTTL,
				lastApproval:  lastApproval,
				lastLock:      lastLock,
				actedApproval: actedApproval,
			})
			if !ok {
				continue
			}
			// Any lock arms BOTH re-arm gates: without touching lastLock, an
			// approval-age trip on a quiet run would be followed immediately by
			// an idle trip, double-bannering one moment.
			lastLock = n
			if reason == ExpireApprovalAge {
				// The timestamp dueExpiry actually judged, NOT a second call to
				// lastApproval: a re-confirmation landing between the two reads
				// would otherwise be recorded as already acted on, and the
				// confirmation the human just gave would never age out.
				actedApproval = actedAt
			}
			// BEFORE the caller revokes: forget the proxy's memoized write
			// token. It is in-memory, so no on-disk withdrawal reaches it — and
			// a re-confirmed agent whose next push replayed the revoked token
			// would get a bare 401 from GitHub instead of a working push (#190).
			h.dropWrite()
			if onExpire == nil {
				h.audit.Record(auditExpired(h.sessionID, expiryDecision(reason, false, nil)))
				continue
			}
			withdrawn, err := onExpire(reason)
			// AGAIN, after: the gate only closed partway through onExpire, so a
			// write that raced the first drop could have minted a fresh token,
			// stored it in the memo, and had it revoked by the same onExpire —
			// leaving the memo holding a dead token, which is the very bug the
			// first drop exists to prevent. Idempotent when nothing is memoized.
			h.dropWrite()
			h.audit.Record(auditExpired(h.sessionID, expiryDecision(reason, withdrawn, err)))
			if err != nil {
				h.logger.Printf("re-attestation (%s) could not withdraw the write approval (%v); stopping the proxy instead", reason, err)
				// Close joins monitorDone — which is THIS goroutine — so mark it
				// done first or the join self-deadlocks.
				h.markMonitorDone()
				_ = h.Close()
				return
			}
		}
	}
}

// dueInput is one poll tick's view of the world.
type dueInput struct {
	now, lastActivity       time.Time
	idle, approvalTTL       time.Duration
	lastApproval            func() (time.Time, bool)
	lastLock, actedApproval time.Time
}

// dueExpiry is the pure "should we withdraw, and why" decision — the whole
// policy, testable without timers or a proxy. It also returns the confirmation
// timestamp it judged, so the caller records exactly what it acted on instead
// of re-reading a clock that may have moved.
//
// Approval age is checked FIRST: it is the bound that does not move, so when
// both trip it is the more informative reason to report.
func dueExpiry(in dueInput) (ExpireReason, time.Time, bool) {
	if in.lastApproval != nil {
		// A run with no confirmed issue has nothing to expire.
		if at, ok := in.lastApproval(); ok && approvalExpired(at, in.now, in.approvalTTL) && at.After(in.actedApproval) {
			return ExpireApprovalAge, at, true
		}
	}
	if expired(in.lastActivity, in.now, in.idle) && (in.lastLock.IsZero() || in.lastActivity.After(in.lastLock)) {
		return ExpireIdle, time.Time{}, true
	}
	return "", time.Time{}, false
}

// dropWrite forgets the proxy's memoized write token, if the host has the seam.
func (h *Host) dropWrite() {
	if h.dropWriteToken != nil {
		h.dropWriteToken()
	}
}

// auditExpired is the audit record for one re-attestation: no host, no upstream
// request, no token — only what the trip actually did.
func auditExpired(sessionID, decision string) proxy.AuditEntry {
	return proxy.AuditEntry{Session: sessionID, Host: "-", Method: "EXPIRY", Decision: decision}
}

// expiryDecision names which bound tripped and what the trip achieved, so the
// audit trail distinguishes a real withdrawal from one that found nothing
// approved and from one that could not withdraw at all.
func expiryDecision(reason ExpireReason, withdrawn bool, err error) string {
	prefix := "expired-idle"
	if reason == ExpireApprovalAge {
		prefix = "expired-approval"
	}
	switch {
	case err != nil:
		return prefix + "-failed"
	case withdrawn:
		return prefix + "-withdrawn"
	default:
		return prefix + "-noop"
	}
}

// markActivity records the current time as the last proxy activity (the idle
// signal). Called inline from the proxy request path via the OnActivity hook, so
// it must stay allocation-free and lock-free — an atomic store.
func (h *Host) markActivity(now time.Time) {
	h.lastActivity.Store(now.UnixNano())
}
