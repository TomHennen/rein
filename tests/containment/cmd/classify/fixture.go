package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The fixture $HOME run.sh probes instead of the developer's real one, so the
// report is identical on every machine. Fake contents only.

// HomeSentinels are files no specific policy entry names: only the wholesale
// $HOME deny hides them. Each is probed on its own.
var HomeSentinels = []string{
	".rein-containment-read-sentinel",
	".aws/credentials",
	".bash_history",
	".docker/config.json",
	".kube/config",
	".npmrc",
}

// fixtureFiles gives every denyRead and allowRead entry rein configures under
// $HOME (cmd/rein: credentialDenyReadPaths, sandboxAllowReadPaths), and srt's
// default write dirs, real content to be judged against. emitTargets fails if
// the policy grows an entry this list does not cover.
var fixtureFiles = []string{
	// credential denies
	".ssh/id_ed25519", ".gnupg/secring.gpg", ".netrc", ".git-credentials",
	".config/gh/hosts.yml", ".config/git/credentials",
	".local/share/keyrings/login.keyring", ".local/share/kwalletd/kdewallet.kwl",
	".cargo/credentials", ".cargo/credentials.toml",
	".config/rein-credentials/app.pem", ".config/rein/ca-key.pem", ".local/state/rein/state.json",
	".claude/history.jsonl", ".claude.json",
	// toolchain allow-backs
	".local/bin/tool", ".cargo/bin/cargo", ".rustup/settings.toml",
	"go/pkg/mod/fixture.txt", ".pyenv/version", ".cache/ms-playwright/fixture.txt",
	// srt's default write dirs (#153)
	".claude/debug/fixture.log", ".npm/_logs/fixture.log",
}

func writeFixture(home string) error {
	files := map[string]string{
		// A denied file too, and what rein's git-identity lookup reads.
		".gitconfig": "[user]\n\tname = rein containment fixture\n\temail = fixture@example.invalid\n",
	}
	for _, f := range append(append([]string{}, fixtureFiles...), HomeSentinels...) {
		files[f] = "rein containment fixture: not a credential\n"
	}
	for rel, content := range files {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// requirePlanted fails when a policy entry under the fixture home has nothing
// planted: it could not be judged, and would silently drop out of the report.
func requirePlanted(home string, entries []string) error {
	for _, e := range entries {
		if !strings.HasPrefix(e, home+string(filepath.Separator)) {
			continue
		}
		if _, err := os.Stat(e); err != nil {
			return fmt.Errorf("policy entry %s has no content in the fixture home: add it to fixtureFiles (tests/containment/cmd/classify/fixture.go)", e)
		}
	}
	return nil
}
