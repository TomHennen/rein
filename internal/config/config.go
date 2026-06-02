// Package config loads rein's Phase 0 configuration from REIN_* environment
// variables. Shared by cmd/rein and cmd/rein-gh so the two binaries see
// the same App and repo configuration.
//
// Phase 0 is single-App, single-repo. CP4+ replaces this with per-session
// config loaded from disk; the env-var path will remain as the developer
// affordance.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/keystore"
)

// AppKeystoreRole is the entry name passed to keystore.Get for the env-var
// PEM. Kept as a constant so mint-site callers and any future audit-App
// callers can refer to the same string without typos.
const AppKeystoreRole = "primary"

// LoadAppConfig reads REIN_APP_CLIENT_ID, REIN_APP_PRIVATE_KEY_PATH,
// REIN_APP_INSTALLATION_ID, and REIN_TEST_REPO_A and returns a validated
// githubapp.Config alongside a Keystore that exposes the env-var PEM
// under AppKeystoreRole. Returns a descriptive error if any of the
// required vars are missing or malformed.
//
// The PEM file is NOT stat'd here — stat-and-read happens on first
// keystore.Get inside a mint. Callers that need an early "file present
// and readable" check should stat REIN_APP_PRIVATE_KEY_PATH themselves
// before threading the keystore into NewClient.
func LoadAppConfig() (githubapp.Config, keystore.Keystore, error) {
	required := []string{
		"REIN_APP_CLIENT_ID",
		"REIN_APP_PRIVATE_KEY_PATH",
		"REIN_APP_INSTALLATION_ID",
		"REIN_TEST_REPO_A",
	}
	for _, k := range required {
		if os.Getenv(k) == "" {
			return githubapp.Config{}, nil, fmt.Errorf("missing env var %s (did you source ./dev-env?)", k)
		}
	}
	installationID, err := strconv.ParseInt(os.Getenv("REIN_APP_INSTALLATION_ID"), 10, 64)
	if err != nil {
		return githubapp.Config{}, nil, fmt.Errorf("REIN_APP_INSTALLATION_ID not an int64: %w", err)
	}
	slug := os.Getenv("REIN_TEST_REPO_A")
	_, repoName, ok := strings.Cut(slug, "/")
	if !ok || repoName == "" {
		return githubapp.Config{}, nil, fmt.Errorf("REIN_TEST_REPO_A %q is not owner/name", slug)
	}
	cfg := githubapp.Config{
		ClientID:       os.Getenv("REIN_APP_CLIENT_ID"),
		InstallationID: installationID,
		RepoName:       repoName,
	}
	ks := keystore.NewSingleFileKeystore(AppKeystoreRole, os.Getenv("REIN_APP_PRIVATE_KEY_PATH"))
	return cfg, ks, nil
}

// StateDir is $XDG_STATE_HOME/rein (defaulting to ~/.local/state/rein).
// Created with mode 0700 on first use.
func StateDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home dir: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "rein")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	return dir, nil
}

// ConfigDir is $XDG_CONFIG_HOME/rein (defaulting to ~/.config/rein).
// Does NOT create the directory — config files are user-edited.
func ConfigDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home dir: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "rein"), nil
}
