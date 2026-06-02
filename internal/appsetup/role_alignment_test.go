package appsetup_test

import (
	"testing"

	"github.com/TomHennen/rein/internal/appsetup"
	"github.com/TomHennen/rein/internal/config"
)

// TestAppKeystoreRoleMatchesAppsetupPrimary locks in that the keystore
// entry name used by the env-var path (config.AppKeystoreRole) and the
// role used by the manifest flow (appsetup.RolePrimary) refer to the
// same on-disk slot. A rename of one without the other would silently
// break mint paths with a confusing "entry not found" deep in
// MintReadOnlyToken — this test fails loudly at CI time instead.
func TestAppKeystoreRoleMatchesAppsetupPrimary(t *testing.T) {
	if got, want := string(appsetup.RolePrimary), config.AppKeystoreRole; got != want {
		t.Fatalf("string(appsetup.RolePrimary)=%q, config.AppKeystoreRole=%q; the two name the same keystore slot and must stay aligned", got, want)
	}
}
