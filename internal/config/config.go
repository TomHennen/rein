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
)

// LoadAppConfig reads REIN_APP_CLIENT_ID, REIN_APP_PRIVATE_KEY_PATH,
// REIN_APP_INSTALLATION_ID, and REIN_TEST_REPO_A and returns a fully
// validated githubapp.Config. Returns a descriptive error if any of the
// required vars are missing or malformed.
func LoadAppConfig() (githubapp.Config, error) {
	required := []string{
		"REIN_APP_CLIENT_ID",
		"REIN_APP_PRIVATE_KEY_PATH",
		"REIN_APP_INSTALLATION_ID",
		"REIN_TEST_REPO_A",
	}
	for _, k := range required {
		if os.Getenv(k) == "" {
			return githubapp.Config{}, fmt.Errorf("missing env var %s (did you source ./dev-env?)", k)
		}
	}
	installationID, err := strconv.ParseInt(os.Getenv("REIN_APP_INSTALLATION_ID"), 10, 64)
	if err != nil {
		return githubapp.Config{}, fmt.Errorf("REIN_APP_INSTALLATION_ID not an int64: %w", err)
	}
	slug := os.Getenv("REIN_TEST_REPO_A")
	_, repoName, ok := strings.Cut(slug, "/")
	if !ok || repoName == "" {
		return githubapp.Config{}, fmt.Errorf("REIN_TEST_REPO_A %q is not owner/name", slug)
	}
	return githubapp.Config{
		ClientID:       os.Getenv("REIN_APP_CLIENT_ID"),
		PrivateKeyPath: os.Getenv("REIN_APP_PRIVATE_KEY_PATH"),
		InstallationID: installationID,
		RepoName:       repoName,
	}, nil
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
