package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/TomHennen/rein/internal/approvals"
	"github.com/TomHennen/rein/internal/runbroker"
	"github.com/TomHennen/rein/internal/session"
)

// The two re-attestation bounds are overridable for tests and demos (#190).
// Both are read ONLY from the human's launch environment — srt's env allowlist
// (PATH/HOME/LANG/TERM/LC_*) never carries them into the sandbox, so the agent
// can neither read nor set them. The journey suite sets them to a few seconds
// so a lock is observable inside a test.
const (
	envIdleTimeout = "REIN_IDLE_TIMEOUT"
	envApprovalTTL = "REIN_APPROVAL_TTL"
)

// minBound floors both overrides. Anything shorter would re-lock faster than a
// single git fetch + declare round trip, which is a footgun, not a test knob.
const minBound = 10 * time.Second

// resolveIdleTimeout / resolveApprovalTTL return the run's bound and whether it
// was overridden.
func resolveIdleTimeout(raw string) (time.Duration, bool, error) {
	return resolveBound(envIdleTimeout, raw, runbroker.DefaultIdleTimeout)
}

func resolveApprovalTTL(raw string) (time.Duration, bool, error) {
	return resolveBound(envApprovalTTL, raw, runbroker.DefaultApprovalTTL)
}

// resolveBound parses one override. An unparseable or too-small value is a hard
// error: fail closed on a misspelled knob rather than silently running with the
// default the operator thought they had replaced.
func resolveBound(name, raw string, def time.Duration) (time.Duration, bool, error) {
	if raw == "" {
		return def, false, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false, fmt.Errorf("%s=%q is not a Go duration (e.g. 30m, 45s): %w", name, raw, err)
	}
	if d < minBound {
		return 0, false, fmt.Errorf("%s=%q is below the %s minimum", name, raw, minBound)
	}
	return d, true, nil
}

// lastApprovalHook builds runbroker.Config.LastApproval for a run: when the
// human most recently confirmed an issue, and whether anything is confirmed at
// all. It reads the SIGNATURE-VALIDATED set — the same read the write gate
// makes — so a record the gate already treats as invalid is not something the
// approval-age bound needs to expire.
func lastApprovalHook(sess session.Session, stateDir, runID string) func() (time.Time, bool) {
	sig := approvals.SignatureOf(sess)
	return func() (time.Time, bool) {
		var newest time.Time
		for _, ci := range approvals.ConfirmedIssues(stateDir, runID, sig) {
			if ci.ConfirmedAt.After(newest) {
				newest = ci.ConfirmedAt
			}
		}
		return newest, !newest.IsZero()
	}
}

// reattestBounds is the launch banner's view of the two re-attestation bounds:
// the resolved values plus whether each came from an override, so the banner
// can name the knob only when the operator actually used it. Grouped because
// printSandboxBanner's positional arg list is already long.
type reattestBounds struct {
	idle, ttl       time.Duration
	idleSet, ttlSet bool
}

// reattestDeps is what one re-attestation needs.
type reattestDeps struct {
	stateDir string
	runID    string
	// idle and approvalTTL are the resolved bounds, quoted in whichever banner
	// the trip's reason selects.
	idle        time.Duration
	approvalTTL time.Duration
	// drainTokens snapshots this run's write-token ledger, removes it, and
	// revokes what it held. Returns the ledger-removal error only (the revokes
	// themselves are best-effort and report on stderr).
	drainTokens func() error
	out         io.Writer
	logger      *log.Logger
}

