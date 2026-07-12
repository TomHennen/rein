package appsetup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/keystore"
)

// RunOptions controls a single end-to-end manifest flow execution.
// Fields here mirror the cmd/rein/init.go flags so the CLI dispatch
// is a thin pass-through.
type RunOptions struct {
	ConfigDir     string
	Keystore      keystore.Keystore
	SkipAudit     bool
	Force         bool
	Stdout        io.Writer
	Stderr        io.Writer
	ExpectedOwner string // empty unless --owner passed at the CLI
	// Port pins the loopback callback port (0 = kernel-assigned
	// ephemeral). A fixed port lets a headless/remote user set up
	// `ssh -L <port>:127.0.0.1:<port>` before running init. Both manifest
	// rounds reuse it (each round's listener is closed before the next
	// binds; SO_REUSEADDR handles the immediate rebind).
	Port int
	// MachineLabel is the sanitized human machine label woven into the
	// created App's name (rein-<role>-<label>-<shortrand>). Empty falls
	// back to the label-less name shape. Resolved by the CLI (hostname
	// default or the "Name this machine" prompt) before the flow runs;
	// see onboarding-ux-design.md §4.
	MachineLabel string
	// APIBase overrides the GitHub API base URL for tests. Empty in
	// production (uses DefaultGitHubAPIBase).
	APIBase string
}

// RunManifestFlow executes the two-step manifest flow.
//
// Step 1 (primary App) runs when state.json is absent or
// state.Primary == nil. Step 2 (audit App) runs when state.Audit == nil
// and SkipAudit is false. On step 2's success the state advances to
// PhaseAuditDone.
//
// Idempotency: re-entry with an existing primary record skips step 1.
// On any error after primary registration, state stays at primary_done
// and the caller is told to re-run `rein init` (resume is implicit:
// env-absence + primary_done routes through BridgeResumeManifest).
func RunManifestFlow(ctx context.Context, opts RunOptions) error {
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.Keystore == nil {
		return errors.New("RunManifestFlow: Keystore is required")
	}
	if opts.ConfigDir == "" {
		return errors.New("RunManifestFlow: ConfigDir is required")
	}

	state, stateErr := ReadState(opts.ConfigDir)
	if stateErr != nil && !errors.Is(stateErr, fs.ErrNotExist) && !opts.Force {
		return fmt.Errorf("read state: %w", stateErr)
	}
	if opts.Force {
		// Start fresh on --force; we keep no record of what was there
		// (the user is presumed to have decided), but the existing
		// GitHub Apps are not deleted (manual cleanup at GitHub UI).
		state = State{}
	}

	// Step 1: primary.
	if state.Primary == nil || state.Primary.Slug == "" {
		// Safety guard: refuse to create a new App if a keystore PEM
		// already exists but no state record covers it. Otherwise we
		// orphan the prior App at GitHub (no API to delete) AND
		// overwrite its local PEM. --force skips the guard since the
		// operator explicitly opted into "ignore disk state."
		if !opts.Force {
			if err := assertNoOrphanPEM(opts.Keystore, RolePrimary); err != nil {
				return err
			}
		}
		fmt.Fprintln(opts.Stdout, "[1/2] primary App")
		rec, runErr := runOneStep(ctx, opts, RolePrimary, 1)
		// rec may be non-nil even when runErr != nil: the PEM-write-
		// after-conversion failure case returns the partial record so
		// we can persist state.json. The design's error matrix
		// (init-manifest-design.md §138) assumes a partial state record
		// exists for the user's recovery `rein init` re-run to work
		// — without it, the orphan guard would refuse the next run.
		if rec != nil {
			state.Phase = PhasePrimaryDone
			state.Source = SourceManifest
			state.Primary = rec
			if werr := WriteState(opts.ConfigDir, state); werr != nil {
				if runErr != nil {
					return fmt.Errorf("primary: %w (additionally, write state failed: %v)", runErr, werr)
				}
				return fmt.Errorf("write state after primary: %w", werr)
			}
		}
		if runErr != nil {
			return fmt.Errorf("primary: %w", runErr)
		}
	} else {
		fmt.Fprintf(opts.Stdout, "[1/2] primary App: existing %s (skipping)\n", state.Primary.Slug)
	}

	// Step 2: audit (unless skipped).
	if opts.SkipAudit {
		fmt.Fprintln(opts.Stdout, "[2/2] audit App: skipped (--skip-audit)")
		printPostFlowSummary(opts.Stdout, state)
		return nil
	}
	if state.Audit == nil || state.Audit.Slug == "" {
		if !opts.Force {
			if err := assertNoOrphanPEM(opts.Keystore, RoleAudit); err != nil {
				return err
			}
		}
		fmt.Fprintln(opts.Stdout, "[2/2] audit App")
		rec, runErr := runOneStep(ctx, opts, RoleAudit, 2)
		// As with primary: persist any partial record so the next
		// `rein init` (implicit resume) can adopt the user-recovered
		// PEM. Without this write, the audit orphan guard refuses on
		// re-run.
		if rec != nil {
			state.Phase = PhaseAuditDone
			state.Audit = rec
			if werr := WriteState(opts.ConfigDir, state); werr != nil {
				if runErr != nil {
					return fmt.Errorf("audit: %w (additionally, write state failed: %v)", runErr, werr)
				}
				return fmt.Errorf("write state after audit: %w", werr)
			}
		}
		if runErr != nil {
			// Primary is already saved at primary_done; tell the user
			// to re-run (resume is implicit on env-absence).
			return fmt.Errorf("audit: %w (primary App already created; re-run `rein init` to retry the audit step)", runErr)
		}
	} else {
		fmt.Fprintf(opts.Stdout, "[2/2] audit App: existing %s (skipping)\n", state.Audit.Slug)
		// Defensive: if state.json was hand-edited or a future code path
		// wrote Audit without advancing Phase, reconcile the invariant
		// here. audit_done is the only correct phase when both records
		// are populated.
		if state.Phase != PhaseAuditDone {
			state.Phase = PhaseAuditDone
			if werr := WriteState(opts.ConfigDir, state); werr != nil {
				return fmt.Errorf("write state after phase reconciliation: %w", werr)
			}
		}
	}

	printPostFlowSummary(opts.Stdout, state)
	return nil
}

