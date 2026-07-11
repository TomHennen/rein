package proxy

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/TomHennen/rein/internal/brokercore"
)

// SessionConfig carries everything needed to build one session's decision core.
// The daemon's proxy factory (or a test) fills it per attached session, so each
// session gets its OWN read cache, write-token cache, and approval state — a
// shared cache would serve session A's repo-scoped token to session B (design
// #10: with the mint scope fixed to the session's repo set, tokens are
// session-specific and must not cross sessions).
type SessionConfig struct {
	// SessionID is the forensic id used in audit lines.
	SessionID string

	// MintRead / MintWrite mint installation tokens at the session's scope.
	// The proxy wraps them with caching + backoff; the raw funcs are injected
	// (real githubapp mints in the daemon, stubs in tests).
	MintRead  brokercore.MintFunc
	MintWrite brokercore.MintFunc

	// InScope is the session's scope ceiling (session.Session.Contains in the
	// daemon). Nil disables scope enforcement — only for tests.
	InScope func(repo string) bool

	// EmptyPathScope governs a request whose repo couldn't be derived from the
	// path (e.g. api.github.com/graphql, /user). "refuse" or "" / "allow".
	// Default "allow": the minted token is already repo-scoped, so an
	// out-of-scope API call fails server-side anyway.
	EmptyPathScope string

	// Approve is the write-approval hook. Since issue #35 it NEVER
	// prompts: production wires a cheap READ of the run's confirmed-issue
	// set (cmd/rein buildSandboxApprove), consulted on EVERY write-tier
	// request — deliberately un-memoized so a mid-run record invalidation
	// (session edit, issue-transfer invalidation) takes effect on the
	// next request. Nil = auto approve (tests only).
	Approve func(repo string) bool

	// ReadCache is this session's in-memory read-token cache. Injected so the
	// daemon gives each session a fresh one. Nil disables read caching.
	ReadCache brokercore.ReadCache

	// ReadCacheSkew / WriteCacheSkew refresh a cached token this long before
	// expiry so we never hand out a token that expires in flight.
	ReadCacheSkew  time.Duration
	WriteCacheSkew time.Duration

	// MintBackoff is how long to stop attempting write mints after GitHub
	// signals a rate-limit / abuse response, so the proxy doesn't hammer the
	// mint API at proxy request rate. Default 30s.
	MintBackoff time.Duration

	// RecordWrite, if set, receives each freshly-minted write token (issue #20
	// ledger). Best-effort.
	RecordWrite func(token string, expiresAt time.Time)

	Logger *log.Logger
}

// sessionState holds the per-session mutable caching/approval state shared by
// the wrapped mint and approval closures. Guarded by mu — concurrent proxy
// connections on one session race it (and the suite runs -race).
type sessionState struct {
	mu sync.Mutex

	writeToken  string
	writeExpiry time.Time

	backoffUntil time.Time
}