// reattest withdraws the run's WRITE APPROVAL when either bound trips (#190)
// without ending the run: the proxy, the read path and the expose tunnel keep
// serving, and the agent's next write hits the ordinary "declare your issue"
// refusal, which the human confirms through the full Form A ceremony again.
//
// Ordering is deliberate: withdraw the approval FIRST — the confirmed set and
// the stale pending declaration, so neither the proxy gate nor an
// out-of-process grant can re-open writes — then drain the ledger (which
// removes it before revoking, so a token minted concurrently re-creates the
// file and survives to the exit-time revoke instead of being deleted
// unrevoked). clearErr is reported LAST so the tokens are revoked even on the
// failure path.
//
// It reports whether it actually withdrew anything. A non-nil error means the
// approval could not be withdrawn; the caller (runbroker) then stops the proxy,
// which is the pre-#190 behavior and the fail-closed answer.
//
// The two bounds differ only in what tripped them and which banner says so, so
// there is exactly one withdrawal, not one per bound.
func reattest(d reattestDeps, reason runbroker.ExpireReason) (bool, error) {
	if !hasWriteCapability(d.stateDir, d.runID) {
		d.logger.Printf("re-attestation (%s): nothing approved and no write tokens to revoke; run continues untouched", reason)
		return false, nil
	}
	clearErr := approvals.ClearConfirmedIssues(d.stateDir, d.runID)
	// The stale pending declaration is part of the approval surface: leaving it
	// would let an out-of-process grant re-open writes without a fresh declare.
	if err := approvals.ClearPendingDeclaration(d.stateDir, d.runID); err != nil && clearErr == nil {
		clearErr = err
	}
	// A drain failure means live write tokens we could not revoke, which is the
	// one thing this must never leave behind — fail closed with the rest.
	if err := d.drainTokens(); err != nil && clearErr == nil {
		clearErr = err
	}
	if clearErr != nil {
		d.logger.Printf("re-attestation (%s): could not withdraw the write approval: %v", reason, clearErr)
		// This branch ends the run's credential path rather than re-attesting
		// it, so tear ALL the per-run state down right now — the pre-#190
		// behavior. Without it the standing approval would sit on disk until
		// the deferred exit-time ClearRun, which a SIGKILL skips entirely,
		// leaving it to the next launch's Sweep.
		if err := approvals.ClearRun(d.stateDir, d.runID); err != nil {
			d.logger.Printf("re-attestation: could not clear the run's state either (%v); it survives until the next launch sweep", err)
		}
		printExpiryHardStopBanner(d.out, clearErr)
		return false, fmt.Errorf("withdraw write approval for run %s: %w", d.runID, clearErr)
	}
	printReattestBanner(d.out, reason, d.idle, d.approvalTTL)
	return true, nil
}

// hasWriteCapability reports whether this run currently holds anything worth
// withdrawing: a confirmed issue, or a write token in the ledger. When it holds
// neither, the re-attestation is a no-op and must stay SILENT — otherwise a
// read-only agent that works and then idles would be banner-spammed about an
// approval it never had.
//
// Only "the file is not there" answers NO. Any other read error (a torn write,
// EACCES, a corrupt record) answers YES: the withdrawal is idempotent, so
// running it needlessly costs a banner, while skipping it on a transient error
// would leave a live approval standing — the fail-OPEN direction.
func hasWriteCapability(stateDir, runID string) bool {
	switch rec, err := approvals.ReadApproval(stateDir, runID); {
	case err == nil:
		if len(rec.Issues) > 0 {
			return true
		}
	case !errors.Is(err, os.ErrNotExist):
		return true
	}
	_, err := os.Stat(approvals.WriteTokenLedgerPath(stateDir, runID))
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

// printReattestBanner is the human-facing notice for a write lock. The agent is
// NOT killed and the proxy is NOT stopped — only the write approval is
// withdrawn, so the message must say what still works. The two bounds get
// different first lines because the remedy differs in the operator's head: idle
// means "nothing was happening", approval age means "this confirmation is old".
func printReattestBanner(w io.Writer, reason runbroker.ExpireReason, idle, approvalTTL time.Duration) {
	fmt.Fprintln(w, "\n===============================================================")
	if reason == runbroker.ExpireApprovalAge {
		fmt.Fprintf(w, "rein: write approval is %s old — writes LOCKED.\n", approvalTTL)
	} else {
		fmt.Fprintf(w, "rein: idle for %s — writes LOCKED.\n", idle)
	}
	fmt.Fprintln(w, "  The agent keeps running; its next GitHub write will ask it to")
	fmt.Fprintln(w, "  declare its issue again, and you confirm once more.")
	fmt.Fprintln(w, "  Reads still work.")
	fmt.Fprintln(w, "===============================================================")
}

// printExpiryHardStopBanner is the fail-closed fallback: rein could not
// withdraw the write approval, so it stops the credential proxy instead. That
// is the pre-#190 behavior, and it must be as loud as the re-attest banner —
// the human was just promised reads keep working, and here they do not.
func printExpiryHardStopBanner(w io.Writer, cause error) {
	fmt.Fprintln(w, "\n===============================================================")
	fmt.Fprintf(w, "rein: SESSION STOPPED — could not withdraw this run's write approval (%v).\n", cause)
	fmt.Fprintln(w, "  Revoked this run's write tokens and STOPPED the credential proxy.")
	fmt.Fprintln(w, "  The agent is still running but can no longer reach GitHub — its")
	fmt.Fprintln(w, "  git/gh requests will now fail. Exit it and re-run `rein run` to")
	fmt.Fprintln(w, "  continue with a fresh, re-authorized session.")
	fmt.Fprintln(w, "===============================================================")
}
