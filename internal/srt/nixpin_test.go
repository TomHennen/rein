package srt

import (
	"encoding/json"
	"os"
	"testing"
)

const srtNpmName = "@anthropic-ai/sandbox-runtime"

// The Nix dev shell builds srt from nix/sandbox-runtime; it must be the
// version rein is verified against.
func TestNixSrtMatchesPinnedVersion(t *testing.T) {
	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	readJSON(t, "../../nix/sandbox-runtime/package.json", &manifest)
	if got := manifest.Dependencies[srtNpmName]; got != PinnedVersion {
		t.Errorf("package.json pins srt %q, PinnedVersion is %q", got, PinnedVersion)
	}

	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	readJSON(t, "../../nix/sandbox-runtime/package-lock.json", &lock)
	if got := lock.Packages["node_modules/"+srtNpmName].Version; got != PinnedVersion {
		t.Errorf("package-lock.json resolves srt %q, PinnedVersion is %q; regenerate the lock", got, PinnedVersion)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