// NeedsManifestFlow is the single source of truth for "should the
// manifest flow run on this invocation?". DecideBridge delegates to
// this predicate before picking a specific BridgeAction; callers that
// only need the yes/no answer (without picking an action) can use it
// directly.
//
// Returns true in exactly the three states DecideBridge maps to a
// flow-running action:
//
//   - Force: --force was passed (highest precedence, even over a
//     corrupt state.json — Force is "ignore disk, start over").
//   - Absent state + env absent: fresh new-user setup (DecideBridge
//     picks BridgeRunManifest).
//   - Present state + env absent + phase=primary_done: implicit
//     resume after a --skip-audit or interrupted prior run
//     (DecideBridge picks BridgeResumeManifest).
//
// All other combinations (corrupt state, env-present override paths,
// managed_externally, audit_done, unknown phase) do NOT run the flow.
// Resume is implicit (env-absence + primary_done → run), so there is
// no Resume field on RunOptions to consult here.
func NeedsManifestFlow(state State, stateErr error, opts RunOptions, envPresent bool) bool {
	if opts.Force {
		return true
	}
	if stateErr != nil && !errors.Is(stateErr, fs.ErrNotExist) {
		// Corrupt state.json: refuse, don't auto-create.
		return false
	}
	stateAbsent := errors.Is(stateErr, fs.ErrNotExist)
	if stateAbsent {
		// env-present + absent state → write env marker, not the flow.
		return !envPresent
	}
	if envPresent {
		// env-override paths classify match/mismatch; flow doesn't run.
		return false
	}
	// state present, env absent.
	return state.Phase == PhasePrimaryDone
}

