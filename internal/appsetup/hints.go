package appsetup

import "fmt"

// PostManifestInstallHint inspects state.json under configDir for a
// manifest-flow phase that has a primary App registered but no
// installation_id yet. When that pattern matches, returns a
// human-readable hint pointing the user at the install deep-link;
// ok=false when state.json is absent, corrupt, doesn't carry a
// manifest phase (e.g. it's a managed_externally marker), or already
// has installation_id set.
//
// Shared between cmd/rein/doctor.go's "app credentials" check and
// cmd/rein/init.go's BridgeUseState branch so both surfaces give the
// same nudge after a fresh manifest-flow run that hasn't yet been
// followed by `REIN_APP_INSTALLATION_ID=...` (or the Stage 2 install
// polling that obviates it).
//
// configDir is the caller's resolved config.ConfigDir(); the helper
// accepts it as a parameter to keep this package from importing the
// config package (which would risk an import cycle if config ever
// reaches for appsetup state).
func PostManifestInstallHint(configDir string) (string, bool) {
	s, err := ReadState(configDir)
	if err != nil {
		return "", false
	}
	if s.Phase != PhasePrimaryDone && s.Phase != PhaseAuditDone {
		return "", false
	}
	if s.Primary == nil || s.Primary.InstallationID != 0 {
		return "", false
	}
	url := s.Primary.HTMLURL
	if url == "" {
		url = "https://github.com/apps/" + s.Primary.Slug
	}
	return fmt.Sprintf("App registered (%s) but not yet installed on a repo; visit %s/installations/new and then set REIN_APP_INSTALLATION_ID before re-running doctor", s.Primary.Slug, url), true
}
