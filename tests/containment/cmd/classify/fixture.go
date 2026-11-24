package main

import (
	"os"
	"path/filepath"
)

// fixtureFiles is the fixture $HOME run.sh probes instead of the developer's
// real one, so the report is identical on every machine. Fake contents only.
// Every denyRead and allowRead entry rein configures under $HOME (cmd/rein:
// credentialDenyReadPaths, sandboxAllowReadPaths) needs content here;
// requirePlanted fails the run if the policy grows an entry this list misses.
var fixtureFiles = []string{
	// hidden only by the wholesale $HOME deny
	".rein-containment-read-sentinel", ".aws/credentials", ".bash_history",
	".docker/config.json", ".kube/config", ".npmrc",
	// credential denies
	".ssh/id_ed25519", ".gnupg/secring.gpg", ".netrc", ".git-credentials", ".gitconfig",
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
	for _, rel := range fixtureFiles {
		content := "rein containment fixture: not a credential\n"
		if rel == ".gitconfig" { // rein's git-identity lookup parses it
			content = "[user]\n\tname = rein containment fixture\n\temail = fixture@example.invalid\n"
		}
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