// runOneStep handles one App's worth of the manifest flow: bind
// listener, build manifest, render nonce, open browser, wait for
// callback, exchange code, verify owner, persist PEM via keystore,
// return the AppRecord to merge into State.
func runOneStep(ctx context.Context, opts RunOptions, role Role, step int) (*AppRecord, error) {
	ln, port, err := bindLoopback(opts.Port)
	if err != nil {
		return nil, err
	}
	// runCallback owns shutdown; ln.Close() is idempotent.
	defer ln.Close()

	m, err := BuildManifest(role, port, opts.MachineLabel)
	if err != nil {
		return nil, err
	}
	state, err := newStateNonce()
	if err != nil {
		return nil, err
	}

	hi := detectHeadless()
	printBrowserInstructions(opts.Stdout, port, opts.Port != 0, hi)
	if !hi.headless {
		// Only auto-open when a local browser could plausibly reach the
		// loopback; on a headless box the ssh -L hint is the real path.
		if err := openBrowser(localURL(port), opts.Stdout); err != nil {
			// openBrowser is best-effort; an error here is logged but not
			// fatal. The URL is already on stdout.
			fmt.Fprintf(opts.Stderr, "  (browser launch error: %v; please open the URL above manually)\n", err)
		}
	}

	cbCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	res, err := runCallback(cbCtx, ln, m, state, role, step, opts.Stdout)
	if err != nil {
		return nil, err
	}

	base := opts.APIBase
	if base == "" {
		base = DefaultGitHubAPIBase
	}
	cfg, err := ConvertManifestCode(ctx, base, res.Code)
	if err != nil {
		return nil, err
	}

	// Case-insensitive: GitHub treats login lookup case-insensitively
	// but echoes whatever casing the user picked at signup, so
	// --owner=Alice against an "alice" login must match.
	if opts.ExpectedOwner != "" && !strings.EqualFold(cfg.Owner.Login, opts.ExpectedOwner) {
		// Refuse to persist; the user created the App under the wrong
		// account. No GitHub API to delete an App programmatically —
		// surface the manual recovery URL.
		return nil, fmt.Errorf("App created under owner %q but expected %q; delete it at %s/advanced and re-run", cfg.Owner.Login, opts.ExpectedOwner, cfg.HTMLURL)
	}

	// Persist the PEM via keystore. Failure here is the "PEM write
	// fails after successful conversion" case — surface the manual
	// recovery copy from the design's error matrix.
	//
	// Return a partial AppRecord alongside the error so the caller can
	// persist state.json. Without that record, the orphan-PEM guard
	// would refuse the user's recovery `rein init` re-run on the next
	// invocation (the user would have placed the PEM but state.json
	// would have no covering record). See init-manifest-design.md §138
	// — the recovery path the design prescribes only works if the
	// partial state record exists. KeyFingerprint stays empty since
	// Set failed; the user-recovered PEM gets its fingerprint computed
	// on first successful Get/use.
	if err := opts.Keystore.Set(string(role), []byte(cfg.PEM)); err != nil {
		partial := &AppRecord{
			Slug:      cfg.Slug,
			AppID:     cfg.ID,
			ClientID:  cfg.ClientID,
			HTMLURL:   cfg.HTMLURL,
			CreatedAt: time.Now().UTC(),
		}
		return partial, fmt.Errorf("save PEM to keystore: %w\n\nThe App %q was created at GitHub but the local save failed.\nThe partial record has been written to state.json so the orphan-PEM guard\nwill not refuse the recovery. Recover by:\n  1. Visit %s/advanced\n  2. Click \"Generate a private key\"\n  3. Save the downloaded file as %s/%s.pem (mode 0600)\n  4. Re-run `rein init` to continue (or `rein doctor` to verify)",
			err, cfg.Slug, cfg.HTMLURL, opts.ConfigDir, role)
	}

	fp, err := opts.Keystore.Fingerprint(string(role))
	if err != nil {
		// Non-fatal: PEM saved, fingerprint failure means we couldn't
		// derive the human-readable identifier. Log and continue with
		// an empty fingerprint.
		fmt.Fprintf(opts.Stderr, "  WARN: fingerprint(%s): %v\n", role, err)
	}
	fpDisplay := fp
	if fpDisplay == "" {
		fpDisplay = "<unavailable>"
	}

	fmt.Fprintf(opts.Stdout, "  registered: %s (id=%d)\n", cfg.Slug, cfg.ID)
	fmt.Fprintf(opts.Stdout, "  pem saved : keystore[%s] (fingerprint %s)\n", role, fpDisplay)
	fmt.Fprintf(opts.Stdout, "  install at: %s/installations/new\n", cfg.HTMLURL)

	return &AppRecord{
		Slug:           cfg.Slug,
		AppID:          cfg.ID,
		ClientID:       cfg.ClientID,
		KeyFingerprint: fp,
		HTMLURL:        cfg.HTMLURL,
		CreatedAt:      time.Now().UTC(),
	}, nil
}

