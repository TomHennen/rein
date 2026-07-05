package runbroker

import (
	"context"
	"time"
)

// expiry constants: the proactive bounds on a granted approval + cached write
// token (design §5.3's run-lifetime capability). Agent-process-exit teardown
// (the deferred revoke in cmd/rein) already covers the normal case; these bound
// the pathological ones — an agent that idles forever, or one that runs forever.
//
//   - Idle 30m: no proxy traffic at all for half an hour ⇒ the agent is wedged,
//     waiting on a human, or done. Revoke + stop rather than leave an approved
//     write path live indefinitely. Comfortably above git's own pauses and any
//     realistic think-time between GitHub calls.
//   - Hard TTL 4h: the absolute wall-clock cap, deliberately equal to
//     cmd/rein's approvalTTL (the orphan-sweep backstop). Write tokens re-mint
//     on GitHub's ~1h native TTL, so the hard cap is what actually bounds
//     SUSTAINED approved-write capability for a long-running agent. A dogfood
//     coding session fits well inside 4h; a run that exceeds it should
//     re-authorize.
const (
	DefaultIdleTimeout = 30 * time.Minute
	DefaultHardTTL     = 4 * time.Hour
)

// expired is the pure expiry decision, split out so the policy is unit-testable
// without any timers. hard is checked before idle so a run that is both idle AND
// past its hard cap reports "hard-ttl" (the stronger reason). A zero idle or
// hard disables that bound. Returns ("", false) when the run may continue.
func expired(last, start, now time.Time, idle, hard time.Duration) (reason string, isExpired bool) {
	if hard > 0 && now.Sub(start) >= hard {
		return "hard-ttl", true
	}
	if idle > 0 && now.Sub(last) >= idle {
		return "idle", true
	}
	return "", false
}

// deriveCheckInterval picks the expiry poll cadence from the configured bounds:
// frequent enough to detect expiry promptly (a quarter of the tighter bound),
// capped at 30s so a 4h TTL doesn't poll only every hour, floored at 1s so a
// tiny test bound doesn't spin. Only reached when at least one bound is set.
func deriveCheckInterval(idle, hard time.Duration) time.Duration {
	interval := 30 * time.Second
	for _, b := range []time.Duration{idle, hard} {
		if b > 0 && b/4 < interval {
			interval = b / 4
		}
	}
	if interval < time.Second {
		interval = time.Second
	}
	return interval
}

// monitor polls the expiry decision until ctx is cancelled or a bound trips. On
// expiry it fires OnExpire (the caller's revoke + loud message) and then tears
// the host down so the next in-sandbox GitHub request fails closed. It runs at
// most once per host (expireOnce), and Close/OnExpire double-firing with the
// deferred exit-time revoke is harmless (revoke is idempotent/best-effort).
func (h *Host) monitor(ctx context.Context, idle, hard, interval time.Duration, now func() time.Time, onExpire func(reason string)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := time.Unix(0, h.lastActivity.Load())
			if reason, isExp := expired(last, h.start, now(), idle, hard); isExp {
				h.expire(reason, onExpire)
				return
			}
		}
	}
}

// expire fires the caller's OnExpire (BEFORE teardown, so tokens are revoked
// while the proxy still holds them) then Closes the host, stopping the proxy.
// Guarded so it happens exactly once even if a later manual Close races it.
//
// It runs on the monitor goroutine, so it marks the monitor done BEFORE calling
// Close — otherwise Close's <-monitorDone join (which waits for THIS goroutine)
// would deadlock. The monitor's own deferred markMonitorDone is then a no-op.
func (h *Host) expire(reason string, onExpire func(reason string)) {
	h.expireOnce.Do(func() {
		if onExpire != nil {
			onExpire(reason)
		}
		h.markMonitorDone()
		_ = h.Close()
	})
}

// markActivity records the current time as the last proxy activity (the idle
// signal). Called inline from the proxy request path via the OnActivity hook, so
// it must stay allocation-free and lock-free — an atomic store.
func (h *Host) markActivity(now time.Time) {
	h.lastActivity.Store(now.UnixNano())
}