// NewSessionCore builds the brokercore.Core for one session, layering the
// proxy-rate caching the design requires on top of the shared decision core:
//
//   - reads: brokercore caches via cfg.ReadCache (per session). Reads never
//     prompt.
//   - writes: token memoized here — one mint covers a whole run until expiry,
//     so a git push (info/refs + receive-pack) mints once, not twice.
//   - approvals: the hook is consulted on EVERY write-tier request and is
//     deliberately NOT memoized (issue #35): production's Approve is a cheap
//     read of the run's confirmed-issue set, and memoizing the first true
//     would freeze out mid-run invalidations (session edit, transferred
//     issue). One ceremony per scope still holds — the ceremony lives at
//     declare time, not here, so "no re-prompt per write" is unaffected.
//     Safe because brokercore.Serve runs inScope BEFORE confirmWrite, so only
//     requests within the session's declared set (and the empty-path/GraphQL
//     case EmptyPathScope=allow lets through) ever reach confirm, and the
//     minted token already covers the full session set (#10).
//   - backoff: after a GitHub rate-limit/abuse mint failure, write mints are
//     suppressed for MintBackoff so the proxy doesn't hammer the API.
func NewSessionCore(cfg SessionConfig) *brokercore.Core {
	st := &sessionState{}
	backoff := cfg.MintBackoff
	if backoff <= 0 {
		backoff = 30 * time.Second
	}
	skew := cfg.WriteCacheSkew
	if skew <= 0 {
		skew = 30 * time.Second
	}
	readSkew := cfg.ReadCacheSkew
	if readSkew <= 0 {
		readSkew = 30 * time.Second // match direct mode (broker.applyDefaults)
	}

	confirm := func(repo string) bool {
		if cfg.Approve != nil {
			return cfg.Approve(repo)
		}
		return true
	}

	mintWrite := func(ctx context.Context) (string, time.Time, error) {
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.writeToken != "" && time.Until(st.writeExpiry) > skew {
			return st.writeToken, st.writeExpiry, nil
		}
		if now := time.Now(); now.Before(st.backoffUntil) {
			return "", time.Time{}, errMintBackoff
		}
		token, expiresAt, err := cfg.MintWrite(ctx)
		if err != nil {
			if isRateLimited(err) {
				st.backoffUntil = time.Now().Add(backoff)
				if cfg.Logger != nil {
					cfg.Logger.Printf("write mint rate-limited; backing off %s", backoff)
				}
			}
			return "", time.Time{}, err
		}
		st.writeToken = token
		st.writeExpiry = expiresAt
		return token, expiresAt, nil
	}

	// NOTE (issue #67): because mintWrite memoizes, brokercore calls RecordWrite
	// with the SAME token on every write-serving request (it cannot see that the
	// mint was a cache hit), so the ledger gets one entry per write REQUEST, not
	// per minted TOKEN — a 3-push run ledgers one token 6 times. That is
	// deliberately NOT suppressed here. Keeping the append unconditional makes it
	// AT-LEAST-ONCE: AppendWriteToken is best-effort and its error is swallowed
	// (TM-G8 — the token must reach the client regardless), so a later request
	// re-appending is what heals a transient append failure. Suppressing repeats
	// would mean one failed append silently leaves a LIVE token out of the ledger
	// and thus never revoked — the fail-OPEN direction. The duplicates are
	// deduped by token value at the consumer instead (revokeRunWriteTokens), which
	// keys on what the ledger actually contains.
	return &brokercore.Core{
		MintRead:       cfg.MintRead,
		MintWrite:      mintWrite,
		ReadCache:      cfg.ReadCache,
		ReadCacheSkew:  readSkew,
		InScope:        cfg.InScope,
		EmptyPathScope: cfg.EmptyPathScope,
		ConfirmWrite:   confirm,
		RecordWrite:    cfg.RecordWrite,
		Logger:         cfg.Logger,
	}
}

// errMintBackoff is returned by the wrapped write mint while a rate-limit
// backoff window is open. brokercore maps any write-mint error to the
// PlaceholderMintFailed credential, which the proxy turns into a local 502 —
// so a backoff surfaces to the client as "try again", never as a hang.
var errMintBackoff = &backoffError{}

type backoffError struct{}

func (*backoffError) Error() string {
	return "write mint suppressed by rate-limit backoff"
}

// isRateLimited reports whether a mint error looks like a GitHub rate-limit or
// abuse/secondary-limit response. go-githubauth surfaces these as wrapped
// errors carrying the upstream status text, so we match conservatively on the
// well-known phrases. Callers treat a false negative as "retry immediately" —
// acceptable, since the mint just failed anyway.
func isRateLimited(err error) bool {
	if err == nil {
		return false
	}
	// Match on the phrases GitHub uses, NOT bare status numbers: a plain 403
	// (App not installed, permission denied) is a hard failure that should
	// surface immediately, not open a backoff window that 502s every write.
	// "too many requests" is the standard 429 body text.
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "rate limit") ||
		strings.Contains(s, "secondary rate") ||
		strings.Contains(s, "abuse") ||
		strings.Contains(s, "too many requests")
}
