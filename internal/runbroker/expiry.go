package runbroker

import (
	"context"
	"time"

	"github.com/TomHennen/rein/internal/proxy"
)

// DefaultIdleTimeout is the only expiry bound (#190): after this long with NO
// proxy traffic the run's WRITE APPROVAL is withdrawn in place — the run keeps
// serving, the agent keeps running, and its next write is refused with the
// ordinary declare instruction. 30m is comfortably above git's own pauses and
// any realistic think-time between GitHub calls.
//
// There is deliberately NO hard TTL: a session ends on agent exit or idle
// re-attestation, never on a wall-clock cap that would interrupt live work.
const DefaultIdleTimeout = 30 * time.Minute

// expired is the pure idle decision, split out so the policy is unit-testable
// without any timers. A zero idle disables expiry entirely.
func expired(last, now time.Time, idle time.Duration) bool {
	return idle > 0 && now.Sub(last) >= idle
}

// deriveCheckInterval picks the expiry poll cadence: a quarter of the idle
// bound, capped at 30s so the 30m default doesn't poll only every 7 minutes,
// floored at 1s so a tiny test bound doesn't spin.
func deriveCheckInterval(idle time.Duration) time.Duration {
	interval := 30 * time.Second
	if idle > 0 && idle/4 < interval {
		interval = idle / 4
	}
	if interval < time.Second {
		interval = time.Second
	}
	return interval
}

// monitor polls the idle decision until ctx is cancelled. On a trip it fires
// onExpire (the caller's revoke + withdraw-approval + banner) and KEEPS
// RUNNING: the proxy, the read path and the expose tunnel stay up, so the
// agent's next write re-declares while everything else continues.
//
// Re-arm: a second lock requires ACTIVITY since the last one. Without that
// gate a wedged agent would re-fire (and re-banner) every idle period forever;
// with it, "work, then go quiet again" locks again, and a run that stays quiet
// locks exactly once.
//
// Fail closed: if onExpire reports it could NOT withdraw the approval, we fall
// back to the pre-#190 behavior — tear the host down so the next in-sandbox
// request fails closed rather than run on with an approval we cannot revoke.
func (h *Host) monitor(ctx context.Context, idle, interval time.Duration, now func() time.Time, onExpire func() (bool, error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastLock time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := time.Unix(0, h.lastActivity.Load())
			if !expired(last, now(), idle) {
				continue
			}
			if !lastLock.IsZero() && !last.After(lastLock) {
				continue // already locked and nothing has happened since
			}
			lastLock = now()
			// BEFORE the caller revokes: forget the proxy's memoized write
			// token. It is in-memory, so no on-disk withdrawal reaches it — and
			// a re-confirmed agent whose next push replayed the revoked token
			// would get a bare 401 from GitHub instead of a working push (#190).
			if h.dropWriteToken != nil {
				h.dropWriteToken()
			}
			if onExpire == nil {
				h.audit.Record(auditExpiredIdle(h.sessionID, "expired-idle-noop"))
				continue
			}
			withdrawn, err := onExpire()
			h.audit.Record(auditExpiredIdle(h.sessionID, expiryDecision(withdrawn, err)))
			if err != nil {
				h.logger.Printf("idle re-attestation could not withdraw the write approval (%v); stopping the proxy instead", err)
				// Close joins monitorDone — which is THIS goroutine — so mark it
				// done first or the join self-deadlocks.
				h.markMonitorDone()
				_ = h.Close()
				return
			}
		}
	}
}

// auditExpiredIdle is the audit record for one idle re-attestation: no host,
// no upstream request, no token — only what the trip actually did.
func auditExpiredIdle(sessionID, decision string) proxy.AuditEntry {
	return proxy.AuditEntry{Session: sessionID, Host: "-", Method: "IDLE", Decision: decision}
}

// expiryDecision names what the idle trip achieved, so the audit trail
// distinguishes a real withdrawal from a trip that found nothing approved and
// from one that could not withdraw at all.
func expiryDecision(withdrawn bool, err error) string {
	switch {
	case err != nil:
		return "expired-idle-failed"
	case withdrawn:
		return "expired-idle-withdrawn"
	default:
		return "expired-idle-noop"
	}
}

// markActivity records the current time as the last proxy activity (the idle
// signal). Called inline from the proxy request path via the OnActivity hook, so
// it must stay allocation-free and lock-free — an atomic store.
func (h *Host) markActivity(now time.Time) {
	h.lastActivity.Store(now.UnixNano())
}