// assertNoOrphanPEM refuses to create a new App if a keystore entry
// for the role already exists but state.json doesn't cover it. The
// alternative — silently calling Set and creating a duplicate App at
// GitHub — orphans the prior App (no API to delete) and is
// near-impossible to recover from.
//
// The design's PEM-write-failure recovery path used to land here too,
// but RunManifestFlow now persists a partial AppRecord before
// returning the Set error (see runOneStep), so that recovery flow no
// longer hits this guard. What this guard catches in practice is:
//
//   - A PEM placed by the user with no corresponding state.json entry
//     (e.g., copy-pasted from another machine), OR
//   - state.json deleted/corrupted while the PEM survives.
//
// Both require manual reconciliation — either reconstruct state.json
// (Stage 2: `rein import-pem` will automate this) or run --force to
// start fresh (creates a new App at GitHub; the old one stays put and
// must be deleted manually).
func assertNoOrphanPEM(ks keystore.Keystore, role Role) error {
	if _, err := ks.Get(string(role)); err != nil {
		if errors.Is(err, keystore.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("check keystore for %s pem: %w", role, err)
	}
	return fmt.Errorf("refusing to create a new %s App: a private key already exists in keystore[%s] but state.json has no record covering it. "+
		"To avoid orphaning the prior App at GitHub: either reconstruct state.json manually (Stage 2 will provide `rein import-pem --app %s`), "+
		"or run `rein init --force` to start over (the prior App at GitHub will NOT be deleted; remove it from %s/settings/apps).",
		role, role, role, "https://github.com")
}

func printPostFlowSummary(w io.Writer, state State) {
	fmt.Fprintln(w)
	if state.Audit != nil {
		fmt.Fprintln(w, "Both Apps registered. To make them useful, install each on the repos you want rein to broker tokens for:")
	} else {
		fmt.Fprintln(w, "Primary App registered. To make it useful, install it on the repos you want rein to broker tokens for:")
	}
	if state.Primary != nil && state.Primary.HTMLURL != "" {
		fmt.Fprintf(w, "  Primary: %s/installations/new\n", state.Primary.HTMLURL)
	}
	if state.Audit != nil && state.Audit.HTMLURL != "" {
		fmt.Fprintf(w, "  Audit:   %s/installations/new\n", state.Audit.HTMLURL)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Save these slugs to your password manager (they're recoverable from state.json but uniqueness across re-runs is enforced by GitHub):")
	if state.Primary != nil {
		fmt.Fprintf(w, "  primary: %s\n", state.Primary.Slug)
	}
	if state.Audit != nil {
		fmt.Fprintf(w, "  audit:   %s\n", state.Audit.Slug)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Once installed, run `rein doctor` to verify; you may need to set REIN_APP_INSTALLATION_ID until install-polling lands in Stage 2.")
}
