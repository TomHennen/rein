package appsetup

import (
	"errors"
	"fmt"
	"io/fs"
	"time"
)

// BridgeAction is the env-var-bridge decision for the current
// invocation. The CLI dispatch is a single switch over this.
type BridgeAction int

const (
	// BridgeRunManifest: nothing on disk and no env — fresh new-user
	// path; run the manifest flow. (Design row 2.)
	BridgeRunManifest BridgeAction = iota

	// BridgeWriteEnvMarker: env vars set & validate, no state.json —
	// write the marker file (phase=managed_externally, source=env)
	// and proceed using env config. (Design row 1.)
	BridgeWriteEnvMarker

	// BridgeUseState: state.json present, env absent, phase is a
	// manifest-flow phase — steady state, use state.json. (Design row 4.)
	BridgeUseState

	// BridgeEnvOverrideMatch: env + state, values match — quiet
	// steady state. (Design row 6.)
	BridgeEnvOverrideMatch

	// BridgeEnvOverrideMismatch: env + state, values differ — env
	// wins for this invocation; print one-line WARN. (Design row 3.)
	BridgeEnvOverrideMismatch

	// BridgeManagedExternallyMissingEnv: marker file says env-managed
	// but env vars are absent. Refuse to proceed; exit non-zero.
	// (Design row 5.)
	BridgeManagedExternallyMissingEnv

	// BridgeForce: --force was passed; ignore state and run the
	// manifest flow. (Wraps "Force overrides all.")
	BridgeForce

	// BridgeStateCorrupt: state.json is present but unparseable.
	// Refuse to proceed; the operator is the only one allowed to
	// touch state.json. (Outside the 6-row table, but a real failure
	// mode the bridge has to handle deterministically.)
	BridgeStateCorrupt

	// BridgeResumeManifest: state.json present, phase < audit_done,
	// no env, no force. Resume is implicit on env-absence — there is
	// no explicit --resume flag. Run the manifest flow from the next
	// step.
	BridgeResumeManifest
)

// DecideBridge implements the design's env-bridge state-transition
// rules plus the corrupt-state and resume cases. Pure: no disk writes,
// no network. All side effects are triggered by the caller switching
// on the returned action.
//
// stateErr semantics: fs.ErrNotExist means "fresh install"; any other
// non-nil err means "state present but unreadable/unparseable" — the
// bridge surfaces that as BridgeStateCorrupt rather than treating it
// as absent (which would create duplicate Apps).
//
// envPresent should be true only when all required REIN_APP_* env
// vars are set AND valid. Partially-set or malformed env fails closed
// to "absent" — the caller decides whether to surface a clearer error.
//
// The "should the manifest flow run?" decision is delegated to
// NeedsManifestFlow so the predicate has exactly one definition.
// DecideBridge picks the specific BridgeAction once that question is
// answered.
//
// Note: there is no --resume parameter. Resume is implicit — when
// state.json says primary_done and env vars are absent, the bridge
// picks BridgeResumeManifest unconditionally. A separate --resume
// flag was tried in early Phase 0.5 and removed when it became clear
// the predicate ignored it on every path.
func DecideBridge(state State, stateErr error, envPresent bool, envClientID string, envInstallationID int64, force bool) (BridgeAction, string) {
	// Delegate to NeedsManifestFlow first so the two functions cannot
	// drift. The opts struct exists for the predicate's signature; the
	// fields it consumes (Force, plus the args here) are the only
	// inputs that matter for the yes/no answer.
	runFlow := NeedsManifestFlow(state, stateErr, RunOptions{Force: force}, envPresent)
	if runFlow {
		if force {
			return BridgeForce, "--force: ignoring state.json, running manifest flow from scratch (existing Apps at GitHub are NOT deleted)"
		}
		if errors.Is(stateErr, fs.ErrNotExist) {
			return BridgeRunManifest, "no state.json, no env — fresh new-user setup; running GitHub App manifest flow"
		}
		// state present, env absent, phase=primary_done: implicit resume.
		// Phase=primary_done with env absent always means continue.
		return BridgeResumeManifest, "state.json: primary_done; continuing manifest flow (creating audit App)"
	}

	// Flow does not run. Pick among the non-flow actions.
	if stateErr != nil && !errors.Is(stateErr, fs.ErrNotExist) {
		return BridgeStateCorrupt, fmt.Sprintf("state.json present but unreadable (%v); refusing to proceed. Inspect or remove the file manually, or re-run with --force.", stateErr)
	}
	stateAbsent := errors.Is(stateErr, fs.ErrNotExist)

	if stateAbsent {
		// runFlow=false + absent → env must be present → write marker.
		return BridgeWriteEnvMarker, "no state.json; REIN_APP_* env vars detected — using env-managed path (marker state.json will be written)"
	}

	// state present.
	if state.Phase == PhaseManagedExternally {
		if !envPresent {
			return BridgeManagedExternallyMissingEnv, "state.json says env-managed but REIN_APP_* are not set; export them again OR run `rein init --force` to switch to the manifest-flow path"
		}
		// env present + managed_externally marker → match or mismatch.
		return classifyEnvMatch(state, envClientID, envInstallationID)
	}

	// state present, phase is primary_done / audit_done / unknown.
	if envPresent {
		return classifyEnvMatch(state, envClientID, envInstallationID)
	}

	// state present, env absent.
	if state.Phase == PhaseAuditDone {
		return BridgeUseState, "state.json: audit_done (steady state from manifest flow)"
	}
	// Unknown phase: treat conservatively as use-state (operator will
	// see the row and can override with --force).
	return BridgeUseState, fmt.Sprintf("state.json present (phase=%q); using as-is", state.Phase)
}

// classifyEnvMatch compares the env vars against state.json's primary
// record. Match → quiet steady state. Mismatch → env wins + warn.
func classifyEnvMatch(state State, envClientID string, envInstallationID int64) (BridgeAction, string) {
	if state.Primary == nil {
		// State has no primary record to compare to. Treat as
		// mismatch so the operator sees the divergence.
		return BridgeEnvOverrideMismatch, "REIN_APP_* set but state.json has no primary record; env is taking precedence. Run `rein init --force` to reconcile."
	}
	if state.Primary.ClientID == envClientID && state.Primary.InstallationID == envInstallationID {
		return BridgeEnvOverrideMatch, "REIN_APP_* matches state.json"
	}
	return BridgeEnvOverrideMismatch, fmt.Sprintf("REIN_APP_CLIENT_ID/INSTALLATION_ID (%s/%d) does not match state.json (%s/%d); env is taking precedence. Run `rein init --force` to reconcile.", envClientID, envInstallationID, state.Primary.ClientID, state.Primary.InstallationID)
}

// WriteEnvMarker creates the marker state.json for the design row 1
// (state absent, env set & validates). phase = PhaseManagedExternally,
// source = SourceEnv. Primary records ClientID + InstallationID only —
// no PEM path, no fingerprint (the env points to a user-managed PEM
// and fingerprinting it would imply ownership rein doesn't claim).
func WriteEnvMarker(configDir, clientID string, installationID int64) error {
	s := State{
		Phase:  PhaseManagedExternally,
		Source: SourceEnv,
		Primary: &AppRecord{
			ClientID:       clientID,
			InstallationID: installationID,
			CreatedAt:      time.Now().UTC(),
		},
	}
	return WriteState(configDir, s)
}
