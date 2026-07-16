package appsetup

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDecideBridge_Table(t *testing.T) {
	const (
		envCID = "Iv23liabc"
		envIID = int64(99887766)
	)
	stateAbsent := fs.ErrNotExist
	statePrimaryDone := State{
		Phase:   PhasePrimaryDone,
		Primary: &AppRecord{ClientID: envCID, InstallationID: envIID},
	}
	stateAuditDone := State{
		Phase:   PhaseAuditDone,
		Primary: &AppRecord{ClientID: envCID, InstallationID: envIID},
		Audit:   &AppRecord{ClientID: "audit-cid"},
	}
	stateAuditDoneOther := State{
		Phase:   PhaseAuditDone,
		Primary: &AppRecord{ClientID: "OTHER-CID", InstallationID: 1},
		Audit:   &AppRecord{ClientID: "audit-cid"},
	}
	stateManaged := State{
		Phase:   PhaseManagedExternally,
		Source:  SourceEnv,
		Primary: &AppRecord{ClientID: envCID, InstallationID: envIID},
	}

	// resume is intentionally absent here: the --resume flag was
	// removed because DecideBridge / NeedsManifestFlow never consulted
	// it (resume is implicit on env-absence + primary_done).
	cases := []struct {
		name        string
		state       State
		stateErr    error
		envPresent  bool
		envCID      string
		envIID      int64
		force       bool
		wantAction  BridgeAction
		wantContain string
	}{
		// Design row 1: absent state + env present → write marker.
		{
			name:        "row1_state_absent_env_present",
			stateErr:    stateAbsent,
			envPresent:  true,
			envCID:      envCID,
			envIID:      envIID,
			wantAction:  BridgeWriteEnvMarker,
			wantContain: "env-managed",
		},
		// Design row 2: absent state + env absent → run manifest.
		{
			name:        "row2_state_absent_env_absent",
			stateErr:    stateAbsent,
			wantAction:  BridgeRunManifest,
			wantContain: "fresh new-user",
		},
		// Design row 3: state primary_done + env mismatch → override warn.
		{
			name:        "row3_state_primary_done_env_mismatch",
			state:       statePrimaryDone,
			envPresent:  true,
			envCID:      "DIFFERENT",
			envIID:      envIID,
			wantAction:  BridgeEnvOverrideMismatch,
			wantContain: "does not match state.json",
		},
		// Design row 4: state audit_done + env absent → use state.
		{
			name:        "row4_state_audit_done_env_absent",
			state:       stateAuditDone,
			wantAction:  BridgeUseState,
			wantContain: "audit_done",
		},
		// Design row 5: state managed_externally + env absent → refuse.
		{
			name:        "row5_managed_externally_env_absent",
			state:       stateManaged,
			wantAction:  BridgeManagedExternallyMissingEnv,
			wantContain: "export them again",
		},
		// Design row 6: state present + env match → quiet steady state.
		{
			name:        "row6_state_audit_done_env_match",
			state:       stateAuditDone,
			envPresent:  true,
			envCID:      envCID,
			envIID:      envIID,
			wantAction:  BridgeEnvOverrideMatch,
			wantContain: "matches",
		},
		// Row 3 also fires when env-mismatch happens with audit_done.
		{
			name:        "row3_audit_done_mismatch",
			state:       stateAuditDoneOther,
			envPresent:  true,
			envCID:      envCID,
			envIID:      envIID,
			wantAction:  BridgeEnvOverrideMismatch,
			wantContain: "does not match state.json",
		},
		// Force short-circuits everything.
		{
			name:        "force_overrides",
			state:       stateAuditDone,
			envPresent:  true,
			envCID:      envCID,
			envIID:      envIID,
			force:       true,
			wantAction:  BridgeForce,
			wantContain: "manifest flow from scratch",
		},
		// Corrupt state.json is not row-2 (must fail closed, not auto-create).
		{
			name:        "corrupt_state",
			stateErr:    errors.New("parse error: invalid json"),
			wantAction:  BridgeStateCorrupt,
			wantContain: "unreadable",
		},
		// managed_externally + env match → row 6 behavior (quiet).
		{
			name:        "managed_externally_env_match",
			state:       stateManaged,
			envPresent:  true,
			envCID:      envCID,
			envIID:      envIID,
			wantAction:  BridgeEnvOverrideMatch,
			wantContain: "matches",
		},
		// managed_externally + env mismatch → row 3 behavior (warn).
		{
			name:        "managed_externally_env_mismatch",
			state:       stateManaged,
			envPresent:  true,
			envCID:      "OTHER",
			envIID:      envIID,
			wantAction:  BridgeEnvOverrideMismatch,
			wantContain: "does not match",
		},
		// primary_done + env absent → resume manifest (implicit resume).
		{
			name:        "primary_done_env_absent_resumes",
			state:       statePrimaryDone,
			wantAction:  BridgeResumeManifest,
			wantContain: "primary_done",
		},
		// Unknown phase + env absent → use state as-is (operator can
		// override with --force). Locks in the bridge.go fallback branch.
		{
			name:        "unknown_phase_env_absent",
			state:       State{Phase: "someday_new_phase"},
			wantAction:  BridgeUseState,
			wantContain: "someday_new_phase",
		},
	}

	// flowActions is the set of BridgeActions that mean "manifest flow
	// will run on this invocation". Used to assert NeedsManifestFlow's
	// bool matches DecideBridge's action on every row — locking the
	// "one source of truth for the flow-runs predicate" property.
	flowActions := map[BridgeAction]bool{
		BridgeRunManifest:    true,
		BridgeForce:          true,
		BridgeResumeManifest: true,
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, msg := DecideBridge(tc.state, tc.stateErr, tc.envPresent, tc.envCID, tc.envIID, tc.force)
			if got != tc.wantAction {
				t.Errorf("action = %v, want %v (msg=%q)", got, tc.wantAction, msg)
			}
			if !strings.Contains(msg, tc.wantContain) {
				t.Errorf("msg %q should contain %q", msg, tc.wantContain)
			}
			// NeedsManifestFlow must agree with the action's flow-vs-not
			// classification — otherwise the two functions have drifted.
			needs := NeedsManifestFlow(tc.state, tc.stateErr, RunOptions{Force: tc.force}, tc.envPresent)
			wantNeeds := flowActions[tc.wantAction]
			if needs != wantNeeds {
				t.Errorf("NeedsManifestFlow = %v, want %v (action=%v)", needs, wantNeeds, tc.wantAction)
			}
		})
	}
}

func TestWriteEnvMarker(t *testing.T) {
	dir := t.TempDir()
	if err := WriteEnvMarker(dir, "Iv23liabc", 99887766); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := ReadState(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if s.Phase != PhaseManagedExternally {
		t.Errorf("phase = %q", s.Phase)
	}
	if s.Source != SourceEnv {
		t.Errorf("source = %q, want %q", s.Source, SourceEnv)
	}
	if s.Primary == nil || s.Primary.ClientID != "Iv23liabc" || s.Primary.InstallationID != 99887766 {
		t.Errorf("primary = %+v", s.Primary)
	}
	if s.Primary != nil && (s.Primary.Slug != "" || s.Primary.KeyFingerprint != "") {
		t.Errorf("marker must not carry slug or fingerprint (got slug=%q fp=%q)", s.Primary.Slug, s.Primary.KeyFingerprint)
	}
	// CreatedAt should be roughly now.
	if s.Primary != nil && time.Since(s.Primary.CreatedAt) > time.Minute {
		t.Errorf("CreatedAt too old: %v", s.Primary.CreatedAt)
	}

	// Verify mode on disk.
	info, err := os.Stat(StatePath(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %#o", mode)
	}
}
